package grpctunnel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"go.uber.org/zap"
)

// agentCall tracks one in-flight call received over a tunnel stream.
type agentCall struct {
	id string
	// bodyW receives request CallData payloads; the backend reads the
	// other end of the pipe.
	bodyW *io.PipeWriter
	// cancel aborts the call's context (backend request included).
	cancel context.CancelFunc
}

// callTable tracks the in-flight calls on one stream. The server sends at
// most one call at a time per stream, but the table keeps the demux honest
// (and cheap) if that ever changes.
type callTable struct {
	mu    sync.Mutex
	calls map[string]*agentCall
}

func newCallTable() *callTable {
	return &callTable{calls: make(map[string]*agentCall)}
}

func (ct *callTable) add(c *agentCall) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if _, exists := ct.calls[c.id]; exists {
		return false
	}
	ct.calls[c.id] = c
	return true
}

func (ct *callTable) get(id string) *agentCall {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.calls[id]
}

func (ct *callTable) remove(id string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	delete(ct.calls, id)
}

// active reports whether any call is in flight. Used to relax
// heartbeat-timeout enforcement while the recv loop may be blocked
// delivering body bytes to a slow upstream.
func (ct *callTable) active() bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return len(ct.calls) > 0
}

// cancelAll aborts every in-flight call (stream died).
func (ct *callTable) cancelAll() {
	ct.mu.Lock()
	calls := make([]*agentCall, 0, len(ct.calls))
	for _, c := range ct.calls {
		calls = append(calls, c)
	}
	ct.calls = make(map[string]*agentCall)
	ct.mu.Unlock()
	for _, c := range calls {
		c.bodyW.CloseWithError(fmt.Errorf("tunnel stream closed"))
		c.cancel()
	}
}

// handleCallFrame demultiplexes one CallFrame received from the server.
func (tc *tunnelClient) handleCallFrame(sc *streamCtx, table *callTable, frame *pb.CallFrame) {
	switch body := frame.Body.(type) {
	case *pb.CallFrame_Start:
		tc.startCall(sc, table, frame.CallId, body.Start)

	case *pb.CallFrame_Data:
		c := table.get(frame.CallId)
		if c == nil {
			// Normal after cancellation: the server keeps sending until it
			// processes our CallCancel.
			tc.logger.Debug("Data for unknown call, dropping", zap.String("callId", frame.CallId))
			return
		}
		// Blocks when the upstream reads slowly; that's per-call
		// backpressure. One call per stream, and heartbeat enforcement is
		// relaxed while a call is active, so this cannot kill the stream.
		if _, err := c.bodyW.Write(body.Data.Payload); err != nil {
			tc.logger.Debug("Request body write failed; call is dead",
				zap.String("callId", frame.CallId), zap.Error(err))
		}

	case *pb.CallFrame_End:
		c := table.get(frame.CallId)
		if c == nil {
			tc.logger.Debug("End for unknown call, dropping", zap.String("callId", frame.CallId))
			return
		}
		c.bodyW.Close()

	case *pb.CallFrame_Cancel:
		c := table.get(frame.CallId)
		if c == nil {
			return
		}
		tc.logger.Debug("Call cancelled by server",
			zap.String("callId", frame.CallId),
			zap.String("reason", body.Cancel.Reason))
		table.remove(frame.CallId)
		c.bodyW.CloseWithError(fmt.Errorf("cancelled by server: %s", body.Cancel.Reason))
		c.cancel()
	}
}

// startCall admits a new call and runs it on its own goroutine.
func (tc *tunnelClient) startCall(sc *streamCtx, table *callTable, callID string, start *pb.CallStart) {
	if tc.router == nil || tc.backend == nil {
		tc.sendCancel(sc.sendFn, callID, http.StatusServiceUnavailable, "agent not ready")
		return
	}

	// Always cap the call: the server's TimeoutMs is honored when smaller,
	// but MaxRequestTimeout prevents a buggy/missing server timeout from
	// leaking goroutines indefinitely against slow downstreams.
	maxTimeout := tc.config.MaxRequestTimeout
	if maxTimeout <= 0 {
		maxTimeout = 5 * time.Minute
	}
	timeout := maxTimeout
	if start.TimeoutMs > 0 && time.Duration(start.TimeoutMs)*time.Millisecond < maxTimeout {
		timeout = time.Duration(start.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	bodyR, bodyW := io.Pipe()
	c := &agentCall{id: callID, bodyW: bodyW, cancel: cancel}
	if !table.add(c) {
		cancel()
		bodyW.CloseWithError(fmt.Errorf("duplicate call id"))
		tc.logger.Warn("Duplicate call_id from server; cancelling stream", zap.String("callId", callID))
		tc.sendCancel(sc.sendFn, callID, 0, "protocol error: duplicate call_id")
		sc.ts.cancel()
		return
	}

	// Mark the stream busy for the call's duration and re-size: one more
	// stream is occupied, so one more is needed to hold the idle reserve.
	// Doing it here rather than on a timer is what lets a burst be met by
	// opening streams instead of by queueing.
	sc.ts.inflight.Add(1)
	sc.ts.lastCallAt.Store(time.Now().UnixNano())
	tc.busySlots.Add(1)
	tc.resize()

	go func() {
		defer func() {
			table.remove(callID)
			cancel()
			tc.busySlots.Add(-1)
			sc.ts.inflight.Add(-1)
			sc.ts.lastCallAt.Store(time.Now().UnixNano())
			// One fewer call in flight, so one fewer stream is needed;
			// retireExcess gives the surplus back on the next tick.
			tc.resize()
		}()

		// Acquire the in-flight semaphore, queueing (bounded by the call's
		// deadline) rather than failing fast: brief bursts over the cap
		// become latency instead of 503s. Queueing here — on the call's own
		// goroutine, with the call already admitted to the table — keeps
		// the stream's recv loop free to deliver body frames (which the
		// pipe backpressures) and heartbeats while we wait. Memory stays
		// bounded: queued bodies push back through h2 flow control rather
		// than accumulating.
		if tc.inflightSem != nil {
			select {
			case tc.inflightSem <- struct{}{}:
				tc.requestsInflight.Inc()
				defer func() {
					<-tc.inflightSem
					tc.requestsInflight.Dec()
				}()
			case <-ctx.Done():
				tc.requestsRejected.Inc()
				tc.requestsTotal.WithLabelValues(start.PseudoHeaders[":method"], "503").Inc()
				bodyR.CloseWithError(ctx.Err())
				tc.sendCancel(sc.sendFn, callID, http.StatusServiceUnavailable,
					"timed out waiting for in-flight capacity")
				return
			}
		}

		tc.runCall(ctx, sc, callID, start, bodyR)
	}()
}

// runCall routes and executes one call, streaming the response back.
func (tc *tunnelClient) runCall(ctx context.Context, sc *streamCtx, callID string, start *pb.CallStart, bodyR *io.PipeReader) {
	method := start.PseudoHeaders[":method"]
	startTime := time.Now()

	breq, err := tc.router.Route(start)
	if err != nil {
		bodyR.CloseWithError(err)
		code, reason := cancelCodeFor(err)
		tc.requestsTotal.WithLabelValues(method, fmt.Sprintf("%d", code)).Inc()
		tc.logger.Warn("Call routing failed",
			zap.String("callId", callID),
			zap.String("method", method),
			zap.String("path", start.PseudoHeaders[":path"]),
			zap.Error(err),
		)
		tc.sendCancel(sc.sendFn, callID, code, reason)
		return
	}
	breq.Body = bodyR

	resp, err := tc.backend.Do(ctx, breq)
	if err != nil {
		bodyR.CloseWithError(err)
		tc.requestsTotal.WithLabelValues(method, "502").Inc()
		tc.logger.Error("Call execution failed",
			zap.String("callId", callID),
			zap.String("method", method),
			zap.String("targetURL", breq.URL.String()),
			zap.Error(err),
		)
		tc.sendCancel(sc.sendFn, callID, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	tc.requestsTotal.WithLabelValues(method, fmt.Sprintf("%d", resp.StatusCode)).Inc()

	// One line per proxied request, at INFO: this is the record of what the
	// agent actually did on someone's behalf, and it is the first thing
	// anyone asks for when a call is slow or comes back wrong. Deferred so
	// it is still written when the response body or a send fails partway
	// through, with the latency that had elapsed by then.
	defer func() {
		tc.logger.Info("Request completed",
			zap.String("callId", callID),
			zap.String("method", method),
			zap.String("targetURL", breq.URL.String()),
			zap.Int("status", resp.StatusCode),
			zap.Int64("durationMs", time.Since(startTime).Milliseconds()),
		)
	}()

	// Response start.
	headers := make(map[string]string, len(resp.Header)+1)
	for k, v := range resp.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}

	// Identify which agent instance served this call. Under snyk-broker the
	// reflector stamps this on the way out; the tunnel routes upstream
	// traffic directly and never touches the reflector, so it has to add the
	// header itself. Without it the two transports differ in exactly the
	// place you would notice last: debugging which agent in a pool answered.
	if tc.config.InstanceId != "" {
		headers["x-axon-relay-instance"] = tc.config.InstanceId
	}
	if err := sc.sendFn(&pb.ClientFrame{Msg: &pb.ClientFrame_Call{Call: &pb.CallFrame{
		CallId: callID,
		Body: &pb.CallFrame_Start{Start: &pb.CallStart{
			PseudoHeaders: map[string]string{":status": fmt.Sprintf("%d", resp.StatusCode)},
			Headers:       headers,
		}},
	}}}); err != nil {
		return // sendFn already cancelled the stream
	}

	// Stream the response body as data frames.
	maxFrame := int(sc.maxFrameBytes)
	if maxFrame <= 0 {
		maxFrame = maxChunkSize
	}
	buf := make([]byte, maxFrame)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			payload := append([]byte(nil), buf[:n]...)
			if err := sc.sendFn(&pb.ClientFrame{Msg: &pb.ClientFrame_Call{Call: &pb.CallFrame{
				CallId: callID,
				Body:   &pb.CallFrame_Data{Data: &pb.CallData{Payload: payload}},
			}}}); err != nil {
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tc.logger.Warn("Response body read failed mid-stream",
				zap.String("callId", callID), zap.Error(readErr))
			tc.sendCancel(sc.sendFn, callID, 0, fmt.Sprintf("upstream read failed: %v", readErr))
			return
		}
	}

	// Trailers (populated only after body EOF) and end-of-call.
	var trailers map[string]string
	if tr := resp.Trailer(); len(tr) > 0 {
		trailers = make(map[string]string, len(tr))
		for k, v := range tr {
			trailers[strings.ToLower(k)] = strings.Join(v, ", ")
		}
	}
	if err := sc.sendFn(&pb.ClientFrame{Msg: &pb.ClientFrame_Call{Call: &pb.CallFrame{
		CallId: callID,
		Body:   &pb.CallFrame_End{End: &pb.CallEnd{Trailers: trailers}},
	}}}); err != nil {
		return
	}

	tc.requestDuration.WithLabelValues(method).Observe(float64(time.Since(startTime).Milliseconds()))
}

// sendCancel sends a CallCancel with an HTTP status hint.
func (tc *tunnelClient) sendCancel(sendFn sendFunc, callID string, code int32, reason string) {
	_ = sendFn(&pb.ClientFrame{Msg: &pb.ClientFrame_Call{Call: &pb.CallFrame{
		CallId: callID,
		Body:   &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{Code: code, Reason: reason}},
	}}})
}

// cancelCodeFor maps a routing error to a CallCancel code and reason.
func cancelCodeFor(err error) (int32, string) {
	if re, ok := err.(*RouteError); ok {
		return re.Code, re.Reason
	}
	return http.StatusBadGateway, err.Error()
}

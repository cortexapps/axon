// Package dispatch implements the transport-agnostic call dispatcher.
//
// Adapters (HTTP today, gRPC in the future) translate their caller's
// protocol into a Request, call Dispatcher.Dispatch, and stream the
// returned Response back to their caller. The dispatcher owns tunnel
// stream acquisition, call_id assignment, frame pumping in both
// directions, and cancellation propagation. It does not know or care what
// protocol the caller spoke. See docs/design/grpc-tunnel-v2.md §6.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ErrNoTunnel is returned when the token has no connected tunnel streams.
var ErrNoTunnel = errors.New("no tunnel available")

// ErrStreamClosed is delivered to calls in flight on a stream that closed.
var ErrStreamClosed = errors.New("tunnel stream closed")

var (
	// errTunnelSend wraps a failed Send on the call's stream.
	errTunnelSend = errors.New("tunnel send failed")
	// errRequestBody wraps a failed read of the caller's request body.
	errRequestBody = errors.New("request body read failed")
	// errCallAborted covers a call that ended before its response started
	// without any more specific error, so Dispatch never returns (nil, nil).
	errCallAborted = errors.New("call ended before response started")
)

// FailureKind classifies why a dispatch failed, for logs and callers.
type FailureKind string

const (
	// FailureNoTunnel: this server holds no stream for the token.
	FailureNoTunnel FailureKind = "no_tunnel"
	// FailureAcquireTimeout: every stream stayed busy until the deadline.
	FailureAcquireTimeout FailureKind = "acquire_timeout"
	// FailureSendFailed: sending a frame on the acquired stream failed.
	FailureSendFailed FailureKind = "send_failed"
	// FailureStreamClosed: the stream closed before the response started.
	FailureStreamClosed FailureKind = "stream_closed"
	// FailureAgentCancel: the agent cancelled the call (see CancelError).
	FailureAgentCancel FailureKind = "agent_cancel"
	// FailureResponseTimeout: no response started before the deadline.
	FailureResponseTimeout FailureKind = "response_timeout"
	// FailureCallerCanceled: the caller went away.
	FailureCallerCanceled FailureKind = "caller_canceled"
	// FailureRequestBody: reading the caller's request body failed.
	FailureRequestBody FailureKind = "request_body_failed"
	// FailureAborted: the call ended before the response with no other cause.
	FailureAborted FailureKind = "aborted"
)

// DispatchError is returned by Dispatch for every failure. It wraps the
// underlying error, so errors.Is(err, ErrNoTunnel), errors.As(err,
// *CancelError) and errors.Is(err, context.DeadlineExceeded) still work, and
// adds what a log line needs to name the call and the agent that had it.
type DispatchError struct {
	Kind FailureKind
	// CallID and StreamID are empty when no stream was acquired.
	CallID   string
	StreamID string
	// Agent is the identity of the agent that owns the stream, when one was
	// acquired.
	Agent tunnel.ClientIdentity
	// AcquireWait is how long Dispatch waited for an idle stream.
	AcquireWait time.Duration
	Err         error
}

func (e *DispatchError) Error() string {
	return fmt.Sprintf("dispatch %s: %v", e.Kind, e.Err)
}

func (e *DispatchError) Unwrap() error { return e.Err }

// kindForContext maps a context error to the failure it means at the point
// Dispatch was waiting.
func kindForContext(err error, waiting FailureKind) FailureKind {
	if errors.Is(err, context.Canceled) {
		return FailureCallerCanceled
	}
	return waiting
}

// kindForPreStart classifies an error delivered before the response started.
func kindForPreStart(err error) FailureKind {
	var cancelErr *CancelError
	switch {
	case errors.As(err, &cancelErr):
		return FailureAgentCancel
	case errors.Is(err, ErrStreamClosed):
		return FailureStreamClosed
	case errors.Is(err, errTunnelSend):
		return FailureSendFailed
	case errors.Is(err, errRequestBody):
		return FailureRequestBody
	default:
		return FailureAborted
	}
}

// acquireRetryInterval is how often Dispatch re-polls for an idle stream
// when all of a token's streams are busy. Bounded by the caller's deadline.
const acquireRetryInterval = 10 * time.Millisecond

// CancelError is returned when the agent cancels a call before starting its
// response. Code carries the agent's HTTP status hint (404 no matching rule,
// 502 upstream unreachable, 503 at in-flight cap); 0 means unspecified.
type CancelError struct {
	Reason string
	Code   int32
}

func (e *CancelError) Error() string {
	return fmt.Sprintf("call cancelled by agent (code=%d): %s", e.Code, e.Reason)
}

// Request is a transport-agnostic call to dispatch through a tunnel.
type Request struct {
	// PseudoHeaders are HTTP/2-shaped pseudo-headers (":method", ":path", …).
	PseudoHeaders map[string]string
	// Headers are regular headers, lowercased keys.
	Headers map[string]string
	// Body is the request body; it is streamed to the agent as bytes become
	// available. May be nil for bodyless requests.
	Body io.Reader
	// Kind hints the call shape to the agent.
	Kind pb.CallStart_Kind
	// TimeoutMs is the call budget sent to the agent. 0 means the agent
	// applies its own ceiling.
	TimeoutMs int32
	// RoutingHint is passed through opaquely to the agent-side dispatcher.
	RoutingHint []byte
}

// Response is the agent's reply, streamed as it arrives.
type Response struct {
	// CallID, StreamID and Agent identify the call and the agent serving it,
	// for logging failures after the response has started.
	CallID   string
	StreamID string
	Agent    tunnel.ClientIdentity
	// PseudoHeaders contains at least ":status".
	PseudoHeaders map[string]string
	// Headers are the response headers.
	Headers map[string]string
	// Body streams response bytes as CallData frames arrive. The caller
	// MUST Close it (closing after a partial read cancels the call toward
	// the agent).
	Body io.ReadCloser
	// TrailersC delivers trailers exactly once when the response ends
	// cleanly, then is closed.
	TrailersC <-chan map[string]string
	// ErrC delivers an error exactly once if the call aborts after the
	// response started, then is closed.
	ErrC <-chan error
}

// Dispatcher routes calls to tunnel streams and pumps frames.
type Dispatcher struct {
	cfg      config.Config
	registry *tunnel.ClientRegistry
	metrics  *metrics.Metrics
	logger   *zap.Logger

	mu       sync.Mutex
	calls    map[string]*call            // call_id → call
	byStream map[string]map[string]*call // stream_id → call_id → call

	// Cumulative backpressure accounting, alongside the tagged metrics, so
	// /healthz can report it without a metrics scrape. acquireWaits counts
	// dispatches that found every stream busy; acquireWaitMs is the total
	// time they spent waiting.
	acquireWaits  atomic.Int64
	acquireWaitMs atomic.Int64
}

// AcquireStats returns how many dispatches had to wait for an idle stream
// and the total milliseconds spent waiting. Rising values mean the agent is
// at the concurrency it can serve and the wait is being passed back to
// callers as latency.
func (d *Dispatcher) AcquireStats() (waits int64, totalMs int64) {
	return d.acquireWaits.Load(), d.acquireWaitMs.Load()
}

// NewDispatcher creates a new Dispatcher.
func NewDispatcher(
	cfg config.Config,
	registry *tunnel.ClientRegistry,
	m *metrics.Metrics,
	logger *zap.Logger,
) *Dispatcher {
	return &Dispatcher{
		cfg:      cfg,
		registry: registry,
		metrics:  m,
		logger:   logger.Named("dispatch"),
		calls:    make(map[string]*call),
		byStream: make(map[string]map[string]*call),
	}
}

// call tracks one in-flight logical call.
type call struct {
	id     string
	stream *tunnel.StreamHandle
	logger *zap.Logger

	// respStartC delivers the agent's response CallStart (cap 1).
	respStartC chan *pb.CallStart
	// bodyR/bodyW pipe response CallData payloads to the adapter.
	bodyR *io.PipeReader
	bodyW *io.PipeWriter
	// trailersC delivers response trailers (cap 1), closed on finish.
	trailersC chan map[string]string
	// errC delivers an abort error (cap 1), closed on finish.
	errC chan error

	// pumpCtx stops the request-body pump when the call dies early.
	pumpCtx    context.Context
	pumpCancel context.CancelFunc

	mu         sync.Mutex
	started    bool // response CallStart received
	sendDone   bool // we sent our terminal frame (End or Cancel)
	recvDone   bool // agent sent its terminal frame (End or Cancel)
	cancelSent bool // we sent CallCancel
	finished   bool // cleanup ran
	// chansClosed guards trailersC/errC. Delivery and close both take mu
	// and consult it, so a terminal frame arriving as the call finishes
	// cannot send on a closed channel.
	chansClosed bool
}

// deliverTrailers hands the response trailers to the consumer, if the call
// is still live. Non-blocking: the channel has room for the one value it
// ever carries, and a consumer that walked away must not wedge the reader.
func (c *call) deliverTrailers(trailers map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chansClosed {
		return
	}
	select {
	case c.trailersC <- trailers:
	default:
	}
}

// deliverError hands an abort error to the consumer under the same guard.
func (c *call) deliverError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chansClosed {
		return
	}
	select {
	case c.errC <- err:
	default:
	}
}

// closeChans closes both delivery channels exactly once. Callers waiting on
// them read the close as "nothing more is coming".
func (c *call) closeChans() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chansClosed {
		return
	}
	c.chansClosed = true
	close(c.trailersC)
	close(c.errC)
}

// Dispatch sends a request through an idle tunnel stream for the token and
// waits for the response to start. The returned Response streams the body as
// it arrives. ctx bounds stream acquisition and time-to-response-start; once
// the response has started, lifecycle is owned by Response.Body (Close to
// abort).
//
// Every error it returns is a *DispatchError.
func (d *Dispatcher) Dispatch(ctx context.Context, token broker.Token, req *Request) (*Response, error) {
	stream, waited, err := d.acquireStream(ctx, token)
	if err != nil {
		kind := FailureNoTunnel
		if !errors.Is(err, ErrNoTunnel) {
			kind = kindForContext(err, FailureAcquireTimeout)
		}
		return nil, &DispatchError{Kind: kind, AcquireWait: waited, Err: err}
	}

	callID := uuid.New().String()
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	bodyR, bodyW := io.Pipe()
	c := &call{
		id:         callID,
		stream:     stream,
		logger:     d.logger.With(zap.String("callId", callID), zap.String("streamId", stream.StreamID)),
		respStartC: make(chan *pb.CallStart, 1),
		bodyR:      bodyR,
		bodyW:      bodyW,
		trailersC:  make(chan map[string]string, 1),
		errC:       make(chan error, 1),
		pumpCtx:    pumpCtx,
		pumpCancel: pumpCancel,
	}

	d.register(c)

	fail := func(kind FailureKind, err error) error {
		return &DispatchError{
			Kind:        kind,
			CallID:      callID,
			StreamID:    stream.StreamID,
			Agent:       stream.Identity,
			AcquireWait: waited,
			Err:         err,
		}
	}

	// Send CallStart.
	start := &pb.CallStart{
		PseudoHeaders: req.PseudoHeaders,
		Headers:       req.Headers,
		TimeoutMs:     req.TimeoutMs,
		RoutingHint:   req.RoutingHint,
		Kind:          req.Kind,
	}
	if err := stream.Send(callFrame(callID, &pb.CallFrame_Start{Start: start})); err != nil {
		d.finish(c, false)
		return nil, fail(FailureSendFailed, fmt.Errorf("%w: %w", errTunnelSend, err))
	}

	// Pump the request body in the background.
	go d.pumpRequestBody(c, req.Body)

	// Wait for the response to start.
	select {
	case respStart := <-c.respStartC:
		return &Response{
			CallID:        callID,
			StreamID:      stream.StreamID,
			Agent:         stream.Identity,
			PseudoHeaders: respStart.PseudoHeaders,
			Headers:       respStart.Headers,
			Body:          &callBody{c: c, d: d},
			TrailersC:     c.trailersC,
			ErrC:          c.errC,
		}, nil

	case err := <-c.errC:
		// Agent cancelled, the stream died, or the request body failed
		// before the response started. errC is closed when the call
		// finishes, so a receive can also yield nil; that must still be an
		// error, or the caller gets neither a response nor a reason.
		d.cancelCall(c, "aborted before response", false)
		if err == nil {
			err = errCallAborted
		}
		return nil, fail(kindForPreStart(err), err)

	case <-ctx.Done():
		d.cancelCall(c, "caller timeout", true)
		return nil, fail(kindForContext(ctx.Err(), FailureResponseTimeout), ctx.Err())
	}
}

// acquireStream finds an idle stream for the token, waiting (bounded by ctx)
// when all streams are busy. Each stream carries one call at a time, so
// briefly waiting is the queue — and waiting here is the whole backpressure
// mechanism: an agent at capacity stops offering idle streams, and that
// shows up as latency to whoever made the request rather than as work
// piling up invisibly inside the agent. The wait is measured so the
// difference is visible, and returned so a failure can report it.
func (d *Dispatcher) acquireStream(ctx context.Context, token broker.Token) (*tunnel.StreamHandle, time.Duration, error) {
	start := time.Now()
	waited := false
	for {
		handle, allBusy := d.registry.AcquireIdleStream(token)
		if handle != nil {
			if !waited {
				return handle, 0, nil
			}
			wait := time.Since(start)
			waitMs := wait.Milliseconds()
			d.acquireWaitMs.Add(waitMs)
			d.withIdentity(token, func(tenant, integration, alias string) {
				d.metrics.DispatchAcquireWait(tenant, integration, alias, float64(waitMs))
			})
			return handle, wait, nil
		}
		if !allBusy {
			return nil, time.Since(start), ErrNoTunnel
		}
		if !waited {
			waited = true
			d.acquireWaits.Add(1)
			d.withIdentity(token, func(tenant, integration, alias string) {
				d.metrics.DispatchAllBusy(tenant, integration, alias)
			})
		}
		select {
		case <-ctx.Done():
			return nil, time.Since(start), ctx.Err()
		case <-time.After(acquireRetryInterval):
		}
	}
}

// withIdentity looks up a token's client identity for metric tagging. It is
// only called off the fast path (when a dispatch actually had to wait), so
// the registry lookup never costs an uncontended dispatch anything.
func (d *Dispatcher) withIdentity(token broker.Token, fn func(tenant, integration, alias string)) {
	if id := d.registry.GetIdentity(token); id != nil {
		fn(id.TenantID, id.Integration, id.Alias)
		return
	}
	fn("", "", "")
}

// pumpRequestBody reads the request body and sends it as CallData frames,
// then CallEnd. On any failure it cancels the call toward the agent.
func (d *Dispatcher) pumpRequestBody(c *call, body io.Reader) {
	maxFrame := d.cfg.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = config.DefaultMaxFrameBytes
	}

	if body != nil {
		buf := make([]byte, maxFrame)
		for {
			if c.pumpCtx.Err() != nil {
				return // call died; no terminal frame needed from us
			}
			n, readErr := body.Read(buf)
			if n > 0 {
				d.metrics.DispatchBytesSent.Inc(int64(n))
				// Copy: Send implementations may retain the payload past
				// return (and the fake in tests does), while buf is reused.
				payload := append([]byte(nil), buf[:n]...)
				frame := callFrame(c.id, &pb.CallFrame_Data{Data: &pb.CallData{Payload: payload}})
				if err := c.stream.Send(frame); err != nil {
					d.abortCall(c, fmt.Errorf("%w: %w", errTunnelSend, err), false)
					return
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				d.abortCall(c, fmt.Errorf("%w: %w", errRequestBody, readErr), true)
				return
			}
		}
	}

	if c.pumpCtx.Err() != nil {
		return
	}
	if err := c.stream.Send(callFrame(c.id, &pb.CallFrame_End{End: &pb.CallEnd{}})); err != nil {
		d.abortCall(c, fmt.Errorf("%w: %w", errTunnelSend, err), false)
		return
	}

	c.mu.Lock()
	c.sendDone = true
	c.mu.Unlock()
	d.maybeFinish(c)
}

// HandleFrame processes an incoming CallFrame from the tunnel service.
// This is the FrameHandler callback set on the tunnel service.
func (d *Dispatcher) HandleFrame(streamID string, frame *pb.CallFrame) {
	d.mu.Lock()
	c := d.calls[frame.CallId]
	d.mu.Unlock()

	if c == nil {
		// Normal after cancellation: the agent keeps sending until it
		// processes our CallCancel.
		d.logger.Debug("Frame for unknown call, dropping",
			zap.String("callId", frame.CallId),
			zap.String("streamId", streamID))
		return
	}

	switch body := frame.Body.(type) {
	case *pb.CallFrame_Start:
		c.mu.Lock()
		dup := c.started
		c.started = true
		c.mu.Unlock()
		if dup {
			c.logger.Warn("Duplicate response CallStart, ignoring")
			return
		}
		c.respStartC <- body.Start

	case *pb.CallFrame_Data:
		payload := body.Data.Payload
		d.metrics.DispatchBytesRecv.Inc(int64(len(payload)))
		// Blocks when the adapter's consumer is slow; that's per-call
		// backpressure. One call per stream, and heartbeat enforcement is
		// relaxed while busy, so this cannot kill the stream.
		if _, err := c.bodyW.Write(payload); err != nil {
			// Adapter closed the response body — caller went away.
			d.cancelCall(c, "response consumer closed", true)
		}

	case *pb.CallFrame_End:
		c.mu.Lock()
		preStart := !c.started
		c.recvDone = true
		c.mu.Unlock()
		if preStart {
			c.logger.Warn("CallEnd before response CallStart; treating as abort")
			d.deliverErr(c, &CancelError{Reason: "agent ended call before response start"})
			d.cancelCall(c, "protocol error: End before Start", false)
			return
		}
		c.deliverTrailers(body.End.Trailers)
		c.bodyW.Close()
		c.stream.LastSuccessAt.Store(time.Now().UnixNano())
		d.maybeFinish(c)

	case *pb.CallFrame_Cancel:
		reason := body.Cancel.Reason
		code := body.Cancel.Code
		c.logger.Debug("Call cancelled by agent",
			zap.String("reason", reason), zap.Int32("code", code))
		c.mu.Lock()
		c.recvDone = true
		c.mu.Unlock()
		err := &CancelError{Reason: reason, Code: code}
		d.deliverErr(c, err)
		c.bodyW.CloseWithError(err)
		// The agent is done; stop pumping request body and finish.
		d.cancelCall(c, "cancelled by agent", false)
	}
}

// HandleStreamClose fails all in-flight calls on a closed stream.
// This is the StreamCloseHandler callback set on the tunnel service.
func (d *Dispatcher) HandleStreamClose(streamID string) {
	d.mu.Lock()
	calls := make([]*call, 0, len(d.byStream[streamID]))
	for _, c := range d.byStream[streamID] {
		calls = append(calls, c)
	}
	d.mu.Unlock()

	for _, c := range calls {
		d.abortCall(c, ErrStreamClosed, false)
	}
}

// abortCall fails a call with err: the waiting Dispatch (or, after the
// response started, ErrC) receives it, the response body reader gets it
// rather than blocking on a pipe no one will write to again, and the call
// finishes. notifyAgent sends CallCancel, for failures the agent cannot see.
func (d *Dispatcher) abortCall(c *call, err error, notifyAgent bool) {
	d.deliverErr(c, err)
	c.bodyW.CloseWithError(err)
	d.cancelCall(c, err.Error(), notifyAgent)
}

// InflightCount returns the number of in-flight calls.
func (d *Dispatcher) InflightCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// deliverErr delivers an error to the call's errC without blocking
// (capacity 1; later errors are dropped).
func (d *Dispatcher) deliverErr(c *call, err error) {
	c.deliverError(err)
}

// cancelCall aborts a call: stops the body pump, optionally notifies the
// agent with CallCancel, and finishes the call.
func (d *Dispatcher) cancelCall(c *call, reason string, notifyAgent bool) {
	c.pumpCancel()

	c.mu.Lock()
	// Cancel is a call-level abort, not a direction terminal: it is valid
	// (and necessary) even after our request-side End — e.g. when the
	// response consumer walks away mid-stream.
	sendCancel := notifyAgent && !c.cancelSent
	if sendCancel {
		c.cancelSent = true
	}
	c.sendDone = true
	c.recvDone = true
	c.mu.Unlock()

	if sendCancel {
		frame := callFrame(c.id, &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{Reason: reason}})
		if err := c.stream.Send(frame); err != nil {
			c.logger.Debug("Failed to send CallCancel", zap.Error(err))
		}
	}

	d.maybeFinish(c)
}

// maybeFinish cleans up once both directions have terminated.
func (d *Dispatcher) maybeFinish(c *call) {
	c.mu.Lock()
	done := c.sendDone && c.recvDone && !c.finished
	if done {
		c.finished = true
	}
	c.mu.Unlock()
	if done {
		d.finish(c, true)
	}
}

// finish unregisters the call and releases its stream slot.
func (d *Dispatcher) finish(c *call, closeChans bool) {
	d.unregister(c)
	c.pumpCancel()
	c.stream.Release()
	d.metrics.DispatchInflight.Update(float64(d.InflightCount()))
	if closeChans {
		c.closeChans()
	}
}

func (d *Dispatcher) register(c *call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls[c.id] = c
	byStream := d.byStream[c.stream.StreamID]
	if byStream == nil {
		byStream = make(map[string]*call)
		d.byStream[c.stream.StreamID] = byStream
	}
	byStream[c.id] = c
	d.metrics.DispatchInflight.Update(float64(len(d.calls)))
}

func (d *Dispatcher) unregister(c *call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.calls, c.id)
	if byStream := d.byStream[c.stream.StreamID]; byStream != nil {
		delete(byStream, c.id)
		if len(byStream) == 0 {
			delete(d.byStream, c.stream.StreamID)
		}
	}
}

// callBody wraps the response pipe reader; Close before EOF cancels the
// call toward the agent so it stops producing.
type callBody struct {
	c *call
	d *Dispatcher
}

func (b *callBody) Read(p []byte) (int, error) {
	return b.c.bodyR.Read(p)
}

func (b *callBody) Close() error {
	err := b.c.bodyR.Close()
	b.c.mu.Lock()
	recvDone := b.c.recvDone
	b.c.mu.Unlock()
	if !recvDone {
		// Consumer abandoned a live response; tell the agent to stop.
		b.d.cancelCall(b.c, "response consumer closed", true)
	}
	return err
}

func callFrame(callID string, body any) *pb.ServerFrame {
	f := &pb.CallFrame{CallId: callID}
	switch b := body.(type) {
	case *pb.CallFrame_Start:
		f.Body = b
	case *pb.CallFrame_Data:
		f.Body = b
	case *pb.CallFrame_End:
		f.Body = b
	case *pb.CallFrame_Cancel:
		f.Body = b
	default:
		panic(fmt.Sprintf("unknown call frame body type %T", body))
	}
	return &pb.ServerFrame{Msg: &pb.ServerFrame_Call{Call: f}}
}

package dispatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// fakeStream records frames the dispatcher sends and lets the test play the
// agent's role.
type fakeStream struct {
	mu     sync.Mutex
	frames []*pb.CallFrame
	notify chan *pb.CallFrame
}

func newFakeStream() *fakeStream {
	return &fakeStream{notify: make(chan *pb.CallFrame, 64)}
}

func (f *fakeStream) send(msg *pb.ServerFrame) error {
	call := msg.GetCall()
	if call == nil {
		return nil // heartbeats etc.
	}
	f.mu.Lock()
	f.frames = append(f.frames, call)
	f.mu.Unlock()
	f.notify <- call
	return nil
}

// next waits for the next call frame sent by the dispatcher.
func (f *fakeStream) next(t *testing.T) *pb.CallFrame {
	t.Helper()
	select {
	case fr := <-f.notify:
		return fr
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for frame")
		return nil
	}
}

func newTestDispatcher(t *testing.T) (*Dispatcher, *tunnel.ClientRegistry, broker.Token, *fakeStream, *tunnel.StreamHandle) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	registry := tunnel.NewClientRegistry(logger)
	m := metrics.New("test-server")
	cfg := config.Config{MaxFrameBytes: 8} // tiny frames to exercise chunking
	d := NewDispatcher(cfg, registry, m, logger)

	token := broker.NewToken("test-token")
	fs := newFakeStream()
	handle := &tunnel.StreamHandle{
		StreamID: "stream-1",
		Send:     fs.send,
		Cancel:   func() {},
	}
	mustRegister(t, registry, token, tunnel.ClientIdentity{TenantID: "t1"}, handle)
	return d, registry, token, fs, handle
}

func TestDispatch_UnaryRoundTrip(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	// The adapter role: dispatch, then read the streamed body as it
	// arrives (pipe writes block until read — that's the backpressure).
	type result struct {
		resp     *Response
		body     []byte
		trailers map[string]string
		err      error
	}
	resC := make(chan result, 1)
	go func() {
		resp, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "POST", ":path": "/hello"},
			Headers:       map[string]string{"x-test": "1"},
			Body:          strings.NewReader("hello, tunnel!"), // 14 bytes → 2 frames at MaxFrameBytes=8
		})
		if err != nil {
			resC <- result{err: err}
			return
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			resC <- result{err: err}
			return
		}
		resC <- result{resp: resp, body: got, trailers: <-resp.TrailersC}
	}()

	// Agent side: expect Start, Data×2, End — in order.
	start := fs.next(t)
	require.NotNil(t, start.GetStart())
	assert.Equal(t, "POST", start.GetStart().PseudoHeaders[":method"])
	assert.Equal(t, "/hello", start.GetStart().PseudoHeaders[":path"])
	assert.Equal(t, "1", start.GetStart().Headers["x-test"])
	callID := start.CallId

	var body bytes.Buffer
	for {
		fr := fs.next(t)
		require.Equal(t, callID, fr.CallId)
		if data := fr.GetData(); data != nil {
			assert.LessOrEqual(t, len(data.Payload), 8, "frame exceeds MaxFrameBytes")
			body.Write(data.Payload)
			continue
		}
		require.NotNil(t, fr.GetEnd(), "expected End after Data")
		break
	}
	assert.Equal(t, "hello, tunnel!", body.String())

	// Respond: Start + Data + End.
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "200"},
		Headers:       map[string]string{"content-type": "text/plain"},
	}}})
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("pong")}}})
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_End{End: &pb.CallEnd{Trailers: map[string]string{"x-trailer": "yes"}}}})

	res := <-resC
	require.NoError(t, res.err)
	assert.Equal(t, "200", res.resp.PseudoHeaders[":status"])
	assert.Equal(t, "text/plain", res.resp.Headers["content-type"])
	assert.Equal(t, "pong", string(res.body))
	assert.Equal(t, "yes", res.trailers["x-trailer"])

	// Call finished → stream released and no inflight calls.
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}

func TestDispatch_StreamedResponseArrivesIncrementally(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	respC := make(chan *Response, 1)
	go func() {
		resp, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/stream"},
		})
		require.NoError(t, err)
		respC <- resp
	}()

	start := fs.next(t)
	callID := start.CallId
	fs.next(t) // request End (no body)

	d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "200"},
	}}})
	resp := <-respC

	// First chunk is readable before the response has ended — no buffering
	// to completion. Frame delivery runs on its own goroutine (as the tunnel
	// recv loop does) because the pipe write blocks until the reader keeps up.
	go d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("chunk-1")}}})
	buf := make([]byte, 16)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "chunk-1", string(buf[:n]))

	go d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("chunk-2")}}})
	n, err = resp.Body.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "chunk-2", string(buf[:n]))

	d.HandleFrame("stream-1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	_, err = resp.Body.Read(buf)
	assert.Equal(t, io.EOF, err)
	resp.Body.Close()
}

func TestDispatch_AgentCancelBeforeStartMapsToCancelError(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	errC := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/nope"},
		})
		errC <- err
	}()

	start := fs.next(t)
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{
		Reason: "no matching accept file rule",
		Code:   404,
	}}})

	err := <-errC
	var cancelErr *CancelError
	require.ErrorAs(t, err, &cancelErr)
	assert.Equal(t, int32(404), cancelErr.Code)
	assert.Contains(t, cancelErr.Reason, "no matching")
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}

func TestDispatch_NoTunnel(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := tunnel.NewClientRegistry(logger)
	d := NewDispatcher(config.Config{}, registry, metrics.New("t"), logger)

	_, err := d.Dispatch(context.Background(), broker.NewToken("unknown"), &Request{
		PseudoHeaders: map[string]string{":method": "GET", ":path": "/"},
	})
	assert.ErrorIs(t, err, ErrNoTunnel)
}

func TestDispatch_AllBusyWaitsForIdleSlot(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	// First call occupies the only stream.
	go func() {
		d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/first"},
		})
	}()
	first := fs.next(t)
	fs.next(t) // request End

	// Second call must wait for the slot.
	secondDone := make(chan *pb.CallFrame, 1)
	go func() {
		resp, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/second"},
		})
		require.NoError(t, err)
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
	}()
	go func() {
		fr := fs.next(t) // second call's Start — only after slot frees
		secondDone <- fr
	}()

	select {
	case <-secondDone:
		t.Fatal("second call dispatched while stream busy")
	case <-time.After(100 * time.Millisecond):
	}

	// Finish the first call; the second should then dispatch.
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: first.CallId, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "204"},
	}}})
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: first.CallId, Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	fr := <-secondDone
	require.NotNil(t, fr.GetStart())
	assert.Equal(t, "/second", fr.GetStart().PseudoHeaders[":path"])

	// Complete the second call so goroutines exit.
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: fr.CallId, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "204"},
	}}})
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: fr.CallId, Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
}

func TestDispatch_CallerTimeoutSendsCancel(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := d.Dispatch(ctx, token, &Request{
		PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))

	// The agent should have seen Start, End (empty body), then Cancel.
	sawCancel := false
	for range 3 {
		fr := fs.next(t)
		if fr.GetCancel() != nil {
			sawCancel = true
		}
	}
	assert.True(t, sawCancel, "agent was not told about the caller timeout")
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}

func TestDispatch_StreamCloseFailsInflightCalls(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	respErrC := make(chan error, 1)
	go func() {
		resp, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/dies"},
		})
		if err != nil {
			respErrC <- err
			return
		}
		_, readErr := io.ReadAll(resp.Body)
		respErrC <- readErr
	}()

	start := fs.next(t)
	fs.next(t) // request End

	// Response starts, then the stream dies mid-body.
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "200"},
	}}})
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("partial")}}})
	d.HandleStreamClose("stream-1")

	err := <-respErrC
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stream closed")
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}

func TestDispatch_ConsumerCloseCancelsCall(t *testing.T) {
	d, _, token, fs, _ := newTestDispatcher(t)

	respC := make(chan *Response, 1)
	go func() {
		resp, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/bigfile"},
		})
		require.NoError(t, err)
		respC <- resp
	}()

	start := fs.next(t)
	fs.next(t) // request End
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "200"},
	}}})
	resp := <-respC

	// Consumer abandons the response mid-stream.
	resp.Body.Close()

	// The dispatcher must tell the agent to stop producing.
	fr := fs.next(t)
	require.NotNil(t, fr.GetCancel(), "expected CallCancel after consumer close, got %v", fr)
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}

func mustRegister(t *testing.T, r *tunnel.ClientRegistry, token broker.Token, id tunnel.ClientIdentity, h *tunnel.StreamHandle) {
	t.Helper()
	_, err := r.Register(token, id, h)
	require.NoError(t, err)
}

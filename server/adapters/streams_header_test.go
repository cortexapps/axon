package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answer completes a call the agent was sent with a 200 and the given
// response headers.
func (fh *failureHarness) answer(callID string, headers map[string]string) {
	fh.d.HandleFrame("s1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "200"},
		Headers:       headers,
	}}})
	fh.d.HandleFrame("s1", &pb.CallFrame{CallId: callID, Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
}

// A response reports the token's load with the answering call's stream
// counted as free, because it is released when the body is copied. The
// agent cannot set x-axon-* headers for the server.
func TestStreamsHeader_OnSuccess(t *testing.T) {
	fh := newFailureHarness(t, time.Second)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- fh.serve(context.Background(), "/2.0/repositories/acme") }()
	call := fh.next(t, isStart)
	fh.answer(call.CallId, map[string]string{
		"content-type":            "application/json",
		"x-axon-streams":          "streams=99, busy=0, waiting=0",
		"X-Axon-Dispatch-Failure": "spoofed",
	})
	w := <-done

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, "streams=1, busy=0, waiting=0", w.Header().Get(StreamsHeader))
	assert.Empty(t, w.Header().Get("X-Axon-Dispatch-Failure"))
}

// A dispatch that gave up waiting reports the stream that is still held.
// It is no longer a waiter when it responds.
func TestStreamsHeader_OnFailure(t *testing.T) {
	fh := newFailureHarness(t, time.Second)

	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		fh.serve(context.Background(), "/2.0/repositories/acme")
	}()
	holder := fh.next(t, isStart)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w := fh.serve(ctx, "/__axon/info")

	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.Equal(t, "streams=1, busy=1, waiting=0", w.Header().Get(StreamsHeader))

	fh.answer(holder.CallId, nil)
	<-holderDone
}

// No stream here: the unknown-token 404 stays exactly as the dispatcher
// expects it, with no load header.
func TestStreamsHeader_NotOnNoTunnel(t *testing.T) {
	fh := newFailureHarness(t, time.Second)

	req := httptest.NewRequest(http.MethodGet, "/broker/other-token/x", nil)
	w := httptest.NewRecorder()
	fh.h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, w.Header().Get(StreamsHeader))
}

// Load counts the dispatches waiting for a stream, so Free goes negative
// when calls queue.
func TestDispatcherLoad_CountsWaiters(t *testing.T) {
	fh := newFailureHarness(t, time.Second)
	token := broker.NewToken(failureToken)

	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		fh.serve(context.Background(), "/holder")
	}()
	holder := fh.next(t, isStart)

	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		fh.serve(context.Background(), "/waiter")
	}()

	require.Eventually(t, func() bool { return fh.d.Load(token).Waiting == 1 },
		5*time.Second, 5*time.Millisecond)
	load := fh.d.Load(token)
	assert.Equal(t, 1, load.Streams)
	assert.Equal(t, 1, load.Busy)
	assert.Equal(t, -1, load.Free())

	fh.answer(holder.CallId, nil)
	<-holderDone
	waiter := fh.next(t, isStart)
	fh.answer(waiter.CallId, nil)
	<-waiterDone
	// The stream is released when both directions of the call finish, and
	// the request body pump can finish after the handler returns.
	require.Eventually(t, func() bool { return fh.d.Load(token).Free() == 1 },
		5*time.Second, 5*time.Millisecond)
}

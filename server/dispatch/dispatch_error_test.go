package dispatch

import (
	"context"
	"errors"
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

// requireDispatchError asserts err is a *DispatchError of the given kind.
func requireDispatchError(t *testing.T, err error, kind FailureKind) *DispatchError {
	t.Helper()
	var dispErr *DispatchError
	require.ErrorAs(t, err, &dispErr, "Dispatch must return *DispatchError, got %v", err)
	require.Equal(t, kind, dispErr.Kind)
	return dispErr
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// A caller hanging up mid-upload fails the request-body read before the agent
// has answered. That path used to cancel the call without delivering an
// error, so Dispatch read nil from the closed errC and returned (nil, nil),
// and the HTTP adapter then dereferenced the nil response.
func TestDispatch_RequestBodyFailureBeforeStartIsAnError(t *testing.T) {
	d, _, token, _, _ := newTestDispatcher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	readErr := errors.New("caller hung up")
	resp, err := d.Dispatch(ctx, token, &Request{
		PseudoHeaders: map[string]string{":method": "POST", ":path": "/upload"},
		Body:          failingReader{err: readErr},
	})
	require.Nil(t, resp)
	dispErr := requireDispatchError(t, err, FailureRequestBody)
	assert.ErrorIs(t, err, readErr)
	assert.NotEmpty(t, dispErr.CallID)
	assert.Equal(t, "stream-1", dispErr.StreamID)
	assert.Eventually(t, func() bool { return d.InflightCount() == 0 }, time.Second, 10*time.Millisecond)
}

// The failure has to name the agent that held the stream, not the token's
// first-registered agent: with several replicas on one token, that is the
// only way to tell which one dropped the call.
func TestDispatch_StreamClosedBeforeStartNamesTheStreamsAgent(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry := tunnel.NewClientRegistry(logger)
	d := NewDispatcher(config.Config{}, registry, metrics.New("t"), logger)
	token := broker.NewToken("replicas")

	// Agent a owns the entry; only agent b's stream is idle.
	busy := &tunnel.StreamHandle{StreamID: "a-1", Send: func(*pb.ServerFrame) error { return nil }}
	mustRegister(t, registry, token, tunnel.ClientIdentity{InstanceID: "agent-a"}, busy)
	require.True(t, busy.TryAcquire())
	fs := newFakeStream()
	mustRegister(t, registry, token, tunnel.ClientIdentity{InstanceID: "agent-b"},
		&tunnel.StreamHandle{StreamID: "b-1", Send: fs.send})

	errC := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(context.Background(), token, &Request{
			PseudoHeaders: map[string]string{":method": "GET", ":path": "/"},
		})
		errC <- err
	}()
	fs.next(t) // Start
	fs.next(t) // End
	d.HandleStreamClose("b-1")

	err := <-errC
	dispErr := requireDispatchError(t, err, FailureStreamClosed)
	assert.ErrorIs(t, err, ErrStreamClosed)
	assert.Equal(t, "b-1", dispErr.StreamID)
	assert.Equal(t, "agent-b", dispErr.Agent.InstanceID)
}

func TestDispatch_FailureKinds(t *testing.T) {
	t.Run("no tunnel", func(t *testing.T) {
		logger := zaptest.NewLogger(t)
		d := NewDispatcher(config.Config{}, tunnel.NewClientRegistry(logger), metrics.New("t"), logger)
		_, err := d.Dispatch(context.Background(), broker.NewToken("unknown"), &Request{})
		requireDispatchError(t, err, FailureNoTunnel)
		assert.ErrorIs(t, err, ErrNoTunnel)
	})

	t.Run("agent cancel", func(t *testing.T) {
		d, _, token, fs, _ := newTestDispatcher(t)
		errC := make(chan error, 1)
		go func() {
			_, err := d.Dispatch(context.Background(), token, &Request{})
			errC <- err
		}()
		start := fs.next(t)
		d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Cancel{
			Cancel: &pb.CallCancel{Reason: "upstream unreachable", Code: 502},
		}})
		err := <-errC
		requireDispatchError(t, err, FailureAgentCancel)
		var cancelErr *CancelError
		require.ErrorAs(t, err, &cancelErr)
		assert.Equal(t, int32(502), cancelErr.Code)
	})

	t.Run("response timeout", func(t *testing.T) {
		d, _, token, _, _ := newTestDispatcher(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := d.Dispatch(ctx, token, &Request{})
		requireDispatchError(t, err, FailureResponseTimeout)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("acquire timeout", func(t *testing.T) {
		d, _, token, _, handle := newTestDispatcher(t)
		require.True(t, handle.TryAcquire(), "occupy the only stream")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := d.Dispatch(ctx, token, &Request{})
		dispErr := requireDispatchError(t, err, FailureAcquireTimeout)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Positive(t, dispErr.AcquireWait)
	})

	t.Run("caller canceled", func(t *testing.T) {
		d, _, token, fs, _ := newTestDispatcher(t)
		ctx, cancel := context.WithCancel(context.Background())
		errC := make(chan error, 1)
		go func() {
			_, err := d.Dispatch(ctx, token, &Request{})
			errC <- err
		}()
		fs.next(t) // Start
		cancel()
		err := <-errC
		requireDispatchError(t, err, FailureCallerCanceled)
	})

	t.Run("send failed", func(t *testing.T) {
		logger := zaptest.NewLogger(t)
		registry := tunnel.NewClientRegistry(logger)
		d := NewDispatcher(config.Config{}, registry, metrics.New("t"), logger)
		token := broker.NewToken("broken")
		sendErr := errors.New("transport is closing")
		mustRegister(t, registry, token, tunnel.ClientIdentity{InstanceID: "agent-a"},
			&tunnel.StreamHandle{StreamID: "s", Send: func(*pb.ServerFrame) error { return sendErr }})
		_, err := d.Dispatch(context.Background(), token, &Request{})
		dispErr := requireDispatchError(t, err, FailureSendFailed)
		assert.ErrorIs(t, err, sendErr)
		assert.Equal(t, "agent-a", dispErr.Agent.InstanceID)
	})
}

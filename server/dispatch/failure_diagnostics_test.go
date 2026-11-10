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

// These tests reproduce the two failure shapes seen in prod (Nov 2026) and
// pin down what a failed dispatch now says about itself:
//
//   - a 2s /__axon/info probe that found the token's only stream on this
//     server busy with another call, and gave up waiting for it;
//   - a 60s call that got a stream, reached the agent, and was still waiting
//     on the upstream when the caller gave up.

// newHoldingDispatcher is newTestDispatcher with a grace period for the
// agent's answer to a server-side cancel, which the prod config has and the
// older tests predate.
func newHoldingDispatcher(t *testing.T, grace time.Duration) (*Dispatcher, broker.Token, *fakeStream, *tunnel.StreamHandle) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{MaxFrameBytes: 1024, AgentReportGrace: grace}
	d := NewDispatcher(cfg, registry, metrics.New("test-server"), logger)

	token := broker.NewToken("test-token")
	fs := newFakeStream()
	handle := &tunnel.StreamHandle{StreamID: "stream-1", Send: fs.send, Cancel: func() {}}
	mustRegister(t, registry, token, tunnel.ClientIdentity{InstanceID: "agent-a"}, handle)
	return d, token, fs, handle
}

// holdStream dispatches a call that the test never answers, so it keeps the
// token's stream busy, and returns its CallStart.
func holdStream(t *testing.T, d *Dispatcher, token broker.Token, fs *fakeStream, path string) *pb.CallFrame {
	t.Helper()
	go d.Dispatch(context.Background(), token, &Request{
		PseudoHeaders: map[string]string{":method": "GET", ":path": path},
	})
	start := fs.next(t)
	require.NotNil(t, start.GetStart())
	fs.next(t) // request-side End: no body
	return start
}

func asDispatchError(t *testing.T, err error) *DispatchError {
	t.Helper()
	var dispErr *DispatchError
	require.True(t, errors.As(err, &dispErr), "want *DispatchError, got %T: %v", err, err)
	return dispErr
}

// The prod 2s probe failure: the token's one stream on this server is held by
// a slow call, the probe waits for it, and the caller (the dispatcher's 2s
// client) gives up. The error must say it never got a stream, and who had it.
func TestDispatch_ProbeStarvedBySlowCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctx      func() (context.Context, context.CancelFunc)
		wantKind FailureKind
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, FailureAcquireTimeout},
		{"caller went away", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		}, FailureCallerCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, token, fs, _ := newHoldingDispatcher(t, 0)
			holder := holdStream(t, d, token, fs, "/2.0/repositories?page=2")

			ctx, cancel := tc.ctx()
			defer cancel()
			_, err := d.Dispatch(ctx, token, &Request{
				PseudoHeaders: map[string]string{":method": "GET", ":path": "/__axon/info"},
			})

			dispErr := asDispatchError(t, err)
			assert.Equal(t, tc.wantKind, dispErr.Kind)
			assert.Equal(t, PhaseAcquire, dispErr.Phase, "the probe never got a stream")
			assert.Empty(t, dispErr.CallID, "nothing reached the agent")
			assert.GreaterOrEqual(t, dispErr.AcquireWait, 40*time.Millisecond)

			s := dispErr.Streams
			assert.Equal(t, 1, s.Streams)
			assert.Equal(t, 1, s.Busy)
			assert.Equal(t, 0, s.Idle())
			assert.Equal(t, 1, s.Waiters, "the probe itself was waiting")
			assert.Equal(t, []tunnel.InstanceStreams{{InstanceID: "agent-a", Streams: 1, Busy: 1}}, s.Instances)
			require.Len(t, s.Holders, 1)
			h := s.Holders[0]
			assert.Equal(t, holder.CallId, h.CallID)
			assert.Equal(t, "GET", h.Method)
			assert.Equal(t, "/2.0/repositories", h.Path, "query strings can carry credentials")
			assert.Equal(t, "agent-a", h.InstanceID)
			assert.GreaterOrEqual(t, h.Age, 40*time.Millisecond)

			rep, outcome := dispErr.AwaitAgentReport()
			assert.Nil(t, rep)
			assert.Equal(t, AgentNotAsked, outcome)
		})
	}
}

// Waiters drop back to zero once nobody is waiting, so a later snapshot does
// not report dispatches that are long gone.
func TestDispatch_WaitersAreCountedOnlyWhileWaiting(t *testing.T) {
	d, token, fs, _ := newHoldingDispatcher(t, 0)
	holdStream(t, d, token, fs, "/slow")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/x"}})
	require.Error(t, err)

	assert.Equal(t, 0, d.streamState(token).Waiters)
}

// The prod 60s failure: the call got a stream at once and went to the agent;
// nothing came back before the caller's deadline. The error must say the call
// was with the agent, for how long, and count its own stream as busy.
func TestDispatch_SlowUpstreamTimesOutAfterSend(t *testing.T) {
	d, token, _, _ := newHoldingDispatcher(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{
		PseudoHeaders: map[string]string{":method": "GET", ":path": "/api/v4/groups/1/merge_requests?per_page=100"},
	})

	dispErr := asDispatchError(t, err)
	assert.Equal(t, FailureResponseTimeout, dispErr.Kind)
	assert.Equal(t, PhaseAwaitingResponse, dispErr.Phase)
	assert.NotEmpty(t, dispErr.CallID)
	assert.Zero(t, dispErr.AcquireWait, "a stream was free")
	assert.GreaterOrEqual(t, dispErr.Elapsed, 40*time.Millisecond)
	assert.Equal(t, 1, dispErr.Streams.Busy, "the failed call was holding the stream")
	require.Len(t, dispErr.Streams.Holders, 1)
	assert.Equal(t, dispErr.CallID, dispErr.Streams.Holders[0].CallID)
	assert.Equal(t, "/api/v4/groups/1/merge_requests", dispErr.Streams.Holders[0].Path)
}

// After a server-side timeout the agent is still running the call until it
// sees the cancel. The stream stays busy until the agent answers, and its
// answer -- where the call was -- is what the failure reports.
func TestDispatch_TimeoutHoldsStreamUntilAgentReports(t *testing.T) {
	d, token, fs, handle := newHoldingDispatcher(t, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"}})
	dispErr := asDispatchError(t, err)

	start := fs.next(t)
	fs.next(t) // End
	require.NotNil(t, fs.next(t).GetCancel(), "agent must be told")

	assert.True(t, handle.Busy(), "the agent still has the call; the stream is not free")
	assert.Equal(t, 1, d.InflightCount())

	// A late CallData must not block the stream's receive loop on a pipe
	// nobody reads.
	dataDone := make(chan struct{})
	go func() {
		d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("late")}}})
		close(dataDone)
	}()
	select {
	case <-dataDone:
	case <-time.After(time.Second):
		t.Fatal("late CallData blocked the receive loop")
	}

	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{
		Code:           502,
		Reason:         "context canceled",
		Stage:          pb.CallCancel_AWAITING_UPSTREAM_HEADERS,
		Cause:          pb.CallCancel_CANCELED_BY_SERVER,
		StageElapsedMs: 29,
		TotalElapsedMs: 30,
		UpstreamHost:   "gitlab.com",
	}}})

	rep, outcome := dispErr.AwaitAgentReport()
	assert.Equal(t, AgentReported, outcome)
	require.NotNil(t, rep)
	assert.Equal(t, pb.CallCancel_AWAITING_UPSTREAM_HEADERS, rep.Stage)
	assert.Equal(t, pb.CallCancel_CANCELED_BY_SERVER, rep.Cause)
	assert.Equal(t, 29*time.Millisecond, rep.StageElapsed)
	assert.Equal(t, "gitlab.com", rep.UpstreamHost)

	assert.False(t, handle.Busy(), "released once the agent answered")
	assert.Equal(t, 0, d.InflightCount())
}

// A held call shows up in another dispatch's failure as cancelled, so a
// starved probe can tell a stream held by a live call from one held only
// while the server waits for the agent.
func TestDispatch_HeldCallIsReportedAsCancelled(t *testing.T) {
	d, token, fs, _ := newHoldingDispatcher(t, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"}})
	require.Error(t, err)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer probeCancel()
	_, err = d.Dispatch(probeCtx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/__axon/info"}})
	dispErr := asDispatchError(t, err)
	require.Len(t, dispErr.Streams.Holders, 1)
	assert.True(t, dispErr.Streams.Holders[0].Cancelled)

	start := fs.next(t)
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{}}})
}

// An agent that never answers the cancel gets the grace period and no more:
// then the stream is released and the failure says the agent was silent.
func TestDispatch_SilentAgentReleasesStreamAfterGrace(t *testing.T) {
	d, token, _, handle := newHoldingDispatcher(t, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"}})
	dispErr := asDispatchError(t, err)

	rep, outcome := dispErr.AwaitAgentReport()
	assert.Nil(t, rep)
	assert.Equal(t, AgentSilent, outcome)
	assert.False(t, handle.Busy())
	assert.Equal(t, 0, d.InflightCount())
}

// An agent that finishes the call just after the server gave up: the upstream
// answered, too late.
func TestDispatch_AgentCompletingLateIsReportedAsSuch(t *testing.T) {
	d, token, fs, handle := newHoldingDispatcher(t, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"}})
	dispErr := asDispatchError(t, err)
	start := fs.next(t)

	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Start{Start: &pb.CallStart{
		PseudoHeaders: map[string]string{":status": "200"},
	}}})
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	_, outcome := dispErr.AwaitAgentReport()
	assert.Equal(t, AgentCompletedLate, outcome)
	assert.False(t, handle.Busy())
}

// A stream that dies while its call is held cannot carry the agent's answer;
// the hold ends at once rather than at the grace deadline.
func TestDispatch_StreamCloseEndsHold(t *testing.T) {
	d, token, _, handle := newHoldingDispatcher(t, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"}})
	dispErr := asDispatchError(t, err)

	d.HandleStreamClose("stream-1")

	done := make(chan AgentOutcome, 1)
	go func() {
		_, outcome := dispErr.AwaitAgentReport()
		done <- outcome
	}()
	select {
	case outcome := <-done:
		assert.Equal(t, AgentSilent, outcome)
	case <-time.After(time.Second):
		t.Fatal("hold outlived its stream")
	}
	assert.False(t, handle.Busy())
}

// Without a grace period the old behaviour stands: the stream is released the
// moment the server gives up.
func TestDispatch_NoGraceReleasesAtOnce(t *testing.T) {
	d, token, _, handle := newHoldingDispatcher(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Dispatch(ctx, token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"}})
	dispErr := asDispatchError(t, err)

	assert.False(t, handle.Busy())
	_, outcome := dispErr.AwaitAgentReport()
	assert.Equal(t, AgentNotAsked, outcome)
}

// An agent that cancels on its own says why, and the error carries it.
func TestDispatch_AgentCancelCarriesItsReport(t *testing.T) {
	d, token, fs, _ := newHoldingDispatcher(t, time.Second)

	errC := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(context.Background(), token, &Request{PseudoHeaders: map[string]string{":method": "GET", ":path": "/x"}})
		errC <- err
	}()
	start := fs.next(t)
	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{
		Code: 503, Reason: "timed out waiting for in-flight capacity",
		Stage: pb.CallCancel_QUEUED_FOR_CAPACITY, Cause: pb.CallCancel_CAPACITY,
	}}})

	dispErr := asDispatchError(t, <-errC)
	assert.Equal(t, FailureAgentCancel, dispErr.Kind)
	rep, outcome := dispErr.AwaitAgentReport()
	assert.Equal(t, AgentReported, outcome)
	require.NotNil(t, rep)
	assert.Equal(t, pb.CallCancel_QUEUED_FOR_CAPACITY, rep.Stage)
}

// Review finding on #186: the caller's request body failing mid-read cancels
// the call toward the agent and holds it -- and then Dispatch, seeing the
// error on errC, cancelled it again without notifying the agent. That second
// cancel took "don't notify" to mean "the agent is done" and released the
// stream while the agent was still running the call, so the next dispatch
// could land on it. A held call is released only by the agent, the grace
// period, or the stream dying.
func TestDispatch_RequestBodyFailureHoldsStreamUntilAgentReports(t *testing.T) {
	d, token, fs, handle := newHoldingDispatcher(t, 5*time.Second)

	readErr := errors.New("caller hung up mid-upload")
	_, err := d.Dispatch(context.Background(), token, &Request{
		PseudoHeaders: map[string]string{":method": "POST", ":path": "/upload"},
		Body:          failingReader{err: readErr},
	})
	dispErr := asDispatchError(t, err)
	assert.Equal(t, FailureRequestBody, dispErr.Kind)

	start := fs.next(t)
	require.NotNil(t, start.GetStart())
	require.NotNil(t, fs.next(t).GetCancel(), "the agent must be told to stop")

	assert.True(t, handle.Busy(), "the agent is still running the call; its stream is not free")
	assert.Equal(t, 1, d.InflightCount())

	d.HandleFrame("stream-1", &pb.CallFrame{CallId: start.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{
		Stage: pb.CallCancel_AWAITING_UPSTREAM_HEADERS, Cause: pb.CallCancel_CANCELED_BY_SERVER,
	}}})
	_, outcome := dispErr.AwaitAgentReport()
	assert.Equal(t, AgentReported, outcome)
	assert.False(t, handle.Busy(), "released once the agent answered")
}

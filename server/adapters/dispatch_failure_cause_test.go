package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// failureHarness is an HTTP adapter over a real dispatcher and registry, one
// agent stream whose frames the test reads, and an observed logger.
type failureHarness struct {
	h      *HttpAdapter
	d      *dispatch.Dispatcher
	logs   *observer.ObservedLogs
	frames chan *pb.CallFrame
}

const failureToken = "tok"

func newFailureHarness(t *testing.T, grace time.Duration) *failureHarness {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{DispatchTimeout: 10 * time.Second, AgentReportGrace: grace}
	m := metrics.New("test")
	d := dispatch.NewDispatcher(cfg, registry, m, logger)
	fh := &failureHarness{
		h:      NewHttpAdapter(cfg, registry, d, m, logger),
		d:      d,
		logs:   logs,
		frames: make(chan *pb.CallFrame, 64),
	}
	mustRegister(t, registry, broker.NewToken(failureToken), tunnel.ClientIdentity{InstanceID: "bitbucket-relay-1", Integration: "bitbucket"},
		&tunnel.StreamHandle{StreamID: "s1", Send: func(f *pb.ServerFrame) error {
			if c := f.GetCall(); c != nil {
				fh.frames <- c
			}
			return nil
		}})
	return fh
}

// next returns the next frame of the given type the agent was sent.
func (fh *failureHarness) next(t *testing.T, want func(*pb.CallFrame) bool) *pb.CallFrame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-fh.frames:
			if want(f) {
				return f
			}
		case <-deadline:
			t.Fatal("timed out waiting for a frame")
		}
	}
}

func isStart(f *pb.CallFrame) bool  { return f.GetStart() != nil }
func isCancel(f *pb.CallFrame) bool { return f.GetCancel() != nil }

// serve runs a GET through the adapter with ctx as the caller's context.
func (fh *failureHarness) serve(ctx context.Context, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/broker/"+failureToken+path, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	fh.h.ServeHTTP(w, req)
	return w
}

// failureLog waits for the "Dispatch failed" line for path.
func (fh *failureHarness) failureLog(t *testing.T, path string) observer.LoggedEntry {
	t.Helper()
	var entry observer.LoggedEntry
	require.Eventually(t, func() bool {
		for _, e := range fh.logs.FilterMessage("Dispatch failed").All() {
			if e.ContextMap()["path"] == path {
				entry = e
				return true
			}
		}
		return false
	}, 5*time.Second, 5*time.Millisecond, "no Dispatch failed line for %s", path)
	return entry
}

// The prod 2s probe failure, end to end: a slow call holds the token's only
// stream, the dispatcher's probe gives up waiting, and both the response and
// the log say the streams were exhausted and by what.
func TestDispatchFailure_ProbeStarvedNamesTheHolder(t *testing.T) {
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
	assert.Equal(t, "streams_exhausted", w.Header().Get("X-Axon-Dispatch-Failure"))
	assert.Equal(t, "acquire", w.Header().Get("X-Axon-Dispatch-Phase"))

	entry := fh.failureLog(t, "/__axon/info")
	assert.Equal(t, zapcore.WarnLevel, entry.Level)
	f := entry.ContextMap()
	assert.Equal(t, "streams_exhausted", f["cause"])
	assert.Equal(t, "acquire", f["phase"])
	assert.EqualValues(t, 1, f["tokenStreams"])
	assert.EqualValues(t, 1, f["tokenBusy"])
	assert.EqualValues(t, 1, f["tokenWaiters"])
	assert.Equal(t, []any{"bitbucket-relay-1 busy=1/1"}, f["tokenInstances"])
	holders, _ := f["holders"].([]any)
	require.Len(t, holders, 1)
	assert.Contains(t, holders[0], "GET /2.0/repositories/acme")
	assert.Contains(t, holders[0], holder.CallId)
	assert.NotContains(t, f, "agentOutcome", "nothing reached the agent to ask")

	// Let the holder finish.
	fh.d.HandleFrame("s1", &pb.CallFrame{CallId: holder.CallId, Body: &pb.CallFrame_Start{Start: &pb.CallStart{PseudoHeaders: map[string]string{":status": "200"}}}})
	fh.d.HandleFrame("s1", &pb.CallFrame{CallId: holder.CallId, Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	<-holderDone
}

// The prod 60s failure, end to end: the call reached the agent and the caller
// gave up while the agent waited on the upstream. The response goes out at
// once; the log line waits for the agent's account and names the upstream.
func TestDispatchFailure_SlowUpstreamIsNamedByTheAgent(t *testing.T) {
	fh := newFailureHarness(t, 5*time.Second)

	// Play the agent: answer the server's cancel the way a current agent does.
	go func() {
		c := fh.next(t, isCancel)
		fh.d.HandleFrame("s1", &pb.CallFrame{CallId: c.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{
			Code: 502, Reason: "context canceled",
			Stage: pb.CallCancel_AWAITING_UPSTREAM_HEADERS, Cause: pb.CallCancel_CANCELED_BY_SERVER,
			StageElapsedMs: 48, TotalElapsedMs: 50, UpstreamHost: "gitlab.com",
		}}})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	w := fh.serve(ctx, "/api/v4/groups/12345/merge_requests")

	assert.Equal(t, 499, w.Code)
	assert.Equal(t, "awaiting_agent", w.Header().Get("X-Axon-Dispatch-Failure"),
		"the response cannot wait for the agent; the log can")
	assert.Equal(t, "awaiting_response", w.Header().Get("X-Axon-Dispatch-Phase"))

	entry := fh.failureLog(t, "/api/v4/groups/12345/merge_requests")
	assert.Equal(t, zapcore.WarnLevel, entry.Level, "the caller gave up because the upstream was slow")
	f := entry.ContextMap()
	assert.Equal(t, "upstream_slow", f["cause"])
	assert.Equal(t, "caller_canceled", f["kind"])
	assert.Equal(t, "reported", f["agentOutcome"])
	assert.Equal(t, "AWAITING_UPSTREAM_HEADERS", f["agentStage"])
	assert.Equal(t, "CANCELED_BY_SERVER", f["agentCause"])
	assert.Equal(t, 48*time.Millisecond, f["agentStageElapsed"])
	assert.Equal(t, "gitlab.com", f["upstreamHost"])
	assert.NotEmpty(t, f["callId"])
}

// An agent that never answers the cancel is itself the finding.
func TestDispatchFailure_SilentAgentIsUnresponsive(t *testing.T) {
	fh := newFailureHarness(t, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	w := fh.serve(ctx, "/slow")
	assert.Equal(t, http.StatusGatewayTimeout, w.Code)

	f := fh.failureLog(t, "/slow").ContextMap()
	assert.Equal(t, "agent_unresponsive", f["cause"])
	assert.Equal(t, "silent", f["agentOutcome"])
}

// An agent at its in-flight cap says so, and the response carries it at once:
// the agent's cancel is the dispatch error itself.
func TestDispatchFailure_AgentAtCapacity(t *testing.T) {
	fh := newFailureHarness(t, time.Second)
	go func() {
		c := fh.next(t, isStart)
		fh.d.HandleFrame("s1", &pb.CallFrame{CallId: c.CallId, Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{
			Code: 503, Reason: "timed out waiting for in-flight capacity",
			Stage: pb.CallCancel_QUEUED_FOR_CAPACITY, Cause: pb.CallCancel_CAPACITY,
		}}})
	}()

	w := fh.serve(context.Background(), "/busy")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "agent_at_capacity", w.Header().Get("X-Axon-Dispatch-Failure"))

	f := fh.failureLog(t, "/busy").ContextMap()
	assert.Equal(t, "agent_at_capacity", f["cause"])
	assert.Equal(t, "QUEUED_FOR_CAPACITY", f["agentStage"])
}

package grpctunnel

import (
	"errors"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Every CallCancel the agent sends says where the call was and why it
// stopped, so the server can tell a slow upstream from a call stuck in the
// agent. These pin that account for each way a call ends early.

// onlyCancel returns the call's CallCancel, failing if it sent anything else.
func onlyCancel(t *testing.T, fc *frameCollector, callID string) *pb.CallCancel {
	t.Helper()
	frames := fc.byCall(callID)
	require.Len(t, frames, 1, "want exactly one frame for %s", callID)
	cancel := frames[0].GetCancel()
	require.NotNil(t, cancel, "want CallCancel for %s, got %v", callID, frames[0])
	return cancel
}

// The prod 60s case from the agent's side: the request went upstream, and
// the server cancelled while it waited for headers. The agent's answer names
// the stage, the server as the cause, how long the upstream had it, and the
// host.
func TestCallReport_ServerCancelWhileAwaitingUpstream(t *testing.T) {
	tc, _ := newTestClient(t, config.AgentConfig{MaxInflightRequests: 4}, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.backend = &stubBackend{statusCode: 200, delay: 5 * time.Second, respectCtx: true}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/api/v4/groups/1/merge_requests", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	time.Sleep(60 * time.Millisecond)
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{Reason: "caller timeout"}}})
	fc.waitDone(t, 1)

	cancel := onlyCancel(t, fc, "c1")
	assert.Equal(t, pb.CallCancel_AWAITING_UPSTREAM_HEADERS, cancel.Stage)
	assert.Equal(t, pb.CallCancel_CANCELED_BY_SERVER, cancel.Cause)
	assert.Equal(t, "stub.internal", cancel.UpstreamHost)
	assert.GreaterOrEqual(t, cancel.StageElapsedMs, int64(50))
	assert.GreaterOrEqual(t, cancel.TotalElapsedMs, cancel.StageElapsedMs)
	assert.Equal(t, int32(502), cancel.Code, "the status hint is unchanged")
}

// The agent's own deadline for the call ran out while the upstream had it.
func TestCallReport_AgentDeadlineWhileAwaitingUpstream(t *testing.T) {
	tc, _ := newTestClient(t, config.AgentConfig{MaxInflightRequests: 4}, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.backend = &stubBackend{statusCode: 200, delay: 5 * time.Second, respectCtx: true}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/slow", 50))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	cancel := onlyCancel(t, fc, "c1")
	assert.Equal(t, pb.CallCancel_AWAITING_UPSTREAM_HEADERS, cancel.Stage)
	assert.Equal(t, pb.CallCancel_AGENT_TIMEOUT, cancel.Cause)
}

// The upstream failed outright.
func TestCallReport_UpstreamError(t *testing.T) {
	tc, _ := newTestClient(t, config.AgentConfig{MaxInflightRequests: 4}, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.backend = &stubBackend{err: errors.New("connection refused")}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/x", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	cancel := onlyCancel(t, fc, "c1")
	assert.Equal(t, pb.CallCancel_AWAITING_UPSTREAM_HEADERS, cancel.Stage)
	assert.Equal(t, pb.CallCancel_UPSTREAM_ERROR, cancel.Cause)
	assert.Contains(t, cancel.Reason, "connection refused")
}

// A call that queued for the agent's in-flight capacity and never got it says
// so: nothing went upstream.
func TestCallReport_QueuedForCapacity(t *testing.T) {
	cfg := config.AgentConfig{MaxInflightRequests: 1, MaxRequestTimeout: 5 * time.Second}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.backend = &stubBackend{statusCode: 200, delay: 2 * time.Second, respectCtx: true}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "r1", reqStart("GET", "/", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	time.Sleep(20 * time.Millisecond)
	tc.startCall(sc, table, "r2", reqStart("GET", "/", 100))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r2", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	cancel := onlyCancel(t, fc, "r2")
	assert.Equal(t, pb.CallCancel_QUEUED_FOR_CAPACITY, cancel.Stage)
	assert.Equal(t, pb.CallCancel_CAPACITY, cancel.Cause)
	assert.Empty(t, cancel.UpstreamHost, "never routed")
	assert.Equal(t, int32(503), cancel.Code)
}

// No route for the request.
func TestCallReport_RouteRejected(t *testing.T) {
	tc, _ := newTestClient(t, config.AgentConfig{MaxInflightRequests: 4}, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	af, err := acceptfile.NewAcceptFile([]byte(`{
		"private": [{"method": "GET", "path": "/allowed", "origin": "http://stub.internal"}]
	}`), config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}, zap.NewNop())
	require.NoError(t, err)
	tc.router, err = NewRouter(af.Wrapper().PrivateRules(), zap.NewNop())
	require.NoError(t, err)
	tc.backend = &stubBackend{statusCode: 200}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/denied", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	cancel := onlyCancel(t, fc, "c1")
	assert.Equal(t, pb.CallCancel_ROUTING, cancel.Stage)
	assert.Equal(t, pb.CallCancel_ROUTE_REJECTED, cancel.Cause)
}

package grpctunnel

import (
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agent holds enough streams for the calls in flight plus an idle
// reserve, and nothing else. These tests pin that one rule: what it opens,
// what it gives back, and what stops it.

func newSizingClient(t *testing.T) *tunnelClient {
	t.Helper()
	tc, _ := newTestClient(t, config.AgentConfig{},
		&fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	return tc
}

// seedStreams gives the client n connected streams spread over the named
// servers, round-robin, the way the balancer places them.
func seedStreams(tc *tunnelClient, n int, servers ...string) {
	if len(servers) == 0 {
		servers = []string{"srv-a"}
	}
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < n; i++ {
		ts := &tunnelStream{id: i, cancel: func() {}, serverID: servers[i%len(servers)]}
		ts.lastCallAt.Store(time.Now().UnixNano())
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = n
	tc.mu.Unlock()
	tc.refreshObservedServers()
}

func TestSizing_HoldsTheReserveWhenIdle(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 2, "srv-a")

	tc.resize()

	assert.Equal(t, int32(idleStreamsPerServer), tc.targetSlots.Load(),
		"an idle agent holds exactly the reserve")
}

func TestSizing_OpensAStreamPerCallOnTopOfTheReserve(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 2, "srv-a")

	tc.busySlots.Store(5)
	tc.resize()

	// 5 in flight + 2 reserve. The reserve stays whole under load; that is
	// what keeps the next call from waiting on a stream open.
	assert.Equal(t, int32(7), tc.targetSlots.Load())
}

func TestSizing_GivesStreamsBackAsCallsFinish(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 20, "srv-a")

	tc.busySlots.Store(18)
	tc.resize()
	require.Equal(t, int32(20), tc.targetSlots.Load())

	// The burst passes.
	tc.busySlots.Store(0)
	tc.resize()

	assert.Equal(t, int32(idleStreamsPerServer), tc.targetSlots.Load(),
		"converges straight back to the reserve, not gradually")
}

func TestSizing_NeverBelowTheReserve(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 1, "srv-a")

	tc.busySlots.Store(0)
	tc.resize()

	assert.Equal(t, int32(idleStreamsPerServer), tc.targetSlots.Load())
}

// Reaching the cap is the backpressure mechanism: the agent stops offering
// idle streams, and the server's dispatch turns that into caller latency.
func TestSizing_StopsAtTheStreamCap(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 4, "srv-a")

	tc.busySlots.Store(maxStreams * 2)
	tc.resize()

	assert.Equal(t, int32(maxStreams), tc.targetSlots.Load())
}

func TestSizing_RespectsTheServerAnnouncedCap(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 4, "srv-a")
	tc.serverMaxStreams.Store(6)

	tc.busySlots.Store(50)
	tc.resize()

	assert.Equal(t, int32(6), tc.targetSlots.Load())
	assert.Equal(t, 6, tc.streamCap())
}

// A server may announce a cap below our own idle reserve. The cap has to
// win: asking for more than it grants gets every extra stream rejected in a
// reconnect loop.
func TestSizing_ServerCapBeatsTheIdleReserve(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 6, "srv-a", "srv-b", "srv-c")
	require.Equal(t, 3*idleStreamsPerServer, tc.idleReserve())

	tc.serverMaxStreams.Store(2)
	tc.busySlots.Store(0)
	tc.resize()

	assert.Equal(t, int32(2), tc.targetSlots.Load(),
		"the reserve must not raise the target back above the announced cap")
}

func TestSizing_IgnoresAServerCapAboveOurOwn(t *testing.T) {
	tc := newSizingClient(t)
	tc.serverMaxStreams.Store(maxStreams * 10)

	assert.Equal(t, maxStreams, tc.streamCap())
}

// -----------------------------------------------------------------------------
// Idle reserve, per server
// -----------------------------------------------------------------------------

// A server dispatches only onto the streams registered with itself, so it
// makes callers wait as soon as its own share of the reserve is taken. The
// reserve therefore has to grow with the fleet, or per-server idleness thins
// toward zero as servers are added.
func TestReserve_ScalesWithTheFleet(t *testing.T) {
	tc := newSizingClient(t)

	seedStreams(tc, 4, "srv-a", "srv-b")
	assert.Equal(t, int32(2), tc.observedServers.Load())
	assert.Equal(t, 2*idleStreamsPerServer, tc.idleReserve())

	seedStreams(tc, 10, "srv-a", "srv-b", "srv-c", "srv-d", "srv-e")
	assert.Equal(t, 5*idleStreamsPerServer, tc.idleReserve(),
		"per-server idleness holds as the fleet grows")
}

func TestReserve_AssumesOneServerBeforeAnyStreamConnects(t *testing.T) {
	tc := newSizingClient(t)

	require.Equal(t, int32(0), tc.observedServers.Load())
	assert.Equal(t, idleStreamsPerServer, tc.idleReserve(),
		"startup opens the reserve rather than nothing")
}

func TestReserve_IgnoresStreamsWithNoServerYet(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 5, "")

	assert.Equal(t, int32(0), tc.observedServers.Load())
	assert.Equal(t, idleStreamsPerServer, tc.idleReserve())
}

func TestReserve_ShrinksWhenServersLeave(t *testing.T) {
	tc := newSizingClient(t)

	seedStreams(tc, 6, "srv-a", "srv-b", "srv-c")
	require.Equal(t, 3*idleStreamsPerServer, tc.idleReserve())

	// Two instances go away; the survivors are all we hold streams on.
	seedStreams(tc, 2, "srv-a")
	assert.Equal(t, idleStreamsPerServer, tc.idleReserve())
}

// -----------------------------------------------------------------------------
// Retiring surplus streams
// -----------------------------------------------------------------------------

func TestRetireExcess_CancelsSurplusIdleStreams(t *testing.T) {
	tc := newSizingClient(t)

	cancelled := make(chan int, 10)
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < 6; i++ {
		id := i
		ts := &tunnelStream{id: id, cancel: func() { cancelled <- id }}
		// Idle since before the eligibility cutoff.
		ts.lastCallAt.Store(time.Now().Add(-time.Hour).UnixNano())
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = 6
	tc.mu.Unlock()

	tc.targetSlots.Store(2)
	tc.retireExcess()

	assert.Len(t, cancelled, 4, "retires exactly the surplus")
}

func TestRetireExcess_LeavesBusyStreamsAlone(t *testing.T) {
	tc := newSizingClient(t)

	var cancels int
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < 4; i++ {
		ts := &tunnelStream{id: i, cancel: func() { cancels++ }}
		ts.lastCallAt.Store(time.Now().Add(-time.Hour).UnixNano())
		ts.inflight.Store(1) // all carrying a call
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = 4
	tc.mu.Unlock()

	tc.targetSlots.Store(1)
	tc.retireExcess()

	assert.Zero(t, cancels, "a stream carrying a call is never cancelled to shrink")
}

// Freshly idle streams are spared for a tick, which narrows the window in
// which the server dispatches onto a stream we are cancelling.
func TestRetireExcess_SparesRecentlyActiveStreams(t *testing.T) {
	tc := newSizingClient(t)

	var cancels int
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < 4; i++ {
		ts := &tunnelStream{id: i, cancel: func() { cancels++ }}
		ts.lastCallAt.Store(time.Now().UnixNano()) // just finished a call
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = 4
	tc.mu.Unlock()

	tc.targetSlots.Store(1)
	tc.retireExcess()

	assert.Zero(t, cancels)
}

func TestRetireExcess_NoopWhenAtTarget(t *testing.T) {
	tc := newSizingClient(t)

	var cancels int
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < 3; i++ {
		ts := &tunnelStream{id: i, cancel: func() { cancels++ }}
		ts.lastCallAt.Store(time.Now().Add(-time.Hour).UnixNano())
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = 3
	tc.mu.Unlock()

	tc.targetSlots.Store(3)
	tc.retireExcess()

	assert.Zero(t, cancels)
}

// -----------------------------------------------------------------------------
// Server-enforced cap
// -----------------------------------------------------------------------------

func TestHandleTokenCap_RecordsTheEnforcedCeiling(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 5, "srv-a")

	tc.busySlots.Store(20)
	tc.handleTokenCap(1)

	assert.Equal(t, int32(5), tc.serverMaxStreams.Load(),
		"remembers what the server actually granted")
	assert.Equal(t, int32(5), tc.targetSlots.Load(),
		"and stops asking for more")
}

func TestHandleTokenCap_KeepsTheLowestSeenCeiling(t *testing.T) {
	tc := newSizingClient(t)
	tc.serverMaxStreams.Store(3)
	seedStreams(tc, 9, "srv-a")

	tc.handleTokenCap(1)

	assert.Equal(t, int32(3), tc.serverMaxStreams.Load())
}

// One stream per pooled connection is an anchor and never retires. A
// connection that empties deregisters the agent from that server in the
// dispatcher's index, and its keepalive pings become pings with no data,
// which is what a load balancer answers with GOAWAY. These pin the floor
// that prevents both.

func TestAnchors_OnePerPooledConnection(t *testing.T) {
	tc := newSizingClient(t)
	tc.pool = newConnPool(4)

	assert.Equal(t, 4, tc.anchorSlots(), "one anchor per pooled connection")
}

func TestAnchors_NeverRetire(t *testing.T) {
	tc := newSizingClient(t)
	tc.pool = newConnPool(4)

	// A target well below the worker count: every worker that can retire
	// should want to.
	tc.mu.Lock()
	tc.runningWorkers = 6
	tc.mu.Unlock()
	tc.targetSlots.Store(1)

	for id := 1; id <= 4; id++ {
		assert.False(t, tc.tryRetireWorker(id), "worker %d anchors a connection", id)
	}
	assert.True(t, tc.tryRetireWorker(5), "workers past the pool size are elastic")
}

func TestAnchors_FloorTheTarget(t *testing.T) {
	tc := newSizingClient(t)
	tc.pool = newConnPool(4)
	seedStreams(tc, 2, "srv-a")

	tc.resize()

	// The reserve alone would ask for 2, leaving two connections with no
	// stream on them.
	assert.Equal(t, int32(4), tc.targetSlots.Load(),
		"the target never drops below one stream per connection")
}

func TestAnchors_YieldToTheServerAnnouncedCap(t *testing.T) {
	tc := newSizingClient(t)
	tc.pool = newConnPool(4)
	tc.serverMaxStreams.Store(2)

	assert.Equal(t, 2, tc.anchorSlots(),
		"a server that will not grant 4 streams cannot be given 4 anchors")

	seedStreams(tc, 2, "srv-a")
	tc.resize()
	assert.Equal(t, int32(2), tc.targetSlots.Load(), "the cap still wins")
}

func TestRetireExcess_SparesAnchors(t *testing.T) {
	tc := newSizingClient(t)
	tc.pool = newConnPool(2)

	cancelled := map[string]bool{}
	mk := func(name string, anchor bool) *tunnelStream {
		ts := &tunnelStream{
			streamID: name,
			anchor:   anchor,
			cancel:   func() { cancelled[name] = true },
		}
		// Idle for longer than a tick, so retirement is otherwise eligible.
		ts.lastCallAt.Store(time.Now().Add(-2 * sizeInterval).UnixNano())
		return ts
	}

	tc.mu.Lock()
	tc.streams = map[*tunnelStream]struct{}{
		mk("anchor-a", true): {},
		mk("anchor-b", true): {},
		mk("elastic", false): {},
	}
	tc.runningWorkers = 3
	tc.mu.Unlock()
	tc.targetSlots.Store(2)

	tc.retireExcess()

	assert.True(t, cancelled["elastic"], "the elastic stream retires")
	assert.False(t, cancelled["anchor-a"], "anchors are not cancellable")
	assert.False(t, cancelled["anchor-b"], "anchors are not cancellable")
}

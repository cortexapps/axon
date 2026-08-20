package grpctunnel

import (
	"sort"
	"sync"
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
// servers, round-robin, the way the balancer places them. They are seeded
// quiet — connected, but with no call inside the window — because that is what
// "the agent has n streams" means to sizing now. Use markUsed to say that some
// of them have carried traffic.
func seedStreams(tc *tunnelClient, n int, servers ...string) {
	if len(servers) == 0 {
		servers = []string{"srv-a"}
	}
	quiet := time.Now().Add(-10 * streamIdleWindow).UnixNano()
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < n; i++ {
		ts := &tunnelStream{id: i, cancel: func() {}, serverID: servers[i%len(servers)]}
		ts.lastCallAt.Store(quiet)
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = n
	tc.mu.Unlock()
	tc.refreshObservedServers()
}

// markUsed says that n of the client's streams have just carried a call, which
// is the signal sizing actually reads. It marks the most recently used streams
// first, because that is how the work lands: the server hands each call to the
// newest idle stream it has, so repeated traffic reuses the same ones instead
// of touching a different stream every time. Returns how many it could mark.
func markUsed(tc *tunnelClient, n int) int {
	now := time.Now().UnixNano()
	tc.mu.Lock()
	defer tc.mu.Unlock()

	hottest := make([]*tunnelStream, 0, len(tc.streams))
	for st := range tc.streams {
		hottest = append(hottest, st)
	}
	sort.Slice(hottest, func(i, j int) bool {
		return hottest[i].lastCallAt.Load() > hottest[j].lastCallAt.Load()
	})
	if n > len(hottest) {
		n = len(hottest)
	}
	for _, st := range hottest[:n] {
		st.lastCallAt.Store(now)
	}
	return n
}

// goQuiet backdates every stream past the window, standing in for traffic
// stopping and streamIdleWindow elapsing.
func goQuiet(tc *tunnelClient) {
	quiet := time.Now().Add(-10 * streamIdleWindow).UnixNano()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for st := range tc.streams {
		st.lastCallAt.Store(quiet)
	}
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
	seedStreams(tc, 5, "srv-a")
	require.Equal(t, 5, markUsed(tc, 5))

	tc.resize()

	// 5 in flight + 2 reserve. The reserve stays whole under load; that is
	// what keeps the next call from waiting on a stream open.
	assert.Equal(t, int32(7), tc.targetSlots.Load())
}

func TestSizing_GivesStreamsBackOnceTheBurstHasAged(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 20, "srv-a")

	require.Equal(t, 18, markUsed(tc, 18))
	tc.resize()
	require.Equal(t, int32(20), tc.targetSlots.Load())

	// The burst passes. The streams it needed are kept, because reopening one
	// costs a round trip and a dispatcher notification and the next request is
	// probably seconds away.
	tc.resize()
	assert.Equal(t, int32(20), tc.targetSlots.Load(),
		"the streams that served the burst are still inside the window")

	// Once they fall out of it, the target drops straight to the reserve.
	goQuiet(tc)
	tc.resize()
	assert.Equal(t, int32(idleStreamsPerServer), tc.targetSlots.Load())
}

// The regression this whole asymmetry exists for. Sizing on instantaneous
// busy meant one stream opened and one retired per call: in staging, 349
// opens and 316 closes in forty minutes at a few requests a minute, each open
// paying a round trip and making the server notify the dispatcher, which
// writes redis — for a stream thrown away seconds later.
func TestSizing_DoesNotOpenAndRetireAStreamPerCall(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 4, "srv-a")

	tc.resize()
	require.Equal(t, int32(idleStreamsPerServer), tc.targetSlots.Load(),
		"quiet to start with")

	// One call arrives and finishes, repeatedly, the way ordinary traffic does.
	// Each lands on the stream the last one used, so only that stream is ever
	// warm and the target must not move after the first.
	var targets []int32
	for i := 0; i < 5; i++ {
		require.Equal(t, 1, markUsed(tc, 1))
		tc.resize()
		targets = append(targets, tc.targetSlots.Load())

		// The completion resize, which used to be the one that gave a stream
		// back and made the next call reopen it.
		tc.resize()
		targets = append(targets, tc.targetSlots.Load())
	}

	// The first call raises the target once. Nothing after that moves it, so
	// the stream opened for the first call serves all five.
	for i, got := range targets {
		assert.Equal(t, int32(1+idleStreamsPerServer), got,
			"target moved at step %d (%v); every change here is a stream opened or closed", i, targets)
	}
}

func TestSizing_GrowthIsStillImmediate(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 9, "srv-a")
	tc.resize()

	// The window applies to giving streams back, never to needing them: a
	// caller must not wait on the sizing tick that follows the next one.
	require.Equal(t, 9, markUsed(tc, 9))
	tc.resize()

	assert.Equal(t, int32(9+idleStreamsPerServer), tc.targetSlots.Load())
}

func TestSizing_NeverBelowTheReserve(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 1, "srv-a")

	tc.resize()

	assert.Equal(t, int32(idleStreamsPerServer), tc.targetSlots.Load())
}

// Reaching the cap is the backpressure mechanism: the agent stops offering
// idle streams, and the server's dispatch turns that into caller latency.
func TestSizing_StopsAtTheStreamCap(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, maxStreams*2, "srv-a")
	require.Equal(t, maxStreams*2, markUsed(tc, maxStreams*2))

	tc.resize()

	assert.Equal(t, int32(maxStreams), tc.targetSlots.Load())
}

func TestSizing_RespectsTheServerAnnouncedCap(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 50, "srv-a")
	tc.serverMaxStreams.Store(6)
	require.Equal(t, 50, markUsed(tc, 50))

	tc.resize()

	assert.Equal(t, int32(6), tc.targetSlots.Load())
	assert.Equal(t, 6, tc.streamCap())
}

// The fleet may not grant as much as our own idle reserve would like. Capacity
// has to win: asking for more than the servers will grant gets every extra
// stream rejected in a reconnect loop. Three servers granting one stream each
// is a capacity of three, against a reserve that wants six.
func TestSizing_FleetCapacityBeatsTheIdleReserve(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 6, "srv-a", "srv-b", "srv-c")
	require.Equal(t, 3*idleStreamsPerServer, tc.idleReserve())

	tc.serverMaxStreams.Store(1)
	tc.resize()

	assert.Equal(t, int32(3), tc.targetSlots.Load(),
		"the reserve must not raise the target above what the fleet grants")
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

// A refusal does not revise what the server told us. The announced cap is a
// per-server limit and it cannot change under a live connection, so being
// refused while we hold fewer streams than that says nothing about our own
// usage — the likeliest cause is the previous generation's streams still
// registered on that pod, not yet reaped by heartbeat timeout. Clamping the
// ceiling to what we happen to hold used to throttle a recovering agent to a
// single stream during exactly the window it was trying to recover in.
func TestHandleTokenCap_DoesNotLowerTheAnnouncedCeiling(t *testing.T) {
	tc := newSizingClient(t)
	tc.serverMaxStreams.Store(64)
	seedStreams(tc, 3, "srv-a")

	tc.handleTokenCap(1)

	assert.Equal(t, int32(64), tc.serverMaxStreams.Load(),
		"a refusal below the announced cap must not become a new, lower ceiling")
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
		// Quiet for longer than the idle window, so retirement is otherwise
		// eligible: past a tick is only the safety threshold now, and a stream
		// used inside the window is wanted regardless.
		ts.lastCallAt.Store(time.Now().Add(-2 * streamIdleWindow).UnixNano())
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

// seedRetirable places one stream per entry in placement, all quiet for longer
// than the idle window so they are candidates, and returns the streams grouped
// by server plus a reader for which servers lost one.
func seedRetirable(tc *tunnelClient, placement []string) (map[string][]*tunnelStream, func() map[string]int) {
	cancelled := map[string]int{}
	var mu sync.Mutex
	byServer := map[string][]*tunnelStream{}

	quiet := time.Now().Add(-10 * streamIdleWindow).UnixNano()
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i, srv := range placement {
		srv := srv
		ts := &tunnelStream{id: i + 1, serverID: srv}
		ts.cancel = func() {
			mu.Lock()
			defer mu.Unlock()
			cancelled[srv]++
		}
		ts.lastCallAt.Store(quiet)
		tc.streams[ts] = struct{}{}
		byServer[srv] = append(byServer[srv], ts)
	}
	tc.runningWorkers = len(placement)
	tc.mu.Unlock()
	tc.refreshObservedServers()

	return byServer, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range cancelled {
			out[k] = v
		}
		return out
	}
}

func useStream(st *tunnelStream) { st.lastCallAt.Store(time.Now().UnixNano()) }

// The surplus is real but it belongs to a particular server. A server
// dispatches only onto the streams registered with itself, so its own recent
// traffic is what says how many it needs — and spending an idle server's
// streams to pay for a busy one's surplus strips coverage from the first while
// leaving the second exactly as fat as it was.
func TestRetire_PrunesFromTheServerHoldingTheSurplus(t *testing.T) {
	tc := newSizingClient(t)
	placement := make([]string, 0, 9)
	for i := 0; i < 8; i++ {
		placement = append(placement, "srv-a")
	}
	placement = append(placement, "srv-b")
	byServer, cancelled := seedRetirable(tc, placement)

	// srv-a has carried one call recently; srv-b has carried none.
	useStream(byServer["srv-a"][0])

	tc.resize()
	tc.retireExcess()

	got := cancelled()
	assert.Equal(t, 5, got["srv-a"],
		"srv-a sheds to its own recent use plus its share of the reserve")
	assert.Zero(t, got["srv-b"], "srv-b was not the one holding the surplus")
}

// Each server keeps its own share of the reserve, so the next call to reach it
// does not wait on a stream open — and its last stream, which is its entire
// ability to reach this agent, is never what pays for someone else's idleness.
func TestRetire_NeverTakesAServerBelowItsOwnReserve(t *testing.T) {
	tc := newSizingClient(t)
	placement := []string{"srv-a", "srv-a", "srv-a", "srv-a", "srv-a", "srv-a", "srv-b", "srv-b"}
	_, cancelled := seedRetirable(tc, placement)

	tc.resize()
	tc.retireExcess()

	got := cancelled()
	assert.Equal(t, 4, got["srv-a"], "down to the reserve, not past it")
	assert.Zero(t, got["srv-b"], "already at its reserve")
}

// Capacity is the per-server limit times the servers we are spread over, so it
// is never smaller than the number of servers: the cap can no longer force the
// agent below one stream per server, and the old conflict between coverage and
// the cap is structurally gone. Coverage is still a preference rather than a
// veto — retireExcess breaks it when the target demands — but the cap is not
// what demands it any more.
func TestRetire_CapNeverForcesCoverageLoss(t *testing.T) {
	tc := newSizingClient(t)
	_, cancelled := seedRetirable(tc, []string{"srv-a", "srv-b"})
	tc.serverMaxStreams.Store(1)

	tc.resize()
	require.Equal(t, int32(2), tc.targetSlots.Load(),
		"two servers granting one stream each is a capacity of two")
	tc.retireExcess()

	total := 0
	for _, n := range cancelled() {
		total += n
	}
	assert.Zero(t, total, "both servers keep their stream; nothing is above capacity")
}

// A stream that has not finished connecting belongs to no server's budget, so
// it protects nothing and is the cheapest thing to give up when the cap forces
// a choice.
func TestRetire_SpendsUnconnectedStreamsFirst(t *testing.T) {
	tc := newSizingClient(t)
	_, cancelled := seedRetirable(tc, []string{"srv-a", ""})
	tc.serverMaxStreams.Store(1)

	tc.resize()
	tc.retireExcess()

	got := cancelled()
	assert.Zero(t, got["srv-a"], "srv-a keeps the stream that reaches it")
	assert.Equal(t, 1, got[""], "the unconnected stream goes instead")
}

// The server's per-token cap is fixed for the life of a server process: it is
// read from config at startup, SetMaxStreamsPerToken is called once, and the
// ServerHello reads it straight from that config. Every pod in the fleet gets
// the same value, so the cap is knowledge about the fleet rather than about one
// connection — and a reconnect is exactly the wrong moment to forget it.
//
// Start used to zero it. With no announced cap, streamCap falls back to
// maxStreams (256), so every reconnect reopened a window where the agent
// believed its ceiling was four times the real one. A reconnect stampede fills
// that window with concurrent stream opens before the first ServerHello is
// read, sails past 64, and takes a burst of ResourceExhausted rejections —
// which is how the agent came to "discover" a cap it had already been told.
func TestResetForStart_KeepsTheAnnouncedCap(t *testing.T) {
	tc := newSizingClient(t)
	tc.serverMaxStreams.Store(64)
	require.Equal(t, 64, tc.streamCap())

	tc.resetForStart()

	require.Equal(t, 64, tc.streamCap(),
		"the announced cap must survive a reconnect, or the agent rediscovers it by being refused")
}

// A cap the agent has never been told is still unknown; the local ceiling
// applies until some ServerHello says otherwise.
func TestResetForStart_UnknownCapStaysUnknown(t *testing.T) {
	tc := newSizingClient(t)
	tc.resetForStart()
	require.Equal(t, maxStreams, tc.streamCap())
}

// Everything that describes the connection we just lost must still be cleared —
// keeping the cap is a deliberate exception, not a licence to inherit state.
func TestResetForStart_ClearsPerConnectionState(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 4, "srv-a", "srv-b")
	tc.busySlots.Store(3)
	tc.observedServers.Store(2)

	tc.resetForStart()

	tc.mu.Lock()
	streams := len(tc.streams)
	workers := tc.runningWorkers
	tc.mu.Unlock()
	assert.Equal(t, 0, streams, "streams from the previous connection must be dropped")
	assert.Equal(t, 0, workers)
	assert.Equal(t, int32(0), tc.busySlots.Load())
	assert.Equal(t, int32(0), tc.observedServers.Load())
}

// -----------------------------------------------------------------------------
// Stream cap: our own policy vs the servers' defensive limit
// -----------------------------------------------------------------------------

// maxStreams and the announced cap are different units. maxStreams is this
// agent's policy — how many calls it will serve at once, anywhere, since a
// stream carries exactly one call. The announced cap is how many streams one
// server instance tolerates from one token, a defensive limit so a single agent
// cannot stampede one pod. Taking the lower of the two compared those units
// directly and let one pod's guard become the agent's fleet-wide policy: the
// agent ran at 64 where its policy is 256. Capacity is the per-server limit
// times the servers we are actually spread over.
func TestStreamCap_ScalesWithTheServersWeAreSpreadOver(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 3, "srv-a", "srv-b", "srv-c")
	tc.serverMaxStreams.Store(64)

	assert.Equal(t, 192, tc.streamCap(), "three servers granting 64 each is 192")
}

// With a single server, capacity is just its cap — the case the old clamp was
// always really defending.
func TestStreamCap_OneServerIsTheAnnouncedCap(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 5, "srv-a")
	tc.serverMaxStreams.Store(64)

	assert.Equal(t, 64, tc.streamCap())
}

// Before any stream has finished handshaking we do not know how wide the fleet
// is, so assume the narrowest one that can be true. That keeps the reconnect
// window conservative rather than optimistic.
func TestStreamCap_UnknownServerCountAssumesOneServer(t *testing.T) {
	tc := newSizingClient(t)
	tc.serverMaxStreams.Store(64)

	assert.Equal(t, 64, tc.streamCap())
}

// Fleet capacity is an upper bound on what the servers will grant, never a
// licence to exceed our own concurrency policy.
func TestStreamCap_NeverExceedsOurOwnPolicy(t *testing.T) {
	tc := newSizingClient(t)
	seedStreams(tc, 8, "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8")
	tc.serverMaxStreams.Store(64)

	assert.Equal(t, maxStreams, tc.streamCap(), "8 x 64 exceeds our own ceiling")
}

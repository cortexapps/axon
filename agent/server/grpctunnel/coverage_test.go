package grpctunnel

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These replay the production failure: an agent on four tunnel servers ended up
// with a single stream on one of them, so one in-flight call made that server
// "all busy" and a 2s health probe there timed out -- with the agent holding
// streams to spare elsewhere. Placement is the load balancer's, and its
// rate-based mode prefers the nearest zone over spreading, so the tests run
// against uneven placement, not round-robin.

// coverageSim drives the real sizing tick against simulated stream workers
// and tunnel servers. Workers behave as manageStream does: a stream cancelled on
// purpose is reopened unless tryRetireWorker says the worker should go, and
// every open is placed by place, standing in for the balancer. Time moves by
// backdating, which is all the sizing rule reads.
type coverageSim struct {
	t       *testing.T
	tc      *tunnelClient
	place   func() string
	opened  int
	starved int
	// deferOpens makes streams a call's admission asks for arrive on the
	// next tick rather than at once, as a real open's round trip does.
	deferOpens bool

	cancelled []*tunnelStream
}

func newCoverageSim(t *testing.T, conns int, place func() string) *coverageSim {
	t.Helper()
	tc := newSizingClient(t)
	tc.pool = newConnPool(conns)
	tc.streams = map[*tunnelStream]struct{}{}
	tc.cov.reset()
	return &coverageSim{t: t, tc: tc, place: place}
}

// openLocked opens a stream for worker id where the balancer puts it. Callers
// hold tc.mu.
func (s *coverageSim) openLocked(id int) {
	var ts *tunnelStream
	ts = newTunnelStream(id, &pb.ServerHello{ServerId: s.place()}, nil,
		func() { s.cancelled = append(s.cancelled, ts) }, id <= s.tc.anchorSlots())
	s.tc.streams[ts] = struct{}{}
	s.opened++
}

// openToTarget spawns workers up to the target, each opening a stream.
func (s *coverageSim) openToTarget() {
	tc := s.tc
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for tc.runningWorkers < int(tc.targetSlots.Load()) {
		tc.runningWorkers++
		tc.slotSeq++
		s.openLocked(tc.slotSeq)
	}
}

// reap plays the workers whose streams were cancelled: each either retires or
// reopens, exactly as manageStream decides.
func (s *coverageSim) reap() {
	for len(s.cancelled) > 0 {
		ts := s.cancelled[0]
		s.cancelled = s.cancelled[1:]
		s.tc.mu.Lock()
		delete(s.tc.streams, ts)
		s.tc.mu.Unlock()
		if s.tc.tryRetireWorker(ts.id) {
			continue
		}
		s.tc.mu.Lock()
		s.openLocked(ts.id)
		s.tc.mu.Unlock()
	}
}

// tick is one second of the agent: a sizing pass, the workers' response, and
// the clock moving on.
func (s *coverageSim) tick() {
	s.tc.sizeTick()
	s.reap()
	s.openToTarget()
	s.elapse(sizeInterval)
}

func (s *coverageSim) run(d time.Duration) {
	for i := 0; i < int(d/sizeInterval); i++ {
		s.tick()
	}
}

// elapse moves the clock by backdating every timestamp sizing and coverage
// read.
func (s *coverageSim) elapse(d time.Duration) {
	tc := s.tc
	tc.mu.Lock()
	for st := range tc.streams {
		if last := st.lastCallAt.Load(); last != 0 {
			st.lastCallAt.Store(last - int64(d))
		}
		st.openedAt.Add(-int64(d))
	}
	tc.mu.Unlock()
	tc.peak.mu.Lock()
	for i := range tc.peak.samples {
		tc.peak.samples[i].at -= int64(d)
	}
	tc.peak.mu.Unlock()
	c := &tc.cov
	c.mu.Lock()
	for _, sc := range c.servers {
		if sc.shortSince != 0 {
			sc.shortSince -= int64(d)
		}
		if sc.evacuatedAt != 0 {
			sc.evacuatedAt -= int64(d)
		}
	}
	c.windowStart -= int64(d)
	c.lastEvacuation -= int64(d)
	c.mu.Unlock()
}

// evacuated is the servers coverage has given up and not yet forgiven.
func (s *coverageSim) evacuated() []string {
	c := &s.tc.cov
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for id, sc := range c.servers {
		if sc.evacuatedAt != 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// begin dispatches a call to server the way a tunnel server does: onto the most
// recently active idle stream the server holds for this agent. It returns nil,
// and counts the call starved, when the server holds streams but none is idle --
// the caller waits, which is the failure.
func (s *coverageSim) begin(server string) *tunnelStream {
	tc := s.tc
	tc.mu.Lock()
	var pick *tunnelStream
	held := false
	for st := range tc.streams {
		if st.serverID != server || st.retiring.Load() {
			continue
		}
		held = true
		if st.inflight.Load() == 0 && (pick == nil || st.quietSince() > pick.quietSince()) {
			pick = st
		}
	}
	tc.mu.Unlock()
	if pick == nil {
		if held {
			s.starved++
		}
		return nil
	}
	tc.callStarted(pick)
	if !s.deferOpens {
		s.openToTarget()
	}
	return pick
}

func (s *coverageSim) end(st *tunnelStream) {
	s.tc.callFinished(st)
}

// servers is every server holding at least one of the agent's live streams, with
// how many and how many idle.
func (s *coverageSim) servers() map[string][2]int {
	s.tc.mu.Lock()
	defer s.tc.mu.Unlock()
	out := map[string][2]int{}
	for st := range s.tc.streams {
		if st.retiring.Load() {
			continue
		}
		c := out[st.serverID]
		c[0]++
		if st.inflight.Load() == 0 {
			c[1]++
		}
		out[st.serverID] = c
	}
	return out
}

// Placement models.

var fourServers = []string{"srv-a", "srv-b", "srv-c", "srv-d"}

// uniform places each stream on a server at random.
func uniform(seed uint64, servers ...string) func() string {
	r := rand.New(rand.NewPCG(seed, 1))
	return func() string { return servers[r.IntN(len(servers))] }
}

// zoneSkewed sends most streams to the first server, the nearest zone under the
// balancer's rate-based mode, and spills the rest across the others.
func zoneSkewed(seed uint64, servers ...string) func() string {
	r := rand.New(rand.NewPCG(seed, 2))
	return func() string {
		if r.Float64() < 0.7 {
			return servers[0]
		}
		return servers[1+r.IntN(len(servers)-1)]
	}
}

// scripted places the first opens as listed, then always on fallback: a server
// that got a stream once and is never chosen again.
func scripted(fallback string, first ...string) func() string {
	i := 0
	return func() string {
		if i < len(first) {
			i++
			return first[i-1]
		}
		return fallback
	}
}

type placementCase struct {
	name  string
	place func() string
}

func placements() []placementCase {
	var cases []placementCase
	for seed := uint64(1); seed <= 25; seed++ {
		cases = append(cases,
			placementCase{fmt.Sprintf("uniform/%d", seed), uniform(seed, fourServers...)},
			placementCase{fmt.Sprintf("skewed/%d", seed), zoneSkewed(seed, fourServers...)},
		)
	}
	return cases
}

// The incident's precondition, removed: once settled, every server the agent is
// on holds its idle reserve, so no server is one call away from all busy.
func TestCoverage_EveryServerHoldsItsReserve(t *testing.T) {
	for _, pc := range placements() {
		t.Run(pc.name, func(t *testing.T) {
			s := newCoverageSim(t, 4, pc.place)
			s.run(2 * time.Minute)

			servers := s.servers()
			require.NotEmpty(t, servers)
			for server, c := range servers {
				assert.GreaterOrEqual(t, c[1], idleStreamsPerServer,
					"%s holds %d streams, %d idle: one call from all busy", server, c[0], c[1])
			}
		})
	}
}

// The incident itself: a call lands on a server, and the health probe that
// follows it to the same server must find a stream free.
func TestCoverage_ProbeBehindACallFindsAStream(t *testing.T) {
	for _, pc := range placements() {
		t.Run(pc.name, func(t *testing.T) {
			s := newCoverageSim(t, 4, pc.place)
			s.run(2 * time.Minute)

			for server := range s.servers() {
				call := s.begin(server)
				require.NotNil(t, call, "%s starved its first call", server)
				probe := s.begin(server)
				assert.NotNil(t, probe, "probe on %s waited behind one call", server)
				if probe != nil {
					s.end(probe)
				}
				s.end(call)
			}
			assert.Zero(t, s.starved)
		})
	}
}

// Slow calls piling onto one server take its reserve; the agent must top that server
// back up, or give it up, while the calls are still running -- not leave it at
// zero idle for as long as they last.
func TestCoverage_SlowCallsOnOneServerAreCoveredWhileTheyRun(t *testing.T) {
	for _, pc := range placements() {
		t.Run(pc.name, func(t *testing.T) {
			s := newCoverageSim(t, 4, pc.place)
			s.run(2 * time.Minute)

			var server string
			for p := range s.servers() {
				if server == "" || p < server {
					server = p
				}
			}
			var calls []*tunnelStream
			for i := 0; i < 2; i++ {
				c := s.begin(server)
				require.NotNil(t, c)
				calls = append(calls, c)
			}
			s.run(time.Minute) // the calls are still in flight

			c := s.servers()[server]
			evacuated := slices.Contains(s.evacuated(), server)
			assert.True(t, c[1] >= idleStreamsPerServer || evacuated,
				"%s has %d of %d streams idle with slow calls running, and was not given up", server, c[1], c[0])
			for _, c := range calls {
				s.end(c)
			}
		})
	}
}

// A server the balancer will not place on again cannot be filled. The agent
// gives it up -- its streams move to servers it can cover -- and the attempts
// stay bounded: no open-and-retire loop against a balancer that keeps
// missing.
func TestCoverage_UnfillableServerIsGivenUpWithBoundedChurn(t *testing.T) {
	// The first stream lands on srv-b; every one after it on srv-a.
	s := newCoverageSim(t, 4, scripted("srv-a", "srv-b"))
	s.run(2 * time.Minute)

	servers := s.servers()
	assert.NotContains(t, servers, "srv-b", "the server that could not be covered still holds the token")
	assert.GreaterOrEqual(t, servers["srv-a"][1], idleStreamsPerServer)

	// An hour on, nothing should still be churning.
	settled := s.opened
	s.run(time.Hour)
	assert.LessOrEqual(t, s.opened-settled, 2,
		"a settled agent kept opening streams: %d in an hour", s.opened-settled)
}

// Evacuating is how the agent leaves a server it cannot cover; it must never be
// how it leaves every server. With a stream cap too low for any server to reach its
// budget, nothing is evacuated.
func TestCoverage_NeverGivesUpEveryServer(t *testing.T) {
	s := newCoverageSim(t, 2, uniform(7, "srv-a", "srv-b"))
	s.tc.serverMaxStreams.Store(1) // cap: one stream per server
	s.run(5 * time.Minute)

	assert.Empty(t, s.evacuated())
	assert.NotEmpty(t, s.servers())
}

// Servers running short together are given up one at a time, so a burst of slow
// calls everywhere does not strip the agent down to a single server at once.
func TestCoverage_GivesUpOneServerAtATime(t *testing.T) {
	// Every stream after the first three lands on srv-a: srv-b, srv-c and
	// srv-d each get one and can never be filled.
	s := newCoverageSim(t, 4, scripted("srv-a", "srv-b", "srv-c", "srv-d"))
	s.run(coverageGrace + 5*time.Second)

	assert.Len(t, s.evacuated(), 1, "one short server given up per grace period")

	s.run(3 * coverageGrace)
	assert.Equal(t, []string{"srv-a"}, keys(s.servers()), "in time, every server that cannot be covered is left")
}

func keys(m map[string][2]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Coverage must not reintroduce churn: once every server is covered, an idle
// agent opens nothing.
func TestCoverage_QuietOnceCovered(t *testing.T) {
	for _, pc := range placements() {
		t.Run(pc.name, func(t *testing.T) {
			s := newCoverageSim(t, 4, pc.place)
			s.run(2 * time.Minute)
			settled := s.opened
			s.run(30 * time.Minute)
			assert.Zero(t, s.opened-settled, "an idle, covered agent kept opening streams")
		})
	}
}

// Periodic probe traffic across every server -- the production steady state --
// must neither starve nor churn.
func TestCoverage_PeriodicProbesNeitherStarveNorChurn(t *testing.T) {
	for _, pc := range placements() {
		t.Run(pc.name, func(t *testing.T) {
			s := newCoverageSim(t, 4, pc.place)
			s.run(2 * time.Minute)
			settled := s.opened
			for i := 0; i < 12; i++ {
				for server := range s.servers() {
					if c := s.begin(server); c != nil {
						s.end(c)
					}
				}
				s.run(285 * time.Second)
			}
			assert.Zero(t, s.starved)
			assert.LessOrEqual(t, s.opened-settled, 8, "steady periodic traffic kept opening streams")
		})
	}
}

// retireExcess spends a surplus only from servers above their budget. A server at
// exactly its reserve keeps it, even with the fleet over target.
func TestRetireExcess_NeverTakesAServerBelowItsReserve(t *testing.T) {
	tc := newSizingClient(t)
	placement := append(repeatServer("srv-a", idleStreamsPerServer+1), repeatServer("srv-b", idleStreamsPerServer)...)
	_, cancelled := seedRetirable(tc, placement)

	tc.targetSlots.Store(int32(idleStreamsPerServer + 1))
	tc.retireExcess()

	assert.Equal(t, map[string]int{"srv-a": 1}, cancelled(),
		"only srv-a's stream above its reserve is surplus")
}

// -----------------------------------------------------------------------------
// Growth under load
// -----------------------------------------------------------------------------

// The dispatcher sends a token's calls to one tunnel server at a time, so
// that server's queue is the agent's whole demand. These pin how fast the
// agent grows into it, without depending on where the balancer places
// streams: the first asserts on what the agent asks for, and the rest bracket
// placement between every new stream landing on the busy server and none of
// them doing so.

// queue models callers waiting on one tunnel server: each tick, the server
// hands waiting calls to whatever idle streams it holds, and they stay in
// flight -- slow upstream calls.
type queue struct {
	s        *coverageSim
	server   string
	waiting  int
	inFlight []*tunnelStream
}

func (q *queue) dispatch() {
	for q.waiting > 0 {
		st := q.s.begin(q.server)
		if st == nil {
			return
		}
		q.waiting--
		q.inFlight = append(q.inFlight, st)
	}
}

func (q *queue) finishAll() {
	for _, st := range q.inFlight {
		q.s.end(st)
	}
	q.inFlight = nil
}

// A server with every stream busy gets a chunk of streams asked for on the
// next tick, not one: growth is greedy, and the floor says so before a single
// stream has landed.
func TestGrowth_AsksForAChunkWhenAServerIsQueueing(t *testing.T) {
	s := newCoverageSim(t, 4, scripted("srv-a"))
	s.run(time.Minute)
	s.deferOpens = true
	q := &queue{s: s, server: "srv-a", waiting: 1000}
	q.dispatch()
	busy := len(q.inFlight)
	require.Positive(t, busy)

	workers := s.tc.runningWorkers
	s.tc.sizeTick()
	asked := int(s.tc.targetSlots.Load()) - workers
	t.Logf("%d in flight, asked for %d more in one tick", busy, asked)
	assert.GreaterOrEqual(t, asked, max(growthChunk, busy),
		"one tick with %d calls in flight and callers waiting asked for %d streams", busy, asked)
	q.finishAll()
}

// Best case: every stream the agent opens lands on the busy server. A burst
// of a thousand waiting calls is fully in flight within a handful of ticks,
// because capacity doubles each tick.
func TestGrowth_DrainsABurstInAFewTicksWhenStreamsLand(t *testing.T) {
	s := newCoverageSim(t, 4, scripted("srv-a"))
	s.run(time.Minute)
	s.deferOpens = true
	q := &queue{s: s, server: "srv-a", waiting: 1000}

	ticks := 0
	for q.dispatch(); q.waiting > 0 && ticks < 60; q.dispatch() {
		s.tick()
		ticks++
	}
	t.Logf("1000-call burst in flight after %d ticks", ticks)
	assert.Zero(t, q.waiting, "callers still waiting after %d ticks", ticks)
	assert.LessOrEqual(t, ticks, 10, "a 1000-call burst took %d ticks to reach streams", ticks)
	q.finishAll()
}

// Worst case: the balancer never places on the busy server. The agent cannot
// relieve that server's queue by opening streams, so it must not grow without
// bound trying, and it must never give up the server the dispatcher is
// sending to.
func TestGrowth_IsBoundedAndKeepsTheBusyServerWhenStreamsNeverLand(t *testing.T) {
	// srv-b gets the first stream and nothing after it.
	s := newCoverageSim(t, 4, scripted("srv-a", "srv-b"))
	s.deferOpens = true
	s.tick() // the first streams open; srv-b holds one
	q := &queue{s: s, server: "srv-b", waiting: 100}
	q.dispatch()
	require.NotEmpty(t, q.inFlight, "srv-b should hold a stream to queue behind")

	s.run(10 * time.Minute)
	q.dispatch()

	servers := s.servers()
	assert.Contains(t, servers, "srv-b", "the busy server was given up")
	total := 0
	for _, c := range servers {
		total += c[0]
	}
	bound := growthSurplusFactor*max(growthChunk, len(q.inFlight)) + 4*idleStreamsPerServer + s.tc.anchorSlots() + growthChunk
	t.Logf("never-landing balancer: %d streams in total after 10m, bound %d", total, bound)
	assert.LessOrEqual(t, total, bound, "growth ran away to %d streams against a balancer that never helps", total)
	q.finishAll()
}

// Greedy on open, firm on prune: once a burst is over and its streams have
// aged out, the agent is back to the resting reserve -- what keeps shared
// tunnel servers from carrying every agent's last burst forever.
func TestGrowth_PrunesBackToRestAfterABurst(t *testing.T) {
	s := newCoverageSim(t, 4, scripted("srv-a"))
	s.run(time.Minute)
	rest := s.servers()["srv-a"][0]

	s.deferOpens = true
	q := &queue{s: s, server: "srv-a", waiting: 500}
	for q.dispatch(); q.waiting > 0; q.dispatch() {
		s.tick()
	}
	peak := s.servers()["srv-a"][0]
	require.Greater(t, peak, 500)
	t.Logf("burst peak %d streams, resting %d", peak, rest)
	q.finishAll()

	s.run(streamIdleWindow + pruneMinAge + time.Minute)
	assert.Equal(t, rest, s.servers()["srv-a"][0], "the burst's streams were not given back")
}

// Each server answers what state it is in from its own counts and history;
// these pin the rule the table in coverage.go describes.
func TestServerCoverage_State(t *testing.T) {
	now := time.Now().UnixNano()
	grace := int64(coverageGrace)
	for _, tc := range []struct {
		name string
		s    serverCoverage
		want coverageState
	}{
		{"at budget", serverCoverage{serverStreamCounts: serverStreamCounts{have: idleStreamsPerServer}}, stateCovered},
		{"above budget, busy", serverCoverage{serverStreamCounts: serverStreamCounts{have: 10, busy: 3}}, stateCovered},
		{"short with calls in flight", serverCoverage{serverStreamCounts: serverStreamCounts{have: 2, busy: 2}, shortSince: now}, stateGrowing},
		{"short with calls in flight, long ago", serverCoverage{serverStreamCounts: serverStreamCounts{have: 1, busy: 1}, shortSince: now - 10*grace}, stateGrowing},
		{"short and idle, just now", serverCoverage{serverStreamCounts: serverStreamCounts{have: 1}, shortSince: now}, stateFilling},
		{"short and idle, past grace", serverCoverage{serverStreamCounts: serverStreamCounts{have: 1}, shortSince: now - grace}, stateOverdue},
		{"evacuated", serverCoverage{serverStreamCounts: serverStreamCounts{have: 1}, evacuatedAt: now}, stateEvacuated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.s.state(now))
		})
	}
}

// growth asks for at least a chunk, at least the calls in flight, and at least
// the shortfall.
func TestServerCoverage_Growth(t *testing.T) {
	assert.Equal(t, growthChunk, (&serverCoverage{serverStreamCounts: serverStreamCounts{have: 1, busy: 1}}).growth())
	assert.Equal(t, 40, (&serverCoverage{serverStreamCounts: serverStreamCounts{have: 40, busy: 40}}).growth())
}

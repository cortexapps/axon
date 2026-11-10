package grpctunnel

import (
	"sort"
	"sync"
	"time"
)

// Per-tunnel-server coverage.
//
// Why: a tunnel server (one relay-grpc-tunnel instance) can only hand a call
// to a stream this agent holds on that same server. If the server the
// dispatcher is using has no idle stream, callers there wait -- even when the
// agent has idle streams on other servers.
//
// What it tracks. Every sizing tick (1s), the agent's streams are grouped by
// the tunnel server each one landed on (ServerHello.ServerId). Per server:
//
//	streams  open streams on that server
//	busy     of those, how many are carrying a call right now
//	budget   busy + idleStreamsPerServer (2): what the server should have
//	short    budget - streams, when positive
//
// Each server (a serverCoverage) answers what state it is in, and each state
// has one response:
//
//	State      When                                Action
//	---------  ----------------------------------  ---------------------------
//	covered    streams >= budget                   nothing to do
//	growing    short, with calls in flight         GROW
//	filling    short, nothing in flight            FILL
//	overdue    short, nothing in flight, for 30s   FILL, then EVACUATE
//	evacuated  given up in the last 10 minutes     recycle its idle streams
//
// What each action does:
//
//   - GROW opens max(growthChunk (8), the server's calls in flight, its
//     shortfall) streams that tick, so capacity roughly doubles each second
//     while callers are waiting. No budget. It pauses once streams sitting
//     idle on covered servers reach growthSurplusFactor (4) times the calls
//     in flight (or 4 x 8, if more), and is capped by maxStreams and the
//     servers' per-token cap.
//
//   - FILL opens enough streams to cover the shortfall, from a shared budget
//     of coverageActionsPerServer (4) opens per server the agent is on, per
//     10 minutes.
//
//   - EVACUATE recycles the server's idle streams so they reopen somewhere
//     else, and the server drops the token.
//
//     When: a server has had nothing in flight and been short of its
//     reserve for 30s (overdue) -- FILL has had its chance and the load
//     balancer is not placing streams there. Typically it got one stream by
//     chance, often an anchor, and rarely gets another because the balancer
//     favours a different zone.
//
//     Why: a server that holds even one of the agent's streams stays in the
//     dispatcher's list for the token, so the dispatcher's liveness probe
//     and any calls routed there land on that one stream. While it is busy
//     they wait; a 2s probe that times out records the agent as
//     unresponsive on that server. The agent cannot make that server
//     healthy -- it cannot choose where streams land -- but it can leave:
//     once the server holds none of its streams it drops the token, the
//     dispatcher stops sending there, and the recycled streams reopen
//     wherever the balancer places them, usually on servers already
//     covered. A server the agent is not on cannot make a caller wait.
//
//     Limits: never a server with calls in flight (that is where the
//     dispatcher is sending, so leaving it would strand those callers --
//     GROW handles it instead); only while another server is covered, so
//     the agent is never left with nowhere good to be; at most one server
//     per 30s; from its own shared budget of 4 recycles per server per 10
//     minutes; and not refilled for 10 minutes, so it is not filled and
//     given up in a loop.
//
// While anything is short, the target is held at the current stream count
// plus what was just opened, so nothing is pruned until the shortfall
// clears.
//
// The agent cannot choose where a new stream lands -- the load balancer does,
// and it favours the nearest zone -- so growth and fill open streams and some
// land on servers that did not need them. Pruning gives those back: when the
// agent holds more than its target, retireExcess retires streams that are
// idle, older than pruneMinAge (2m) and unused for streamIdleWindow (10m),
// keeping on each server one stream per call it carried in that window plus
// the reserve.
const (
	// coverageGrace is how long a server may stay short while fill tries to
	// reach it before it is evacuated.
	coverageGrace = 30 * time.Second
	// coverageActionsPerServer is the budget per streamIdleWindow, per server
	// the agent holds streams on, for fill opens and, separately, for
	// evacuation recycles. Growth for calls in flight is not budgeted.
	coverageActionsPerServer = 4
	// growthChunk is the fewest streams growth opens for a server queueing
	// callers in one tick.
	growthChunk = 8
	// growthSurplusFactor bounds the streams growth leaves idle on servers
	// that did not need them: it stops opening while those exceed this many
	// times the calls in flight (or growthChunk, if more). It is what keeps a
	// balancer that never places on the busy server from growing the agent to
	// its cap.
	growthSurplusFactor = 4
)

// coverage is what the tunnel client's sizing tick remembers between ticks: one
// serverCoverage per tunnel server, and the budgets. Timestamps are UnixNano.
type coverage struct {
	mu      sync.Mutex
	servers map[string]*serverCoverage
	// lastEvacuation is when a server was last given up. At most one goes per
	// coverageGrace, so servers running short together are not given up in a
	// cascade.
	lastEvacuation int64
	// windowStart, filled and recycled meter the budgets.
	windowStart int64
	filled      int
	recycled    int
	// warned is set once the fill budget runs out with a server still short,
	// so that is logged once per window.
	warned bool
}

func (c *coverage) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servers = map[string]*serverCoverage{}
	c.lastEvacuation = 0
	c.windowStart = 0
	c.filled = 0
	c.recycled = 0
	c.warned = false
}

// serverStreamCounts is one tunnel server's streams as coverage sees them.
type serverStreamCounts struct {
	have int // open and not being retired
	busy int // in flight now
	// recyclable are idle streams quiet for a full tick: the ones an
	// evacuation may take without cancelling a call the server is about to
	// dispatch onto them.
	recyclable []*tunnelStream
}

// budget is how many streams the server needs now: one for each call in
// flight on it, plus the idle reserve.
//
// Calls, not the streams that carried them recently. The server hands each
// call to the newest idle stream it holds, so a server seeing one call every
// few minutes touches a different stream each time; counting those made a
// quiet server look short, spent the fill budget on it, and then gave it up.
// retireExcess keeps one stream per recent call plus the reserve, which is
// never less than this, so what fill opens toward retirement leaves alone.
func (s *serverStreamCounts) budget() int { return s.busy + idleStreamsPerServer }

func (s *serverStreamCounts) short() int {
	if d := s.budget() - s.have; d > 0 {
		return d
	}
	return 0
}

// countServerStreams groups streams by the tunnel server each landed on.
// Streams not yet placed, and ones being retired, are left out. The caller
// holds whatever guards streams.
func countServerStreams(streams map[*tunnelStream]struct{}, now time.Time) map[string]*serverStreamCounts {
	quietBefore := now.Add(-sizeInterval).UnixNano()
	servers := make(map[string]*serverStreamCounts)
	for st := range streams {
		if st.serverID == "" || st.retiring.Load() {
			continue
		}
		s := servers[st.serverID]
		if s == nil {
			s = &serverStreamCounts{}
			servers[st.serverID] = s
		}
		s.have++
		inflight := st.inflight.Load() != 0
		if inflight {
			s.busy++
		}
		if !inflight && st.quietSince() < quietBefore {
			s.recyclable = append(s.recyclable, st)
		}
	}
	return servers
}

// serverCoverage is coverage's view of one tunnel server: its streams this
// tick, and what coverage remembers about it across ticks.
type serverCoverage struct {
	serverStreamCounts
	// shortSince is when the server fell short of its budget; zero while it
	// is not short.
	shortSince int64
	// evacuatedAt is when the server was given up; zero unless that was
	// within the last streamIdleWindow.
	evacuatedAt int64
}

// coverageState is where a tunnel server stands, and so what coverage does
// about it.
type coverageState int

const (
	// stateCovered: the server holds its budget. Nothing to do.
	stateCovered coverageState = iota
	// stateGrowing: short, with calls in flight -- callers may be waiting.
	// Grow, unbudgeted.
	stateGrowing
	// stateFilling: short, nothing in flight, for less than coverageGrace.
	// Fill, within the budget.
	stateFilling
	// stateOverdue: short, nothing in flight, for coverageGrace or longer.
	// Still filled, and evacuated as soon as evacuation is allowed.
	stateOverdue
	// stateEvacuated: given up within the last streamIdleWindow. Its idle
	// streams are recycled; it is neither grown nor filled.
	stateEvacuated
)

func (st coverageState) String() string {
	switch st {
	case stateCovered:
		return "covered"
	case stateGrowing:
		return "growing"
	case stateFilling:
		return "filling"
	case stateOverdue:
		return "overdue"
	case stateEvacuated:
		return "evacuated"
	}
	return "unknown"
}

// state is where the server stands at now.
func (s *serverCoverage) state(now int64) coverageState {
	switch {
	case s.evacuatedAt != 0:
		return stateEvacuated
	case s.short() == 0:
		return stateCovered
	case s.busy > 0:
		return stateGrowing
	case now-s.shortSince >= int64(coverageGrace):
		return stateOverdue
	default:
		return stateFilling
	}
}

// growth is how many streams a growing server asks for in one tick: at least
// growthChunk, and at least as many as it has calls in flight, so capacity
// doubles per tick under pressure.
func (s *serverCoverage) growth() int { return max(s.short(), growthChunk, s.busy) }

// tick runs one sizing tick of coverage: it folds counts -- the agent's
// streams per tunnel server, from countServerStreams -- into what it tracks,
// and plans what to do. workers is the stream workers running, pending those
// still opening a stream. The caller carries the plan out.
func (c *coverage) tick(counts map[string]*serverStreamCounts, now int64, workers, pending int) coveragePlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(counts, now)
	return c.plan(now, workers, pending)
}

// observe folds one tick's stream counts into the servers coverage tracks:
// new servers are added, ones the agent has left are dropped (unless still
// marked evacuated), evacuations expire, and each server's shortSince starts
// or clears. Callers hold c.mu.
func (c *coverage) observe(counts map[string]*serverStreamCounts, now int64) {
	if c.servers == nil {
		c.servers = map[string]*serverCoverage{}
	}
	if now-c.windowStart >= int64(streamIdleWindow) {
		c.windowStart, c.filled, c.recycled, c.warned = now, 0, 0, false
	}
	for id, s := range c.servers {
		if s.evacuatedAt != 0 && now-s.evacuatedAt >= int64(streamIdleWindow) {
			s.evacuatedAt = 0
		}
		if _, ok := counts[id]; !ok && s.evacuatedAt == 0 {
			delete(c.servers, id)
			continue
		}
		s.serverStreamCounts = serverStreamCounts{}
	}
	for id, n := range counts {
		s := c.servers[id]
		if s == nil {
			s = &serverCoverage{}
			c.servers[id] = s
		}
		s.serverStreamCounts = *n
	}
	for _, s := range c.servers {
		switch {
		case s.evacuatedAt != 0, s.short() == 0:
			s.shortSince = 0
		case s.shortSince == 0:
			s.shortSince = now
		}
	}
}

// coveragePlan is what one tick of coverage decided.
type coveragePlan struct {
	// floor is the stream count resize must not go below; zero when nothing
	// is short.
	floor int
	// grown and filled are the streams opened for growth and for the
	// reserve this tick.
	grown, filled int
	// evacuate are the servers given up this tick.
	evacuate []string
	// recycle are the idle streams on evacuated servers to reopen elsewhere.
	recycle []*tunnelStream
	// budget is this window's fill and recycle budget, each.
	budget int
	// exhausted is set the first tick in a window the fill budget runs out
	// with servers still short; short names them.
	exhausted bool
	short     []string
}

// plan asks each server its state and decides the tick: what to open, what
// to evacuate and recycle, and the floor to hold the target at. workers is
// the stream workers running, pending those still opening a stream. Callers
// hold c.mu.
func (c *coverage) plan(now int64, workers, pending int) coveragePlan {
	ids := make([]string, 0, len(c.servers))
	present := 0
	for id, s := range c.servers {
		ids = append(ids, id)
		if s.have > 0 {
			present++
		}
	}
	sort.Strings(ids)
	p := coveragePlan{budget: coverageActionsPerServer * max(present, 1)}

	growWant, reserveDeficit, covered, surplus, busyTotal := 0, 0, 0, 0, 0
	var overdue []string
	for _, id := range ids {
		s := c.servers[id]
		busyTotal += s.busy
		switch s.state(now) {
		case stateCovered:
			covered++
			surplus += s.have - s.budget()
		case stateGrowing:
			growWant += s.growth()
		case stateOverdue:
			overdue = append(overdue, id)
			fallthrough
		case stateFilling:
			reserveDeficit += s.short()
			p.short = append(p.short, id)
		}
	}

	// Evacuate an overdue server -- only while another is covered, and one
	// per coverageGrace.
	for _, id := range overdue {
		if covered == 0 || now-c.lastEvacuation < int64(coverageGrace) {
			continue
		}
		s := c.servers[id]
		c.lastEvacuation = now
		s.evacuatedAt, s.shortSince = now, 0
		reserveDeficit -= s.short()
		p.evacuate = append(p.evacuate, id)
	}

	// Recycle the idle streams of evacuated servers, as the budget allows; a
	// busy one waits for its call to finish.
	for _, id := range ids {
		s := c.servers[id]
		if s.evacuatedAt == 0 {
			continue
		}
		for _, st := range s.recyclable {
			if c.recycled >= p.budget {
				break
			}
			c.recycled++
			p.recycle = append(p.recycle, st)
		}
	}

	// Open for what is short, less what is already on its way: growth first,
	// unbudgeted but paused while streams that landed elsewhere pile up idle;
	// then the reserve, within its budget. Hold the target while they land,
	// and let it go once nothing is short.
	if surplus >= growthSurplusFactor*max(growthChunk, busyTotal) {
		growWant = 0
	}
	if growWant > 0 || reserveDeficit > 0 {
		inFlight := pending
		p.grown = max(growWant-inFlight, 0)
		inFlight = max(inFlight-growWant, 0)
		p.filled = min(max(reserveDeficit-inFlight, 0), max(p.budget-c.filled, 0))
		c.filled += p.filled
		p.floor = workers + p.grown + p.filled
	}
	if reserveDeficit > 0 && c.filled >= p.budget && !c.warned {
		c.warned = true
		p.exhausted = true
	}
	return p
}

// serverSummary is one tunnel server's streams and coverage state for
// /systemcheck, as of the last sizing tick.
type serverSummary struct {
	ServerID string `json:"server_id"`
	Streams  int    `json:"streams"`
	Busy     int    `json:"busy"`
	Budget   int    `json:"budget"`
	State    string `json:"state"`
}

// summaries reports each tunnel server's streams and state as of the last
// tick, for /systemcheck.
func (c *coverage) summaries(now int64) []serverSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]serverSummary, 0, len(c.servers))
	for id, s := range c.servers {
		out = append(out, serverSummary{
			ServerID: id, Streams: s.have, Busy: s.busy, Budget: s.budget(), State: s.state(now).String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out
}

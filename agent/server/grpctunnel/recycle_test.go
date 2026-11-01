package grpctunnel

import (
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Something between the agent and the relay ends every stream at exactly 24
// hours with RST_STREAM INTERNAL_ERROR. The agent comes back in a second, but
// it is a failure: logged at Warn, and fatal to any call on the stream at the
// time. So the agent retires its own streams first, idle and on purpose, at
// a lifetime each instance picks for itself and each stream jitters, so that
// neither one agent's streams nor a fleet's line up on the same instant.

// agedStream is a connected stream whose lifetime ran out the given time ago
// (negative: has yet to), quiet for longer than a tick.
func agedStream(tc *tunnelClient, id int, overdue time.Duration, cancelled *[]int) *tunnelStream {
	now := time.Now()
	ts := &tunnelStream{id: id, serverID: "srv-a"}
	ts.cancel = func() { *cancelled = append(*cancelled, id) }
	ts.openedAt.Store(now.Add(-time.Hour).UnixNano())
	ts.lastCallAt.Store(now.Add(-time.Minute).UnixNano())
	ts.recycleAt = now.Add(-overdue).UnixNano()
	tc.mu.Lock()
	if tc.streams == nil {
		tc.streams = map[*tunnelStream]struct{}{}
	}
	tc.streams[ts] = struct{}{}
	tc.runningWorkers++
	tc.mu.Unlock()
	return ts
}

func TestRecycle_OnlyStreamsPastTheirLifetime(t *testing.T) {
	tc := newSizingClient(t)
	var cancelled []int
	due := agedStream(tc, 1, time.Second, &cancelled)
	agedStream(tc, 2, -time.Minute, &cancelled)

	tc.recycleAged()

	assert.Equal(t, []int{1}, cancelled)
	assert.True(t, due.retiring.Load(), "a recycle is deliberate, not a failure")
	assert.True(t, due.recycling.Load())
}

// Several streams coming due together — after a relay restart, say, every
// stream opened in the same second — must not all go at once.
func TestRecycle_OnePerTickOldestFirst(t *testing.T) {
	tc := newSizingClient(t)
	var cancelled []int
	agedStream(tc, 1, time.Minute, &cancelled)
	agedStream(tc, 2, time.Hour, &cancelled)
	agedStream(tc, 3, 2*time.Minute, &cancelled)

	tc.recycleAged()
	assert.Equal(t, []int{2}, cancelled, "one per tick, the most overdue")

	tc.recycleAged()
	assert.Equal(t, []int{2}, cancelled, "nothing more while that one is still going")
}

func TestRecycle_LeavesStreamsThatAreWorking(t *testing.T) {
	tc := newSizingClient(t)
	var cancelled []int
	busy := agedStream(tc, 1, time.Hour, &cancelled)
	busy.inflight.Store(1)
	justUsed := agedStream(tc, 2, time.Hour, &cancelled)
	justUsed.lastCallAt.Store(time.Now().UnixNano())

	tc.recycleAged()

	assert.Empty(t, cancelled,
		"a busy stream or one the server may be dispatching onto right now waits for a quiet moment")
}

func TestRecycle_DisabledWithoutALifetime(t *testing.T) {
	tc := newSizingClient(t)
	var cancelled []int
	ts := agedStream(tc, 1, time.Hour, &cancelled)
	ts.recycleAt = 0

	tc.recycleAged()

	assert.Empty(t, cancelled)
}

func TestStreamLifetime_EachInstancePicksItsOwn(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		tc := newSizingClient(t)
		tc.resetForStart()
		require.GreaterOrEqual(t, tc.streamLifetime, streamLifetimeMin)
		require.Less(t, tc.streamLifetime, streamLifetimeMax)
		seen[tc.streamLifetime] = true
	}
	assert.Greater(t, len(seen), 1, "instances do not share a lifetime")
	assert.Less(t, streamLifetimeMax+streamLifetimeMax/10, 24*time.Hour,
		"well inside the 24h limit, jitter included")
}

func TestStreamLifetime_EachStreamJittersItsOwn(t *testing.T) {
	tc := newSizingClient(t)
	tc.streamLifetime = 4 * time.Hour
	opened := time.Now().UnixNano()

	seen := map[int64]bool{}
	for i := 0; i < 50; i++ {
		at := tc.recycleDeadline(opened)
		life := time.Duration(at - opened)
		require.GreaterOrEqual(t, life, 4*time.Hour-24*time.Minute)
		require.LessOrEqual(t, life, 4*time.Hour+24*time.Minute)
		seen[at] = true
	}
	assert.Greater(t, len(seen), 1)

	tc.streamLifetime = 0
	assert.Zero(t, tc.recycleDeadline(opened), "no lifetime, no recycling")
}

// End to end against a real server: a stream that reaches its lifetime is
// reopened straight away, and none of the failure machinery notices — no
// warning, no reconnect counted.
func TestRecycle_ReopensTheStreamQuietly(t *testing.T) {
	stubStreamLifetime(t, 50*time.Millisecond)

	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "srv-a"}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	tc, _ := newTestClient(t, config.AgentConfig{}, &countingRegistration{})
	core, logs := observer.New(zapcore.WarnLevel)
	tc.logger = zap.New(core)
	tc.mu.Lock()
	tc.currentToken, tc.currentServerAddr = "tok", addr
	tc.mu.Unlock()
	startWorkers(t, tc, 1)

	waitFor(t, 5*time.Second, func() bool {
		tc.recycleAged()
		return svc.streams.Load() >= 3
	})
	waitFor(t, 5*time.Second, func() bool {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		return len(tc.streams) == 1
	})

	assert.Zero(t, logs.Len(), "a recycle logs no warning: %v", logs.All())
	assert.Zero(t, counterVecValue(t, tc.reconnectsTotal, ""), "a recycle is not a reconnect")
}

// stubStreamLifetime makes every instance pick lifetime d.
func stubStreamLifetime(t *testing.T, d time.Duration) {
	t.Helper()
	old := pickStreamLifetime
	pickStreamLifetime = func() time.Duration { return d }
	t.Cleanup(func() { pickStreamLifetime = old })
}

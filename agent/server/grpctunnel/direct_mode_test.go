package grpctunnel

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func directConfig(idle, max int) config.AgentConfig {
	return config.AgentConfig{
		TunnelConnMode:    config.TunnelConnModeDirect,
		TunnelConns:       2,
		TunnelIdleStreams: idle,
		TunnelMaxStreams:  max,
	}
}

func newDirectClient(t *testing.T, idle, max int) *tunnelClient {
	t.Helper()
	tc, _ := newTestClient(t, directConfig(idle, max),
		&fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(int32(idle))
	return tc
}

func TestDirect_OpensStreamsToRestoreIdleReserve(t *testing.T) {
	tc := newDirectClient(t, 4, 64)

	// All four reserve streams took a call: idle is 0, so four more are
	// needed to restore the reserve.
	fakeStreams(tc, 4)
	tc.busySlots.Store(4)
	tc.maybeGrow()

	assert.Equal(t, int32(8), tc.targetSlots.Load())
}

func TestDirect_NoGrowthWhileReserveIntact(t *testing.T) {
	tc := newDirectClient(t, 4, 64)

	// 10 connected, 6 busy → 4 idle, exactly the reserve. Nothing to do.
	fakeStreams(tc, 10)
	tc.targetSlots.Store(10)
	tc.busySlots.Store(6)
	tc.maybeGrow()

	assert.Equal(t, int32(10), tc.targetSlots.Load())
}

// A burst arrives as many near-simultaneous calls. Each one calls maybeGrow,
// and without counting still-dialing workers each would see the same deficit
// and add its own streams, overshooting far past what the reserve needs.
func TestDirect_ConcurrentAdmissionsDoNotOvershoot(t *testing.T) {
	tc := newDirectClient(t, 4, 64)

	fakeStreams(tc, 4)
	tc.busySlots.Store(4)
	for i := 0; i < 10; i++ {
		tc.maybeGrow()
	}

	// runningWorkers stays at 4 (nothing actually dials in this test), so
	// after the first call raises the target to 8, the four pending workers
	// cover the reserve and no further growth is warranted.
	assert.Equal(t, int32(8), tc.targetSlots.Load())
}

func TestDirect_ClampedAtMaxStreams(t *testing.T) {
	tc := newDirectClient(t, 4, 6)

	fakeStreams(tc, 5)
	tc.targetSlots.Store(5)
	tc.busySlots.Store(5)
	tc.maybeGrow()

	// Wants 5+4=9, capped at TunnelMaxStreams. Beyond this the agent stops
	// offering idle streams, which is what pushes back on the server.
	assert.Equal(t, int32(6), tc.targetSlots.Load())
}

func TestDirect_ServerAnnouncedCapStillClamps(t *testing.T) {
	tc := newDirectClient(t, 4, 64)
	tc.serverMaxStreams.Store(5)

	fakeStreams(tc, 4)
	tc.busySlots.Store(4)
	tc.maybeGrow()

	assert.Equal(t, int32(5), tc.targetSlots.Load())
}

func TestDirect_ShrinksBackToReserveAsCallsFinish(t *testing.T) {
	tc := newDirectClient(t, 4, 64)

	// A burst grew the pool to 20; now every call has finished.
	fakeStreams(tc, 20)
	tc.targetSlots.Store(20)
	tc.busySlots.Store(0)

	// Each completing call gives back at most one stream, so drive it the
	// way real completions would.
	for i := 0; i < 50; i++ {
		tc.releaseIdleHeadroom()
	}

	assert.Equal(t, int32(4), tc.targetSlots.Load(), "converges to the idle reserve, not below")
}

func TestDirect_NoShrinkWhileStreamsAreBusy(t *testing.T) {
	tc := newDirectClient(t, 4, 64)

	// 20 connected, 17 busy → only 3 idle, under the reserve. Nothing should
	// be given back even though the target is well above the floor.
	fakeStreams(tc, 20)
	tc.targetSlots.Store(20)
	tc.busySlots.Store(17)
	tc.releaseIdleHeadroom()

	assert.Equal(t, int32(20), tc.targetSlots.Load())
}

// The watermark's growth rule must stay inert in direct mode: the reserve
// rule is the only thing sizing the stream count.
func TestDirect_WatermarkRuleDoesNotApply(t *testing.T) {
	tc := newDirectClient(t, 2, 64)

	// Under the watermark rule, 8 connected with 7 busy (idle 1 < watermark
	// 2) would grow by 4. The reserve rule sees idle 1 against a reserve of
	// 2 and adds exactly 1.
	fakeStreams(tc, 8)
	tc.targetSlots.Store(8)
	tc.busySlots.Store(7)
	tc.maybeGrow()

	assert.Equal(t, int32(9), tc.targetSlots.Load())
}

func TestDirect_ModeClassification(t *testing.T) {
	require.True(t, config.TunnelConnModeDirect.IsDirect())
	require.False(t, config.TunnelConnModeDirect.IsFixed(),
		"direct grows on demand, so the fixed-size short circuits must not catch it")
	require.False(t, config.TunnelConnModePool.IsDirect())
	require.False(t, config.TunnelConnModeMux.IsDirect())
}

func TestDirect_FloorIsTheIdleReserve(t *testing.T) {
	tc := newDirectClient(t, 6, 64)
	assert.Equal(t, 6, tc.minSlots())
	assert.Equal(t, 64, tc.effectiveMaxSlots())
	assert.Equal(t, 0, tc.fixedSlots(), "direct is not a fixed-size mode")
}

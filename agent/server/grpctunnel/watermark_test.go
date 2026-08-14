package grpctunnel

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStreams seeds the client with n connected pseudo-streams and matching
// worker count, without dialing anything (running stays false so
// ensureWorkers doesn't spawn goroutines — these tests assert on
// targetSlots decisions only).
func fakeStreams(tc *tunnelClient, n int) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.streams = make(map[*tunnelStream]struct{})
	for i := 0; i < n; i++ {
		ts := &tunnelStream{id: i, cancel: func() {}}
		ts.lastCallAt.Store(time.Now().UnixNano())
		tc.streams[ts] = struct{}{}
	}
	tc.runningWorkers = n
}

func TestMaybeGrow_WatermarkBreached(t *testing.T) {
	cfg := config.AgentConfig{MinTunnelSlots: 2, MaxTunnelSlots: 16}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(2)

	// 2 connected, both busy → idle 0 < watermark 1 → grow by max(1, 2/2)=1.
	fakeStreams(tc, 2)
	tc.busySlots.Store(2)
	tc.maybeGrow()
	assert.Equal(t, int32(3), tc.targetSlots.Load())
}

func TestMaybeGrow_EnoughIdleCapacity(t *testing.T) {
	cfg := config.AgentConfig{MinTunnelSlots: 2, MaxTunnelSlots: 16}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(4)

	// 4 connected, 2 busy → idle 2 >= watermark 1 → no growth.
	fakeStreams(tc, 4)
	tc.busySlots.Store(2)
	tc.maybeGrow()
	assert.Equal(t, int32(4), tc.targetSlots.Load())
}

func TestMaybeGrow_ClampedAtMax(t *testing.T) {
	cfg := config.AgentConfig{MinTunnelSlots: 2, MaxTunnelSlots: 4}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(4)

	fakeStreams(tc, 4)
	tc.busySlots.Store(4)
	tc.maybeGrow()
	assert.Equal(t, int32(4), tc.targetSlots.Load(), "must not grow past max")
}

func TestMaybeGrow_ClampedAtServerAnnouncedCap(t *testing.T) {
	cfg := config.AgentConfig{MinTunnelSlots: 2, MaxTunnelSlots: 16}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(3)
	tc.serverMaxStreams.Store(3)

	fakeStreams(tc, 3)
	tc.busySlots.Store(3)
	tc.maybeGrow()
	assert.Equal(t, int32(3), tc.targetSlots.Load(), "server cap overrides configured max")
}

func TestMaybeGrow_Cooldown(t *testing.T) {
	cfg := config.AgentConfig{MinTunnelSlots: 1, MaxTunnelSlots: 16}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(1)

	fakeStreams(tc, 1)
	tc.busySlots.Store(1)
	tc.maybeGrow()
	require.Equal(t, int32(2), tc.targetSlots.Load())

	// Still saturated, but within the cooldown window: no second step.
	fakeStreams(tc, 2)
	tc.busySlots.Store(2)
	tc.maybeGrow()
	assert.Equal(t, int32(2), tc.targetSlots.Load(), "growth must be rate-limited")
}

func TestHandleTokenCap_ClampsTarget(t *testing.T) {
	cfg := config.AgentConfig{MinTunnelSlots: 1, MaxTunnelSlots: 16}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.targetSlots.Store(8)

	fakeStreams(tc, 3)
	tc.handleTokenCap(99)
	assert.Equal(t, int32(3), tc.targetSlots.Load(), "target clamps to connected count")
	assert.Equal(t, int32(3), tc.serverMaxStreams.Load(), "remembered as effective cap")
	assert.Equal(t, 3, tc.effectiveMaxSlots())
}

// TestPoolGrowsUnderLoad drives real calls through a fake server and
// asserts the pool grows beyond min with no server-side signal.
func TestPoolGrowsUnderLoad(t *testing.T) {
	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "grow-server"}}
	// Each new stream immediately receives a slow call, keeping every slot
	// busy so the watermark stays breached.
	svc.behavior.onStream = func(stream pb.TunnelService_TunnelServer, _ *pb.ClientHello) error {
		callID := fmt.Sprintf("call-%d", time.Now().UnixNano())
		stream.Send(&pb.ServerFrame{Msg: &pb.ServerFrame_Call{Call: &pb.CallFrame{
			CallId: callID,
			Body: &pb.CallFrame_Start{Start: &pb.CallStart{
				PseudoHeaders: map[string]string{":method": "GET", ":path": "/slow"},
			}},
		}}})
		stream.Send(&pb.ServerFrame{Msg: &pb.ServerFrame_Call{Call: &pb.CallFrame{
			CallId: callID,
			Body:   &pb.CallFrame_End{End: &pb.CallEnd{}},
		}}})
		for {
			if _, err := stream.Recv(); err != nil {
				return err
			}
		}
	}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{
		MinTunnelSlots:      1,
		MaxTunnelSlots:      3,
		MaxStreamsPerServer: 8,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	tc.backend = &stubBackend{statusCode: 200, body: []byte("ok"), delay: 30 * time.Second, respectCtx: true}
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	// With every slot instantly busy, the pool must climb to max — and not
	// beyond.
	waitFor(t, 10*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "grow-server") == 3
	})
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, float64(3), gaugeVecValue(t, tc.connectionsActive, "grow-server"))
}

// TestPoolShrinksWhenIdle grows the pool, lets the load stop, and asserts
// it converges back to min (and not below).
func TestPoolShrinksWhenIdle(t *testing.T) {
	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "shrink-server"}}
	var sentCalls atomic.Int32
	// Only the first two streams receive a (fast) call; afterwards all
	// streams sit idle.
	svc.behavior.onStream = func(stream pb.TunnelService_TunnelServer, _ *pb.ClientHello) error {
		if sentCalls.Add(1) <= 2 {
			callID := fmt.Sprintf("call-%d", time.Now().UnixNano())
			stream.Send(&pb.ServerFrame{Msg: &pb.ServerFrame_Call{Call: &pb.CallFrame{
				CallId: callID,
				Body: &pb.CallFrame_Start{Start: &pb.CallStart{
					PseudoHeaders: map[string]string{":method": "GET", ":path": "/quick"},
				}},
			}}})
			stream.Send(&pb.ServerFrame{Msg: &pb.ServerFrame_Call{Call: &pb.CallFrame{
				CallId: callID,
				Body:   &pb.CallFrame_End{End: &pb.CallEnd{}},
			}}})
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return err
			}
		}
	}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{
		MinTunnelSlots:      1,
		MaxTunnelSlots:      4,
		MaxStreamsPerServer: 8,
		SlotIdleTimeout:     300 * time.Millisecond,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	tc.backend = &stubBackend{statusCode: 200, body: []byte("ok"), delay: 100 * time.Millisecond, respectCtx: true}
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	// Grow past min first.
	waitFor(t, 10*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "shrink-server") >= 2
	})

	// Then the calls complete, everything idles, and the pool converges
	// back to min.
	waitFor(t, 10*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "shrink-server") == 1
	})
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, float64(1), gaugeVecValue(t, tc.connectionsActive, "shrink-server"),
		"pool must not shrink below min")
}

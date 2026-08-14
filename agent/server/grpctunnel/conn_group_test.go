package grpctunnel

import (
	"errors"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dialStub returns a real (lazy) ClientConn without touching the network:
// grpc.NewClient does not connect until an RPC is attempted.
func dialStub(t *testing.T) func() (*grpc.ClientConn, error) {
	t.Helper()
	return func() (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///stub", grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
}

func TestConnGroupSharesOneConnPerIndex(t *testing.T) {
	g := newConnGroup()
	dial := dialStub(t)

	e1, err := g.acquire(0, dial)
	require.NoError(t, err)
	e2, err := g.acquire(0, dial)
	require.NoError(t, err)

	require.Same(t, e1, e2, "same index must hand back the same connection")
	require.Equal(t, 2, e1.refs)

	other, err := g.acquire(1, dial)
	require.NoError(t, err)
	require.NotSame(t, e1, other, "different index must get its own connection")
}

func TestConnGroupClosesOnLastRelease(t *testing.T) {
	g := newConnGroup()
	dial := dialStub(t)

	_, err := g.acquire(0, dial)
	require.NoError(t, err)
	_, err = g.acquire(0, dial)
	require.NoError(t, err)

	require.Equal(t, "", g.release(0), "connection still referenced; nothing to give back")
	require.Len(t, g.entries, 1)

	require.Equal(t, "", g.release(0))
	require.Empty(t, g.entries, "last release must drop the connection")
}

func TestConnGroupReturnsServerSlotOnLastRelease(t *testing.T) {
	g := newConnGroup()
	dial := dialStub(t)

	_, err := g.acquire(0, dial)
	require.NoError(t, err)
	_, err = g.acquire(0, dial)
	require.NoError(t, err)

	require.True(t, g.noteServerID(0, "server-a"), "first stream owns the slot")
	g.setSlotHeld(0, true)
	require.False(t, g.noteServerID(0, "server-a"), "second stream must not take a second slot")

	require.Equal(t, "", g.release(0), "slot is held until the connection goes")
	require.Equal(t, "server-a", g.release(0), "last release returns the slot to be freed")
}

func TestConnGroupDialFailureIsNotCached(t *testing.T) {
	g := newConnGroup()
	boom := errors.New("dial failed")

	_, err := g.acquire(0, func() (*grpc.ClientConn, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
	require.Empty(t, g.entries, "a failed dial must not leave an entry behind")

	_, err = g.acquire(0, dialStub(t))
	require.NoError(t, err, "a later dial for the same index must still work")
}

func TestConnGroupCloseAllReportsHeldSlots(t *testing.T) {
	g := newConnGroup()
	dial := dialStub(t)

	_, err := g.acquire(0, dial)
	require.NoError(t, err)
	require.True(t, g.noteServerID(0, "server-a"))
	g.setSlotHeld(0, true)

	_, err = g.acquire(1, dial)
	require.NoError(t, err)
	// index 1 never took a slot, so it must not be reported.

	require.ElementsMatch(t, []string{"server-a"}, g.closeAll())
	require.Empty(t, g.entries)
}

func TestFixedSlotsByMode(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mode      config.TunnelConnMode
		conns     int
		perConn   int
		wantSlots int
	}{
		{"pool is adaptive", config.TunnelConnModePool, 4, 8, 0},
		{"conns is one stream each", config.TunnelConnModeConns, 6, 8, 6},
		{"mux multiplies", config.TunnelConnModeMux, 2, 8, 16},
		{"mux with one stream each", config.TunnelConnModeMux, 4, 1, 4},
		{"zero conns floors at one", config.TunnelConnModeConns, 0, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := &tunnelClient{config: config.AgentConfig{
				TunnelConnMode:       tt.mode,
				TunnelConns:          tt.conns,
				TunnelStreamsPerConn: tt.perConn,
			}}
			require.Equal(t, tt.wantSlots, tc.fixedSlots())
		})
	}
}

func TestFixedModesPinMinSlots(t *testing.T) {
	tc := &tunnelClient{config: config.AgentConfig{
		TunnelConnMode:       config.TunnelConnModeMux,
		TunnelConns:          2,
		TunnelStreamsPerConn: 8,
		MinTunnelSlots:       4, // must be ignored by a fixed mode
	}}
	require.Equal(t, 16, tc.minSlots())

	// ...and a fixed mode must never grow or retire.
	require.False(t, tc.noteIdleRetire())
}

func TestConnIndexSpreadsWorkersEvenly(t *testing.T) {
	tc := &tunnelClient{config: config.AgentConfig{
		TunnelConnMode:       config.TunnelConnModeMux,
		TunnelConns:          3,
		TunnelStreamsPerConn: 2,
	}}

	// Worker ids start at 1 and must round-robin across the connections.
	got := make([]int, 0, 6)
	for id := 1; id <= 6; id++ {
		got = append(got, tc.connIndex(id))
	}
	require.Equal(t, []int{0, 1, 2, 0, 1, 2}, got)

	// A worker keeps the same connection across reconnects.
	require.Equal(t, tc.connIndex(4), tc.connIndex(4))
}

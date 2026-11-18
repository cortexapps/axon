package grpctunnel

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

func testDial(t *testing.T, target string, dials *int) func() (*grpc.ClientConn, error) {
	return func() (*grpc.ClientConn, error) {
		*dials++
		return grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
}

// A connection keeps redialing the target it was created for. When the
// registered server address changes, the pool must replace it, or the agent
// stays on the old host until it restarts.
func TestConnPool_ReplacesConnectionWhenTargetChanges(t *testing.T) {
	p := newConnPool(1)
	defer p.closeAll()
	dials := 0

	a, err := p.get(0, "a:443", testDial(t, "a:443", &dials))
	require.NoError(t, err)
	again, err := p.get(0, "a:443", testDial(t, "a:443", &dials))
	require.NoError(t, err)
	require.Same(t, a, again, "same target reuses the pooled connection")
	require.Equal(t, 1, dials)

	b, err := p.get(0, "b:443", testDial(t, "b:443", &dials))
	require.NoError(t, err)
	require.NotSame(t, a, b, "a new target gets a new connection")
	require.Equal(t, 2, dials)
	require.Equal(t, "b:443", b.Target()[len("passthrough:///"):])
	require.Equal(t, connectivity.Shutdown, a.GetState(), "the old connection is closed")
}

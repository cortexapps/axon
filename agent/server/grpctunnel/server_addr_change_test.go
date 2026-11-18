package grpctunnel

import (
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
)

// countingServer records how many streams said hello and how many are open.
func countingServer(t *testing.T, id string) (addr string, hellos, open *atomic.Int32, stop func()) {
	hellos, open = &atomic.Int32{}, &atomic.Int32{}
	svc := &fakeTunnelService{behavior: serverBehavior{serverID: id, heartbeatIntervalMs: 30000}}
	svc.behavior.onStream = func(stream pb.TunnelService_TunnelServer, _ *pb.ClientHello) error {
		hellos.Add(1)
		open.Add(1)
		defer open.Add(-1)
		for {
			if _, err := stream.Recv(); err != nil {
				return err
			}
		}
	}
	addr, stop = startFakeServer(t, svc)
	return addr, hellos, open, stop
}

// When re-registration returns a different server address, the agent moves
// its streams to it. Before, pooled connections kept redialing the first
// address for the life of the process.
func TestReregister_FollowsServerAddressChange(t *testing.T) {
	t.Setenv("BROKER_SERVER_URL", "")
	t.Setenv("BROKER_TOKEN", "")

	addrA, _, openA, stopA := countingServer(t, "server-a")
	defer stopA()
	addrB, hellosB, openB, stopB := countingServer(t, "server-b")
	defer stopB()

	reg := &fakeRegistration{serverURI: addrA, tokens: []string{"tok"}}
	tc, _ := newTestClient(t, config.AgentConfig{TunnelConns: 2}, reg)
	tc.storeRegistration("tok", addrA)

	require.NoError(t, tc.Start())
	defer tc.Close()
	waitFor(t, 5*time.Second, func() bool { return openA.Load() >= 2 })

	reg.setServerURI(addrB)
	tc.reregister("test", false)

	waitFor(t, 5*time.Second, func() bool { return hellosB.Load() >= 2 && openB.Load() >= 2 })
	waitFor(t, 5*time.Second, func() bool { return openA.Load() == 0 })
}

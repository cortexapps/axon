package tunnel

import (
	"context"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// Several agent replicas share one token. The entry keeps the first agent's
// identity, so per-agent connect and disconnect have to come from per-agent
// stream counts, and each stream has to remember its own agent.
func TestRegistry_TracksEachInstanceSeparately(t *testing.T) {
	registry := NewClientRegistry(zaptest.NewLogger(t))
	token := broker.NewToken("shared")
	a := ClientIdentity{InstanceID: "agent-a"}
	b := ClientIdentity{InstanceID: "agent-b"}

	res, err := registry.RegisterStream(token, a, testStream("a-1"))
	require.NoError(t, err)
	assert.Equal(t, RegisterResult{FirstForToken: true, FirstForInstance: true, TokenStreams: 1, InstanceStreams: 1}, res)

	res, err = registry.RegisterStream(token, a, testStream("a-2"))
	require.NoError(t, err)
	assert.Equal(t, RegisterResult{TokenStreams: 2, InstanceStreams: 2}, res)

	bStream := testStream("b-1")
	res, err = registry.RegisterStream(token, b, bStream)
	require.NoError(t, err)
	assert.Equal(t, RegisterResult{FirstForInstance: true, TokenStreams: 3, InstanceStreams: 1}, res)
	assert.Equal(t, "agent-b", bStream.Identity.InstanceID, "the stream keeps its own agent")
	assert.Equal(t, "agent-a", registry.GetIdentity(token).InstanceID, "the entry keeps the first agent")

	// Agent a leaves: one stream is not yet the instance edge, the second is.
	un := registry.UnregisterStream(token, "a-1")
	assert.True(t, un.Found)
	assert.False(t, un.InstanceRemoved)
	assert.Equal(t, 1, un.InstanceStreams)
	un = registry.UnregisterStream(token, "a-2")
	assert.True(t, un.InstanceRemoved)
	assert.False(t, un.TokenRemoved, "agent b still holds the token")
	assert.Equal(t, "agent-a", un.Identity.InstanceID)

	// Agent b's last stream is both edges, and is reported as agent b even
	// though the entry was created by agent a.
	un = registry.UnregisterStream(token, "b-1")
	assert.True(t, un.InstanceRemoved)
	assert.True(t, un.TokenRemoved)
	assert.Equal(t, "agent-b", un.Identity.InstanceID)

	assert.False(t, registry.UnregisterStream(token, "b-1").Found)
}

// openStream completes a handshake for instanceID and returns the stream and
// a cancel that tears it down from the client side.
func openStream(t *testing.T, client pb.TunnelServiceClient, token, instanceID string) (pb.TunnelService_TunnelClient, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ClientFrame{Msg: &pb.ClientFrame_Hello{Hello: &pb.ClientHello{
		BrokerToken: token,
		Integration: "gitlab",
		Alias:       "gitlab-relay",
		InstanceId:  instanceID,
	}}}))
	msg, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetHello())
	return stream, cancel
}

// waitForLog returns the context of the n-th (1-based) entry with message msg.
func waitForLog(t *testing.T, logs *observer.ObservedLogs, msg string, n int) map[string]any {
	t.Helper()
	var fields map[string]any
	require.Eventually(t, func() bool {
		found := logs.FilterMessage(msg).All()
		if len(found) < n {
			return false
		}
		fields = found[n-1].ContextMap()
		return true
	}, 5*time.Second, 20*time.Millisecond, "no %q log (want %d)", msg, n)
	return fields
}

// A proxy or load balancer dropping the agent's connection ends the stream's
// context at the same moment its Recv fails, and the context branch used to
// clean up silently. Both have to produce the disconnect line, with the agent
// and the reason.
func TestTunnel_ClientDisconnectIsLoggedPerInstance(t *testing.T) {
	client, logs := startTestServiceWithLogs(t)

	_, cancel1 := openStream(t, client, "tok", "agent-a")
	_, cancel2 := openStream(t, client, "tok", "agent-a")

	connected := waitForLog(t, logs, "Agent instance connected", 1)
	assert.Equal(t, "agent-a", connected["instanceId"])
	assert.Equal(t, "gitlab-relay", connected["alias"])
	assert.Equal(t, broker.NewToken("tok").Hashed(), connected["tokenHash"])
	assert.Len(t, logs.FilterMessage("Agent instance connected").All(), 1,
		"an agent's second stream is not a new connection")

	// Losing one of two streams is not the agent leaving.
	cancel1()
	waitForLog(t, logs, "Tunnel stream closed", 1)
	assert.Empty(t, logs.FilterMessage("Agent instance disconnected").All())

	cancel2()
	disconnected := waitForLog(t, logs, "Agent instance disconnected", 1)
	assert.Equal(t, "agent-a", disconnected["instanceId"])
	assert.Equal(t, closeClientGone, disconnected["reason"])
	assert.EqualValues(t, 0, disconnected["instanceStreams"])
	assert.Contains(t, disconnected, "connectedFor")
}

func TestTunnel_ClientHalfCloseIsClientClosed(t *testing.T) {
	client, logs := startTestServiceWithLogs(t)

	stream, cancel := openStream(t, client, "tok", "agent-a")
	defer cancel()
	require.NoError(t, stream.CloseSend())

	disconnected := waitForLog(t, logs, "Agent instance disconnected", 1)
	assert.Equal(t, closeClientClosed, disconnected["reason"])
}

func TestTunnel_HeartbeatTimeoutIsLoggedWithReason(t *testing.T) {
	client, logs := startTestServiceWithLogs(t, func(c *config.Config) {
		c.HeartbeatInterval = 100 * time.Millisecond
	})

	stream, cancel := openStream(t, client, "tok", "agent-a")
	defer cancel()
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	disconnected := waitForLog(t, logs, "Agent instance disconnected", 1)
	assert.Equal(t, closeHeartbeatTimeout, disconnected["reason"])
	assert.Equal(t, "agent-a", disconnected["instanceId"])
}

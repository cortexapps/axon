package tunnel

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startTestService spins up a real Service on an ephemeral localhost port and
// returns a client connection. Caller closes via t.Cleanup.
func startTestService(t *testing.T) pb.TunnelServiceClient {
	t.Helper()
	logger := zaptest.NewLogger(t)

	cfg := config.Config{
		ServerID:          "test-server",
		HeartbeatInterval: 30 * time.Second,
	}
	registry := NewClientRegistry(logger)
	brokerClient := broker.NewClient("", "test-server", logger)
	m := metrics.New("test-server")
	svc := NewService(cfg, logger, registry, brokerClient, m)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	pb.RegisterTunnelServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() {
		conn.Close()
		grpcServer.Stop()
	})

	return pb.NewTunnelServiceClient(conn)
}

// TestTunnel_EmptyBrokerToken_ReturnsUnauthenticated asserts that an empty
// broker_token causes the server to return codes.Unauthenticated so the client
// can distinguish auth failures from transient network errors.
func TestTunnel_EmptyBrokerToken_ReturnsUnauthenticated(t *testing.T) {
	client := startTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)

	require.NoError(t, stream.Send(&pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_Hello{
			Hello: &pb.ClientHello{
				BrokerToken: "", // empty token
				TenantId:    "tenant-1",
				Integration: "github",
			},
		},
	}))

	_, err = stream.Recv()
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "expected Unauthenticated, got %v", err)
}

// TestTunnel_EmptyTenantID_ReturnsUnauthenticated asserts that an empty
// tenant_id also yields codes.Unauthenticated.
func TestTunnel_EmptyTenantID_ReturnsUnauthenticated(t *testing.T) {
	client := startTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)

	require.NoError(t, stream.Send(&pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_Hello{
			Hello: &pb.ClientHello{
				BrokerToken: "valid-token",
				TenantId:    "", // empty tenant
				Integration: "github",
			},
		},
	}))

	_, err = stream.Recv()
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "expected Unauthenticated, got %v", err)
}

// TestTunnel_ValidHandshake_Succeeds asserts that a well-formed hello
// produces a ServerHello (sanity check for the test scaffold itself).
func TestTunnel_ValidHandshake_Succeeds(t *testing.T) {
	client := startTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)

	require.NoError(t, stream.Send(&pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_Hello{
			Hello: &pb.ClientHello{
				BrokerToken: "valid-token",
				TenantId:    "tenant-1",
				Integration: "github",
				InstanceId:  "i-1",
			},
		},
	}))

	msg, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetHello())
	require.Equal(t, "test-server", msg.GetHello().ServerId)
}

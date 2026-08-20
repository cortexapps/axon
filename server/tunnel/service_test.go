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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startTestService spins up a real Service on an ephemeral localhost port and
// returns a client connection. Caller closes via t.Cleanup.
func startTestService(t *testing.T, opts ...func(*config.Config)) pb.TunnelServiceClient {
	t.Helper()
	client, _ := startTestServiceWithRegistry(t, opts...)
	return client
}

// startTestServiceWithLogs is startTestService plus the log records it wrote,
// for tests that assert on what an operator would actually read. An observer
// core rather than zaptest: the service's BROKER_SERVER notify goroutines can
// log after the test finishes, which zaptest treats as a failure and an
// observer absorbs.
func startTestServiceWithLogs(t *testing.T, opts ...func(*config.Config)) (pb.TunnelServiceClient, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	client, _ := startTestServiceWithLogger(t, zap.New(core), opts...)
	return client, logs
}

// startTestServiceWithRegistry is startTestService plus the registry, for tests
// that assert on server-side stream bookkeeping.
func startTestServiceWithRegistry(t *testing.T, opts ...func(*config.Config)) (pb.TunnelServiceClient, *ClientRegistry) {
	t.Helper()
	// Nop logger: the service spawns goroutines (BROKER_SERVER notify) that
	// may log after the test completes, which zaptest treats as a failure.
	return startTestServiceWithLogger(t, zap.NewNop(), opts...)
}

func startTestServiceWithLogger(t *testing.T, logger *zap.Logger, opts ...func(*config.Config)) (pb.TunnelServiceClient, *ClientRegistry) {
	t.Helper()

	cfg := config.Config{
		ServerID:          "test-server",
		HeartbeatInterval: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
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

	return pb.NewTunnelServiceClient(conn), registry
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

	require.NoError(t, stream.Send(&pb.ClientFrame{
		Msg: &pb.ClientFrame_Hello{
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

// TestTunnel_EmptyTenantID_Succeeds asserts that tenant_id is informational
// metadata: an empty value does not gate the handshake — the broker token is
// the sole credential.
func TestTunnel_EmptyTenantID_Succeeds(t *testing.T) {
	client := startTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)

	require.NoError(t, stream.Send(&pb.ClientFrame{
		Msg: &pb.ClientFrame_Hello{
			Hello: &pb.ClientHello{
				BrokerToken: "valid-token",
				TenantId:    "", // informational; empty is fine
				Integration: "github",
			},
		},
	}))

	msg, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetHello())
}

// TestTunnel_PerTokenStreamCap_Wire validates the stream cap end to end
// through the real gRPC handshake: with MaxStreamsPerToken=1, a second
// concurrent stream for the same token is rejected with ResourceExhausted
// while the first stays healthy, and a new stream succeeds after the first
// closes.
func TestTunnel_PerTokenStreamCap_Wire(t *testing.T) {
	client := startTestService(t, func(c *config.Config) {
		c.MaxStreamsPerToken = 1
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hello := func() *pb.ClientFrame {
		return &pb.ClientFrame{
			Msg: &pb.ClientFrame_Hello{
				Hello: &pb.ClientHello{
					BrokerToken: "cap-token",
					TenantId:    "tenant-1",
					Integration: "github",
				},
			},
		}
	}

	// First stream: handshake completes and announces the cap.
	stream1, err := client.Tunnel(ctx)
	require.NoError(t, err)
	require.NoError(t, stream1.Send(hello()))
	msg, err := stream1.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetHello())
	require.Equal(t, int32(1), msg.GetHello().MaxStreams)

	// Second concurrent stream for the same token: ResourceExhausted.
	stream2, err := client.Tunnel(ctx)
	require.NoError(t, err)
	require.NoError(t, stream2.Send(hello()))
	_, err = stream2.Recv()
	// The server sends ServerHello before registering, so the rejection may
	// arrive on the first or second Recv depending on scheduling.
	if err == nil {
		_, err = stream2.Recv()
	}
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "expected ResourceExhausted, got %v", err)

	// First stream is still healthy: it can send a heartbeat and stays open.
	require.NoError(t, stream1.Send(&pb.ClientFrame{
		Msg: &pb.ClientFrame_Heartbeat{Heartbeat: &pb.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
	}))

	// Close the first stream; its slot frees and a new stream succeeds.
	require.NoError(t, stream1.CloseSend())
	cancelStream1Cleanup := waitForStreamRelease(t, client, ctx, hello)
	cancelStream1Cleanup()
}

// waitForStreamRelease retries the handshake until the server has processed
// the previous stream's close and admits a new one (cleanup is async).
func waitForStreamRelease(t *testing.T, client pb.TunnelServiceClient, ctx context.Context, hello func() *pb.ClientFrame) func() {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stream, err := client.Tunnel(ctx)
		require.NoError(t, err)
		if err := stream.Send(hello()); err == nil {
			if msg, err := stream.Recv(); err == nil && msg.GetHello() != nil {
				return func() { _ = stream.CloseSend() }
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("stream slot was not released after close")
	return nil
}

// TestTunnel_ValidHandshake_Succeeds asserts that a well-formed hello
// produces a ServerHello (sanity check for the test scaffold itself).
func TestTunnel_ValidHandshake_Succeeds(t *testing.T) {
	client := startTestService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)

	require.NoError(t, stream.Send(&pb.ClientFrame{
		Msg: &pb.ClientFrame_Hello{
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

// TestTunnel_HeartbeatTimeout_UnregistersStream is the regression test for the
// uninterruptible Recv: the heartbeat timeout has to actually reap the stream
// from the registry, not just stop heartbeating at it.
//
// The client here is the shape gRPC keepalive cannot catch. It completes the
// handshake, keeps draining server frames so the transport stays healthy and
// answers PINGs, and then never sends a heartbeat again. Nothing at the
// transport layer will notice, so if cancelling ctx does not interrupt the
// server's blocked Recv, the stream stays registered and keeps occupying its
// slot against the per-token cap.
func TestTunnel_HeartbeatTimeout_UnregistersStream(t *testing.T) {
	client, registry := startTestServiceWithRegistry(t, func(c *config.Config) {
		c.HeartbeatInterval = 200 * time.Millisecond
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := client.Tunnel(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ClientFrame{
		Msg: &pb.ClientFrame_Hello{
			Hello: &pb.ClientHello{
				BrokerToken: "wedged-token",
				TenantId:    "tenant-1",
				Integration: "github",
			},
		},
	}))
	msg, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetHello())
	require.Equal(t, 1, registry.StreamCount(), "stream should be registered after the handshake")

	// Drain server frames so its sends never block, but never heartbeat back.
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	// The monitor ticks every 2*HeartbeatInterval and reaps once that much time
	// has passed with no client frame, so this lands within a few ticks.
	require.Eventually(t, func() bool { return registry.StreamCount() == 0 },
		10*time.Second, 50*time.Millisecond,
		"heartbeat timeout did not unregister the stream")
}

// rejectionEntry waits for the stream-cap warning and returns its fields. The
// client's error can surface before the handler's log write lands, so poll.
func rejectionEntry(t *testing.T, logs *observer.ObservedLogs) map[string]any {
	t.Helper()
	var fields map[string]any
	require.Eventually(t, func() bool {
		found := logs.FilterMessage("Rejecting stream: token at stream cap").All()
		if len(found) == 0 {
			return false
		}
		fields = found[0].ContextMap()
		return true
	}, 5*time.Second, 20*time.Millisecond, "no stream-cap rejection was logged")
	return fields
}

// driveToCap opens streams until one is rejected, with a cap of 1.
func driveToCap(t *testing.T, client pb.TunnelServiceClient, ctx context.Context, tenantID string) {
	t.Helper()
	hello := func() *pb.ClientFrame {
		return &pb.ClientFrame{
			Msg: &pb.ClientFrame_Hello{
				Hello: &pb.ClientHello{
					BrokerToken:   "cap-token",
					TenantId:      tenantID,
					Integration:   "github",
					Alias:         "my-github",
					InstanceId:    "agent-7",
					ClientVersion: "v1.2.3",
				},
			},
		}
	}
	stream1, err := client.Tunnel(ctx)
	require.NoError(t, err)
	require.NoError(t, stream1.Send(hello()))
	msg, err := stream1.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetHello())

	stream2, err := client.Tunnel(ctx)
	require.NoError(t, err)
	require.NoError(t, stream2.Send(hello()))
	if _, err := stream2.Recv(); err == nil {
		_, _ = stream2.Recv()
	}
}

// The rejection is the line an operator reads when a token is at its cap, and
// it has to be joinable to the dispatcher's view of the same token.
// BROKER_SERVER logs the hashed token as its connection id, so tokenHash is
// the join key; instanceId, integration and clientVersion say which agent
// tripped the cap. Logging only tenantId and cap names no agent at all — and
// tenantId is empty in every real deployment (see the test below).
func TestTunnel_StreamCapRejection_LogsAttribution(t *testing.T) {
	client, logs := startTestServiceWithLogs(t, func(c *config.Config) {
		c.MaxStreamsPerToken = 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	driveToCap(t, client, ctx, "tenant-1")
	fields := rejectionEntry(t, logs)

	require.Equal(t, broker.NewToken("cap-token").Hashed(), fields["tokenHash"],
		"tokenHash is the only field that joins this warning to the dispatcher's logs")
	require.Equal(t, "agent-7", fields["instanceId"])
	require.Equal(t, "github", fields["integration"])
	require.Equal(t, "my-github", fields["alias"])
	require.Equal(t, "v1.2.3", fields["clientVersion"])
	require.Equal(t, "tenant-1", fields["tenantId"])
	require.EqualValues(t, 1, fields["cap"])
}

// The agent's only source for tenant_id is CORTEX_TENANT_ID, which is set
// nowhere outside the local test compose files — so in staging and production
// the field arrives empty. Emitting it as "" makes a blank column in the log
// backend indistinguishable from a tenant whose id really is the empty string,
// and silently attributes every rejection to the same non-existent tenant.
// Omitting the key lets "no tenant claimed" be queried as absent.
func TestTunnel_StreamCapRejection_OmitsTenantIdWhenAgentSentNone(t *testing.T) {
	client, logs := startTestServiceWithLogs(t, func(c *config.Config) {
		c.MaxStreamsPerToken = 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	driveToCap(t, client, ctx, "")
	fields := rejectionEntry(t, logs)

	_, present := fields["tenantId"]
	require.False(t, present, "tenantId should be omitted, not logged as an empty string")
	// The agent is still identified by what it did send.
	require.Equal(t, "agent-7", fields["instanceId"])
	require.Equal(t, broker.NewToken("cap-token").Hashed(), fields["tokenHash"])
}

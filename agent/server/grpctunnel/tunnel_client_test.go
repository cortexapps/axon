package grpctunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/requestexecutor"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// -----------------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------------

// fakeRegistration is a minimal Registration that returns a sequence of
// pre-configured responses. Multiple calls cycle through tokens for token-
// rotation tests.
type fakeRegistration struct {
	mu        sync.Mutex
	serverURI string
	tokens    []string
	idx       int
	calls     int32
}

func (r *fakeRegistration) Register(integration common.Integration, alias string) (*snykbroker.RegistrationInfoResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	atomic.AddInt32(&r.calls, 1)
	tok := r.tokens[r.idx]
	if r.idx < len(r.tokens)-1 {
		r.idx++
	}
	return &snykbroker.RegistrationInfoResponse{
		ServerUri: r.serverURI,
		Token:     tok,
	}, nil
}

// stubExecutor is a deterministic RequestExecutor for handleRequest tests.
type stubExecutor struct {
	mu          sync.Mutex
	delay       time.Duration
	statusCode  int
	body        []byte
	err         error
	respectCtx  bool
	calls       int32
	lastTimeout time.Duration
}

func (s *stubExecutor) Execute(ctx context.Context, method, path string, headers map[string]string, body []byte) (*requestexecutor.ExecutorResponse, error) {
	atomic.AddInt32(&s.calls, 1)
	if dl, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.lastTimeout = time.Until(dl)
		s.mu.Unlock()
	}
	if s.respectCtx && s.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.delay):
		}
	} else if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &requestexecutor.ExecutorResponse{
		StatusCode: s.statusCode,
		Headers:    map[string]string{"Content-Type": "text/plain"},
		Body:       s.body,
	}, nil
}

// -----------------------------------------------------------------------------
// Fake TunnelService implementations
// -----------------------------------------------------------------------------

// serverBehavior controls how a single fake server handles a stream.
type serverBehavior struct {
	serverID            string
	heartbeatIntervalMs int32
	// onStream is invoked for every incoming stream. If nil, a default
	// "ServerHello + heartbeat-on-recv" loop runs.
	onStream func(stream pb.TunnelService_TunnelServer, helloReceived *pb.ClientHello) error
}

type fakeTunnelService struct {
	pb.UnimplementedTunnelServiceServer
	streams  atomic.Int32
	behavior serverBehavior
}

func (s *fakeTunnelService) Tunnel(stream pb.TunnelService_TunnelServer) error {
	s.streams.Add(1)

	firstMsg, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := firstMsg.GetHello()
	if hello == nil {
		return fmt.Errorf("expected ClientHello")
	}

	streamID := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	hbMs := s.behavior.heartbeatIntervalMs
	if hbMs == 0 {
		hbMs = 30000
	}

	if err := stream.Send(&pb.TunnelServerMessage{
		Message: &pb.TunnelServerMessage_Hello{
			Hello: &pb.ServerHello{
				ServerId:            s.behavior.serverID,
				HeartbeatIntervalMs: hbMs,
				StreamId:            streamID,
			},
		},
	}); err != nil {
		return err
	}

	if s.behavior.onStream != nil {
		return s.behavior.onStream(stream, hello)
	}

	// Default: respond to recv loop forever.
	for {
		_, err := stream.Recv()
		if err != nil {
			return err
		}
	}
}

// startFakeServer launches a fake gRPC tunnel server on a random localhost port
// and returns the host:port and a stop function.
func startFakeServer(t *testing.T, svc pb.TunnelServiceServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	pb.RegisterTunnelServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	stop := func() {
		grpcServer.Stop()
	}
	return lis.Addr().String(), stop
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// newTestClient builds a tunnelClient ready for testing, with metrics
// registered, a stub executor wired in, and the given registration backend.
func newTestClient(t *testing.T, cfg config.AgentConfig, reg snykbroker.Registration) (*tunnelClient, *prometheus.Registry) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	registry := prometheus.NewRegistry()

	cfg.GrpcInsecure = true
	if cfg.TunnelCount == 0 {
		cfg.TunnelCount = 1
	}
	if cfg.MaxStreamsPerServer == 0 {
		cfg.MaxStreamsPerServer = 2
	}
	if cfg.MaxInflightRequests == 0 {
		cfg.MaxInflightRequests = 16
	}
	if cfg.MaxRequestTimeout == 0 {
		cfg.MaxRequestTimeout = 5 * time.Second
	}
	if cfg.FailWaitTime == 0 {
		cfg.FailWaitTime = 50 * time.Millisecond
	}
	if cfg.InstanceId == "" {
		cfg.InstanceId = "test-instance"
	}

	tc := &tunnelClient{
		config:             cfg,
		logger:             logger.Named("grpc-tunnel"),
		integrationInfo:    common.IntegrationInfo{Integration: common.IntegrationGithub},
		registration:       reg,
		serverStreamCounts: make(map[string]int),
	}
	tc.executor = &stubExecutor{statusCode: 200, body: []byte("ok")}

	if cfg.MaxInflightRequests > 0 {
		tc.inflightSem = make(chan struct{}, cfg.MaxInflightRequests)
	}

	tc.connectionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "grpc_tunnel_connections_active"}, []string{"server_id"})
	tc.requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_requests_total"}, []string{"method", "status"})
	tc.requestsInflight = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "grpc_tunnel_requests_inflight"})
	tc.requestsRejected = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "grpc_tunnel_request_rejected_total"})
	tc.reconnectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_reconnects_total"}, []string{"server_id"})
	tc.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "grpc_tunnel_request_duration_ms"}, []string{"method"})
	tc.heartbeatTimeoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_heartbeat_timeouts_total"}, []string{"server_id"})
	tc.tokenRotationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "grpc_tunnel_token_rotations_total"})
	tc.connectErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_connect_errors_total"}, []string{"phase"})

	registry.MustRegister(
		tc.connectionsActive, tc.requestsTotal, tc.requestsInflight,
		tc.requestsRejected, tc.reconnectsTotal, tc.requestDuration,
		tc.heartbeatTimeoutsTotal, tc.tokenRotationsTotal, tc.connectErrorsTotal,
	)

	return tc, registry
}

// startClientWithEnv sets BROKER_SERVER_URL/BROKER_TOKEN so the client uses
// direct-config mode (skipping initial registration), then starts it.
func startClientWithEnv(t *testing.T, tc *tunnelClient, serverAddr, token string) {
	t.Helper()
	t.Setenv("BROKER_SERVER_URL", serverAddr)
	t.Setenv("BROKER_TOKEN", token)
	require.NoError(t, tc.Start())
}

func counterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m, err := cv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	pb := &dto.Metric{}
	require.NoError(t, m.Write(pb))
	return pb.GetCounter().GetValue()
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	pb := &dto.Metric{}
	require.NoError(t, c.Write(pb))
	return pb.GetCounter().GetValue()
}

func gaugeVecValue(t *testing.T, gv *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	m, err := gv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	pb := &dto.Metric{}
	require.NoError(t, m.Write(pb))
	return pb.GetGauge().GetValue()
}

// waitFor polls a condition until true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// TestStreamCap_LimitsStreamsPerServer asserts MaxStreamsPerServer=2 prevents
// a third stream landing on the same server_id, even though TunnelCount=3.
func TestStreamCap_LimitsStreamsPerServer(t *testing.T) {
	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "single-server"}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{
		TunnelCount:                 3,
		MaxStreamsPerServer:         2,
		RegistrationRefreshInterval: 0,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	waitFor(t, 3*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "single-server") == 2
	})

	// Confirm cap holds — connectionsActive stays at 2 for the cap duration.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, float64(2), gaugeVecValue(t, tc.connectionsActive, "single-server"))
}

// TestHeartbeatTimeout_TriggersReconnect: server sends ServerHello then goes
// silent. Client should detect timeout via 2x heartbeat interval and reconnect.
func TestHeartbeatTimeout_TriggersReconnect(t *testing.T) {
	var streams atomic.Int32
	svc := &fakeTunnelService{behavior: serverBehavior{
		serverID:            "hb-server",
		heartbeatIntervalMs: 100, // 2x = 200ms timeout
		onStream: func(stream pb.TunnelService_TunnelServer, _ *pb.ClientHello) error {
			streams.Add(1)
			// Don't send heartbeats; keep Recv alive so the stream stays "open".
			for {
				_, err := stream.Recv()
				if err != nil {
					return err
				}
			}
		},
	}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{TunnelCount: 1, MaxStreamsPerServer: 2}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	// Wait for at least one heartbeat timeout to register.
	waitFor(t, 5*time.Second, func() bool {
		return counterVecValue(t, tc.heartbeatTimeoutsTotal, "hb-server") >= 1
	})

	// And expect reconnect (multiple streams over time).
	waitFor(t, 5*time.Second, func() bool {
		return streams.Load() >= 2
	})
}

// TestRecvErrorReconnects: server closes the stream mid-life; client reopens.
func TestRecvErrorReconnects(t *testing.T) {
	var streams atomic.Int32
	svc := &fakeTunnelService{behavior: serverBehavior{
		serverID:            "rc-server",
		heartbeatIntervalMs: 30000,
		onStream: func(stream pb.TunnelService_TunnelServer, _ *pb.ClientHello) error {
			streams.Add(1)
			// First two streams die immediately, third stays alive.
			if streams.Load() <= 2 {
				return io.EOF
			}
			for {
				_, err := stream.Recv()
				if err != nil {
					return err
				}
			}
		},
	}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{TunnelCount: 1, MaxStreamsPerServer: 2}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	waitFor(t, 5*time.Second, func() bool {
		return streams.Load() >= 3
	})
}

// TestInitialConnectRetry_RefusedDial: dial fails initially (no server),
// then a server starts on the same port; client should establish.
func TestInitialConnectRetry_RefusedDial(t *testing.T) {
	// Pre-allocate a port, close the listener, then start the client
	// pointing at that port. After a few failed attempts, start the real server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	lis.Close()

	cfg := config.AgentConfig{TunnelCount: 1, MaxStreamsPerServer: 2}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	// Wait for at least one connect error to be recorded.
	waitFor(t, 5*time.Second, func() bool {
		// Any connect-phase error counter > 0.
		total := 0.0
		for _, phase := range []string{"dial", "recv_hello", "send_hello", "open_stream"} {
			total += counterVecValue(t, tc.connectErrorsTotal, phase)
		}
		return total >= 1
	})

	// Now bring the server up.
	lis2, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "delayed-server"}}
	pb.RegisterTunnelServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis2) }()
	defer grpcServer.Stop()

	waitFor(t, 10*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "delayed-server") == 1
	})
}

// TestSendErrorCancelsStream: when stream.Send fails, the wrapping sendFunc
// cancels the stream context so the recv loop returns and a reconnect happens.
func TestSendErrorCancelsStream(t *testing.T) {
	var streams atomic.Int32
	svc := &fakeTunnelService{behavior: serverBehavior{
		serverID:            "se-server",
		heartbeatIntervalMs: 50, // server sends heartbeats fast
		onStream: func(stream pb.TunnelService_TunnelServer, _ *pb.ClientHello) error {
			streams.Add(1)
			// First two streams: server sends one heartbeat then closes (forcing
			// the client's heartbeat-response Send to fail on the dead stream).
			if streams.Load() <= 2 {
				_ = stream.Send(&pb.TunnelServerMessage{
					Message: &pb.TunnelServerMessage_Heartbeat{
						Heartbeat: &pb.Heartbeat{TimestampMs: time.Now().UnixMilli()},
					},
				})
				return io.EOF
			}
			// Steady-state.
			for {
				_, err := stream.Recv()
				if err != nil {
					return err
				}
			}
		},
	}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{TunnelCount: 1, MaxStreamsPerServer: 2}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	waitFor(t, 5*time.Second, func() bool { return streams.Load() >= 3 })
}

// TestInflightCap_RejectsOverflow: with MaxInflightRequests=1 and a slow
// executor, the 2nd concurrent request should be rejected with 503.
func TestInflightCap_RejectsOverflow(t *testing.T) {
	cfg := config.AgentConfig{
		TunnelCount:         1,
		MaxStreamsPerServer: 2,
		MaxInflightRequests: 1,
		MaxRequestTimeout:   5 * time.Second,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.executor = &stubExecutor{
		statusCode: 200,
		body:       []byte("ok"),
		delay:      300 * time.Millisecond,
	}

	// Drive handleRequest directly with a no-op sendFn so we don't need a server.
	sent := make(chan *pb.HttpResponse, 4)
	sendFn := func(msg *pb.TunnelClientMessage) error {
		if r := msg.GetHttpResponse(); r != nil {
			sent <- r
		}
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		tc.handleRequest(sendFn, &pb.HttpRequest{
			RequestId: "r1", Method: "GET", Path: "/", IsFinal: true,
		})
	}()
	// Give r1 a head start to acquire the semaphore.
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		tc.handleRequest(sendFn, &pb.HttpRequest{
			RequestId: "r2", Method: "GET", Path: "/", IsFinal: true,
		})
	}()

	wg.Wait()
	close(sent)

	var rejected, ok bool
	for r := range sent {
		switch r.StatusCode {
		case 503:
			rejected = true
			require.True(t, r.IsFailedDispatch)
		case 200:
			ok = true
		}
	}
	require.True(t, rejected, "expected one 503 from in-flight cap")
	require.True(t, ok, "expected one 200 from the in-flight request")
	require.Equal(t, float64(1), counterValue(t, tc.requestsRejected))
}

// TestMaxRequestTimeout_AppliesWhenTimeoutMsZero: with TimeoutMs=0 and
// MaxRequestTimeout=200ms, a 5-second executor should return deadline exceeded.
func TestMaxRequestTimeout_AppliesWhenTimeoutMsZero(t *testing.T) {
	cfg := config.AgentConfig{
		TunnelCount:         1,
		MaxInflightRequests: 4,
		MaxRequestTimeout:   200 * time.Millisecond,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})

	stub := &stubExecutor{
		statusCode: 200,
		body:       []byte("ok"),
		delay:      5 * time.Second,
		respectCtx: true,
	}
	tc.executor = stub

	var got *pb.HttpResponse
	done := make(chan struct{})
	sendFn := func(msg *pb.TunnelClientMessage) error {
		if r := msg.GetHttpResponse(); r != nil {
			got = r
			close(done)
		}
		return nil
	}

	start := time.Now()
	go tc.handleRequest(sendFn, &pb.HttpRequest{
		RequestId: "r1", Method: "GET", Path: "/", TimeoutMs: 0, IsFinal: true,
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleRequest did not complete within 2s — MaxRequestTimeout not applied")
	}

	require.WithinDuration(t, start.Add(200*time.Millisecond), time.Now(), 1*time.Second)
	require.NotNil(t, got)
	require.Equal(t, int32(502), got.StatusCode)

	// Verify the stub saw a context with a short deadline (well under 1s).
	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Less(t, stub.lastTimeout, 1*time.Second, "executor ctx had deadline %v", stub.lastTimeout)
}

// TestCACertLoadFailure_NoSilentDowngrade: an invalid CA cert path must
// prevent startup, not silently fall back to insecure.
func TestCACertLoadFailure_NoSilentDowngrade(t *testing.T) {
	cfg := config.AgentConfig{
		TunnelCount:         1,
		MaxStreamsPerServer: 2,
		HttpCaCertFilePath:  filepath.Join(t.TempDir(), "missing-ca.pem"),
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.config.GrpcInsecure = false // force TLS path

	_, err := tc.buildTransportCredentials()
	require.Error(t, err, "missing CA cert must error, not silently downgrade")
	require.Contains(t, err.Error(), "read CA cert")
}

// TestAssemblerCap_EvictsOldest: more than maxPending first-chunks should
// cause the oldest to be evicted.
func TestAssemblerCap_EvictsOldest(t *testing.T) {
	ra := newRequestAssembler(3, zap.NewNop())

	for i := 0; i < 4; i++ {
		ra.handleChunk(&pb.HttpRequest{
			RequestId:  fmt.Sprintf("req-%d", i),
			Method:     "POST",
			Path:       "/",
			Body:       []byte("a"),
			ChunkIndex: 0,
			IsFinal:    false,
		})
	}

	ra.mu.Lock()
	_, hasReq0 := ra.pending["req-0"]
	_, hasReq3 := ra.pending["req-3"]
	count := len(ra.pending)
	ra.mu.Unlock()

	require.Equal(t, 3, count, "should have evicted to fit cap")
	require.False(t, hasReq0, "oldest (req-0) should be evicted")
	require.True(t, hasReq3, "newest (req-3) should remain")
}

// TestAuthErrorTriggersReregister: server returns codes.Unauthenticated on
// the first connection; client should re-register and the second attempt
// should send the rotated token.
func TestAuthErrorTriggersReregister(t *testing.T) {
	var receivedTokens []string
	var tokensMu sync.Mutex
	var helloCount atomic.Int32

	svc := &fakeTunnelService{behavior: serverBehavior{
		serverID:            "auth-server",
		heartbeatIntervalMs: 30000,
		onStream: nil,
	}}
	// We need to intercept the ClientHello to capture the token. Override
	// the default Tunnel by wrapping.
	svc.behavior.onStream = func(stream pb.TunnelService_TunnelServer, hello *pb.ClientHello) error {
		tokensMu.Lock()
		receivedTokens = append(receivedTokens, hello.BrokerToken)
		tokensMu.Unlock()
		// Default: stay alive.
		for {
			_, err := stream.Recv()
			if err != nil {
				return err
			}
		}
	}

	// Wrap the service to inject Unauthenticated on first call before sending ServerHello.
	wrapped := &authFailingService{inner: svc, helloCount: &helloCount}
	addr, stop := startFakeServer(t, wrapped)
	defer stop()

	cfg := config.AgentConfig{TunnelCount: 1, MaxStreamsPerServer: 2}
	reg := &fakeRegistration{
		serverURI: addr,
		tokens:    []string{"new-token-after-401"},
	}
	tc, _ := newTestClient(t, cfg, reg)

	// Seed currentToken with the stale token via env, but force re-register on auth.
	// Use direct-config first to get into running state, then clear env so reregister
	// will actually call the registration backend.
	t.Setenv("BROKER_SERVER_URL", "")
	t.Setenv("BROKER_TOKEN", "")

	tc.mu.Lock()
	tc.currentToken = "stale-token"
	tc.currentServerAddr = addr
	tc.mu.Unlock()

	// Skip initial registration since we pre-seeded the token.
	require.NoError(t, tc.Start())
	defer tc.Close()

	waitFor(t, 5*time.Second, func() bool {
		tokensMu.Lock()
		defer tokensMu.Unlock()
		for _, t := range receivedTokens {
			if t == "new-token-after-401" {
				return true
			}
		}
		return false
	})

	require.GreaterOrEqual(t, atomic.LoadInt32(&reg.calls), int32(1), "registration should have been called")
}

// authFailingService rejects the first connection with codes.Unauthenticated.
type authFailingService struct {
	pb.UnimplementedTunnelServiceServer
	inner      *fakeTunnelService
	helloCount *atomic.Int32
}

func (a *authFailingService) Tunnel(stream pb.TunnelService_TunnelServer) error {
	n := a.helloCount.Add(1)
	if n == 1 {
		// Read hello so the client doesn't block, then return Unauthenticated.
		_, _ = stream.Recv()
		return status.Error(codes.Unauthenticated, "stale token")
	}
	return a.inner.Tunnel(stream)
}

// TestClose_ShutsDownCleanly: Start + Close completes with no leaked goroutines / hangs.
func TestClose_ShutsDownCleanly(t *testing.T) {
	svc := &fakeTunnelService{behavior: serverBehavior{serverID: "clean-close"}}
	addr, stop := startFakeServer(t, svc)
	defer stop()

	cfg := config.AgentConfig{TunnelCount: 2, MaxStreamsPerServer: 2}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"t"}})
	startClientWithEnv(t, tc, addr, "t")

	waitFor(t, 3*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "clean-close") >= 1
	})

	done := make(chan error, 1)
	go func() { done <- tc.Close() }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s")
	}
}

// Make sure the package compiles even without snykbroker import being only
// used inside test fixtures.
var _ = os.Getenv

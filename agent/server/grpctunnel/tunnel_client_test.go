package grpctunnel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
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

// stubBackend is a deterministic Backend for call-handling tests.
type stubBackend struct {
	mu          sync.Mutex
	delay       time.Duration
	statusCode  int
	body        []byte
	err         error
	respectCtx  bool
	calls       int32
	lastTimeout time.Duration
}

func (s *stubBackend) Do(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
	atomic.AddInt32(&s.calls, 1)
	if dl, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.lastTimeout = time.Until(dl)
		s.mu.Unlock()
	}
	// Drain the request body like a real backend would.
	if req.Body != nil {
		io.Copy(io.Discard, req.Body)
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
	return &BackendResponse{
		StatusCode: s.statusCode,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Trailer:    func() http.Header { return nil },
	}, nil
}

// catchAllRouter builds a Router with a single any-method wildcard rule so
// every call routes; the stub backend never dials the origin.
func catchAllRouter(t *testing.T) *Router {
	t.Helper()
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}
	af, err := acceptfile.NewAcceptFile([]byte(`{
		"private": [{"method": "any", "path": "/*", "origin": "http://stub.internal"}]
	}`), cfg, zap.NewNop())
	require.NoError(t, err)
	router, err := NewRouter(af.Wrapper().PrivateRules(), zap.NewNop())
	require.NoError(t, err)
	return router
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

	if err := stream.Send(&pb.ServerFrame{
		Msg: &pb.ServerFrame_Hello{
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
// registered, a stub router/backend wired in, and the given registration
// backend.
func newTestClient(t *testing.T, cfg config.AgentConfig, reg snykbroker.Registration) (*tunnelClient, *prometheus.Registry) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	registry := prometheus.NewRegistry()

	cfg.GrpcInsecure = true
	if cfg.TunnelConns == 0 {
		cfg.TunnelConns = 1
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
		config:          cfg,
		logger:          logger.Named("grpc-tunnel"),
		integrationInfo: common.IntegrationInfo{Integration: common.IntegrationGithub},
		registration:    reg,
	}
	tc.router = catchAllRouter(t)
	tc.backend = &stubBackend{statusCode: 200, body: []byte("ok")}

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

// newTestStreamCtx builds a streamCtx suitable for driving startCall directly
// (no live gRPC stream behind it).
func newTestStreamCtx(sendFn sendFunc) *streamCtx {
	_, cancel := context.WithCancel(context.Background())
	return &streamCtx{
		ts:            &tunnelStream{streamID: "test-stream", serverID: "test-server", cancel: cancel},
		sendFn:        sendFn,
		maxFrameBytes: 1024,
	}
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

// collectFrames wires a sendFn that gathers outgoing call frames.
type frameCollector struct {
	mu     sync.Mutex
	frames []*pb.CallFrame
	doneC  chan string // receives call_id when a terminal frame (End/Cancel) arrives
}

func newFrameCollector() *frameCollector {
	return &frameCollector{doneC: make(chan string, 16)}
}

func (fc *frameCollector) sendFn(msg *pb.ClientFrame) error {
	call := msg.GetCall()
	if call == nil {
		return nil
	}
	fc.mu.Lock()
	fc.frames = append(fc.frames, call)
	fc.mu.Unlock()
	switch call.Body.(type) {
	case *pb.CallFrame_End, *pb.CallFrame_Cancel:
		fc.doneC <- call.CallId
	}
	return nil
}

// byCall returns the collected frames for one call id.
func (fc *frameCollector) byCall(id string) []*pb.CallFrame {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []*pb.CallFrame
	for _, f := range fc.frames {
		if f.CallId == id {
			out = append(out, f)
		}
	}
	return out
}

// waitDone waits for n calls to terminate.
func (fc *frameCollector) waitDone(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-fc.doneC:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for call to terminate")
		}
	}
}

// newTestCallTable returns a call table that holds the test open until every
// call goroutine it tracks has actually finished.
//
// A call's last act is table.remove, in the outermost defer of the goroutine
// startCall launches — which, being registered first, runs after runCall's own
// deferred "Request completed" line. So an empty table is the only barrier
// that says nothing will log any more. Waiting on the terminal frame is not
// enough: that log defer runs after the frame is sent.
//
// Without this a test that returns while a call is still in flight panics the
// whole package with "Log in goroutine after <test> has completed" — zaptest
// logging into a finished test. It is intermittent, because it depends on
// whether the goroutine gets scheduled before the test returns.
func newTestCallTable(t *testing.T) *callTable {
	t.Helper()
	table := newCallTable()
	t.Cleanup(func() {
		// Cancel whatever is still running first, so a test that deliberately
		// leaves a slow call in flight does not pay its full delay here.
		// cancelAll empties the map itself and so cannot be the barrier —
		// cancel in place and let each goroutine's own defer do the removing.
		table.mu.Lock()
		for _, c := range table.calls {
			c.cancel()
		}
		table.mu.Unlock()
		waitFor(t, 10*time.Second, func() bool { return !table.active() })
	})
	return table
}

func reqStart(method, path string, timeoutMs int32) *pb.CallStart {
	return &pb.CallStart{
		PseudoHeaders: map[string]string{":method": method, ":path": path},
		TimeoutMs:     timeoutMs,
	}
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

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

	cfg := config.AgentConfig{}
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

	cfg := config.AgentConfig{}
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

	cfg := config.AgentConfig{}
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

	// >= 1, not == 1: connectionsActive counts streams, and this client runs
	// two idle streams per server, so the gauge settles at 2. It passes
	// through 1 on the way, but waitFor polls every 10ms and the two slots
	// connect within a few of each other — in CI they landed 6ms apart, so
	// the poll saw 0 then 2 and the equality never held. What this test is
	// about is that the client connects at all after a refused dial, which
	// is what >= 1 says. TestClose_ShutsDownCleanly already spells it that
	// way against the same gauge.
	waitFor(t, 10*time.Second, func() bool {
		return gaugeVecValue(t, tc.connectionsActive, "delayed-server") >= 1
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
				_ = stream.Send(&pb.ServerFrame{
					Msg: &pb.ServerFrame_Heartbeat{
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

	cfg := config.AgentConfig{}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: addr, tokens: []string{"tok"}})
	startClientWithEnv(t, tc, addr, "tok")
	defer tc.Close()

	waitFor(t, 5*time.Second, func() bool { return streams.Load() >= 3 })
}

// TestInflightCap_QueuesThenRuns: with MaxInflightRequests=1 and a slow
// backend, the 2nd concurrent call queues until capacity frees and then
// completes (no 503 for brief bursts).
func TestInflightCap_QueuesThenRuns(t *testing.T) {
	cfg := config.AgentConfig{
		MaxInflightRequests: 1,
		MaxRequestTimeout:   5 * time.Second,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.backend = &stubBackend{
		statusCode: 200,
		body:       []byte("ok"),
		delay:      300 * time.Millisecond,
	}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "r1", reqStart("GET", "/", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	// Give r1 a head start to acquire the semaphore.
	time.Sleep(20 * time.Millisecond)
	tc.startCall(sc, table, "r2", reqStart("GET", "/", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r2", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	// Both complete: r1 immediately, r2 after queueing behind it.
	fc.waitDone(t, 2)

	for _, id := range []string{"r1", "r2"} {
		frames := fc.byCall(id)
		require.NotNil(t, frames[0].GetStart(), "%s should have started a response", id)
		require.Equal(t, "200", frames[0].GetStart().PseudoHeaders[":status"], id)
	}
	require.Equal(t, float64(0), counterValue(t, tc.requestsRejected))
}

// TestInflightCap_QueueTimeout: a queued call whose deadline expires before
// capacity frees fails with CallCancel(503).
func TestInflightCap_QueueTimeout(t *testing.T) {
	cfg := config.AgentConfig{
		MaxInflightRequests: 1,
		MaxRequestTimeout:   5 * time.Second,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.backend = &stubBackend{
		statusCode: 200,
		body:       []byte("ok"),
		delay:      2 * time.Second,
		respectCtx: true,
	}

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "r1", reqStart("GET", "/", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	time.Sleep(20 * time.Millisecond)
	// r2 has a 100ms budget; r1 holds the only slot for 2s.
	tc.startCall(sc, table, "r2", reqStart("GET", "/", 100))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r2", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	// r2 times out queued.
	fc.waitDone(t, 1)
	r2frames := fc.byCall("r2")
	require.Len(t, r2frames, 1)
	cancel := r2frames[0].GetCancel()
	require.NotNil(t, cancel, "expected CallCancel for r2")
	require.Equal(t, int32(503), cancel.Code)
	require.Contains(t, cancel.Reason, "in-flight capacity")
	require.Equal(t, float64(1), counterValue(t, tc.requestsRejected))
}

// TestMaxRequestTimeout_AppliesWhenTimeoutMsZero: with TimeoutMs=0 and
// MaxRequestTimeout=200ms, a 5-second backend should return deadline exceeded
// as a CallCancel(502).
func TestMaxRequestTimeout_AppliesWhenTimeoutMsZero(t *testing.T) {
	cfg := config.AgentConfig{
		MaxInflightRequests: 4,
		MaxRequestTimeout:   200 * time.Millisecond,
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})

	stub := &stubBackend{
		statusCode: 200,
		body:       []byte("ok"),
		delay:      5 * time.Second,
		respectCtx: true,
	}
	tc.backend = stub

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	start := time.Now()
	tc.startCall(sc, table, "r1", reqStart("GET", "/", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "r1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	fc.waitDone(t, 1)
	require.WithinDuration(t, start.Add(200*time.Millisecond), time.Now(), 1*time.Second)

	frames := fc.byCall("r1")
	require.Len(t, frames, 1)
	cancel := frames[0].GetCancel()
	require.NotNil(t, cancel, "expected CallCancel on timeout")
	require.Equal(t, int32(502), cancel.Code)

	// Verify the stub saw a context with a short deadline (well under 1s).
	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Less(t, stub.lastTimeout, 1*time.Second, "backend ctx had deadline %v", stub.lastTimeout)
}

// TestCall_StreamedRequestBody: request body chunks delivered over multiple
// Data frames reach the backend intact and in order.
func TestCall_StreamedRequestBody(t *testing.T) {
	cfg := config.AgentConfig{MaxInflightRequests: 4}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})

	var gotBody []byte
	var mu sync.Mutex
	tc.backend = backendFunc(func(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		gotBody = b
		mu.Unlock()
		return &BackendResponse{
			StatusCode: 201,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Trailer:    func() http.Header { return nil },
		}, nil
	})

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("POST", "/upload", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("part-1;")}}})
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_Data{Data: &pb.CallData{Payload: []byte("part-2")}}})
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	fc.waitDone(t, 1)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "part-1;part-2", string(gotBody))

	frames := fc.byCall("c1")
	require.NotNil(t, frames[0].GetStart())
	require.Equal(t, "201", frames[0].GetStart().PseudoHeaders[":status"])
}

// TestCall_ServerCancelAbortsBackend: a CallCancel from the server aborts the
// in-flight backend request.
func TestCall_ServerCancelAbortsBackend(t *testing.T) {
	cfg := config.AgentConfig{MaxInflightRequests: 4, MaxRequestTimeout: 30 * time.Second}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})

	backendDone := make(chan error, 1)
	tc.backend = backendFunc(func(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
		<-ctx.Done()
		backendDone <- ctx.Err()
		return nil, ctx.Err()
	})

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/slow", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})

	// Cancel from the server side.
	time.Sleep(50 * time.Millisecond)
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_Cancel{Cancel: &pb.CallCancel{Reason: "caller gone"}}})

	select {
	case err := <-backendDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("backend was not cancelled")
	}
}

// TestCACertLoadFailure_NoSilentDowngrade: an invalid CA cert path must
// prevent startup, not silently fall back to insecure.
func TestCACertLoadFailure_NoSilentDowngrade(t *testing.T) {
	cfg := config.AgentConfig{
		HttpCaCertFilePath: filepath.Join(t.TempDir(), "missing-ca.pem"),
	}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.config.GrpcInsecure = false // force TLS path

	_, err := tc.buildTransportCredentials()
	require.Error(t, err, "missing CA cert must error, not silently downgrade")
	require.Contains(t, err.Error(), "read CA cert")
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
		onStream:            nil,
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

	cfg := config.AgentConfig{}
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

	cfg := config.AgentConfig{}
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

// backendFunc adapts a function to the Backend interface.
type backendFunc func(ctx context.Context, req *BackendRequest) (*BackendResponse, error)

func (f backendFunc) Do(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
	return f(ctx, req)
}

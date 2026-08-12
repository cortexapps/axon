package grpctunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/requestexecutor"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

const (
	maxChunkSize              = 1024 * 1024
	maxGrpcMsgSize            = 8 * 1024 * 1024
	defaultMaxPending         = 64
	handshakeTimeout          = 30 * time.Second
	keepaliveInterval         = 20 * time.Second
	keepaliveTimeout          = 10 * time.Second
	minBackoff                = time.Second
	maxBackoffDuration        = 30 * time.Second
	forcedReregisterRateLimit = time.Minute
	failuresBeforeReregister  = 5
)

var errServerCapHit = errors.New("server-id stream cap reached")

// connectError tags an error from the initial connection establishment with
// the phase in which it occurred, so metrics can break down where things fail.
type connectError struct {
	phase string
	cause error
}

func (e *connectError) Error() string { return e.phase + ": " + e.cause.Error() }
func (e *connectError) Unwrap() error { return e.cause }

func newConnectErr(phase string, cause error) *connectError {
	return &connectError{phase: phase, cause: cause}
}

// tunnelClient implements the snykbroker.RelayInstanceManager interface
// using gRPC bidirectional streaming instead of snyk-broker.
type tunnelClient struct {
	config          config.AgentConfig
	logger          *zap.Logger
	integrationInfo common.IntegrationInfo
	registration    snykbroker.Registration
	executor        requestexecutor.RequestExecutor
	httpClient      *http.Client

	running   atomic.Bool
	parentCtx context.Context
	cancelAll context.CancelFunc

	mu                 sync.Mutex
	slots              []*tunnelStream
	serverStreamCounts map[string]int
	currentToken       string
	currentServerAddr  string

	// restartMu serializes Restart() so two concurrent calls do not race the
	// Close→Start handoff. Held only across the Close+Start; never with mu.
	restartMu sync.Mutex

	// registerMu serializes Register() calls and protects lastRegisterAt.
	registerMu     sync.Mutex
	lastRegisterAt time.Time

	consecFailures atomic.Int32

	// inflightSem caps concurrent in-flight requests. nil means unbounded.
	inflightSem chan struct{}

	wg sync.WaitGroup

	// Metrics
	connectionsActive      *prometheus.GaugeVec
	requestsTotal          *prometheus.CounterVec
	requestsInflight       prometheus.Gauge
	requestsRejected       prometheus.Counter
	reconnectsTotal        *prometheus.CounterVec
	requestDuration        *prometheus.HistogramVec
	heartbeatTimeoutsTotal *prometheus.CounterVec
	tokenRotationsTotal    prometheus.Counter
	connectErrorsTotal     *prometheus.CounterVec
}

type tunnelStream struct {
	index    int
	streamID string
	serverID string
	conn     *grpc.ClientConn
	cancel   context.CancelFunc
}

// streamCtx bundles the per-stream values needed to run streamLoop after a
// successful handshake.
type streamCtx struct {
	ts     *tunnelStream
	stream pb.TunnelService_TunnelClient
	sendFn sendFunc
	hbMs   int32
}

// sendFunc serializes Send() calls on a gRPC stream. Multiple goroutines
// (heartbeat responses, HTTP response handlers) may send concurrently; the
// mutex prevents data races. On a Send error the func cancels the stream so
// the recv loop notices and reconnects.
type sendFunc func(msg *pb.TunnelClientMessage) error

type requestAssembler struct {
	mu         sync.Mutex
	pending    map[string]*pendingRequest
	order      []string // insertion order for LRU eviction
	maxPending int
	logger     *zap.Logger
}

type pendingRequest struct {
	method    string
	path      string
	headers   map[string]string
	body      []byte
	timeoutMs int32
}

func newRequestAssembler(maxPending int, logger *zap.Logger) *requestAssembler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &requestAssembler{
		pending:    make(map[string]*pendingRequest),
		maxPending: maxPending,
		logger:     logger,
	}
}

// handleChunk processes an incoming HttpRequest chunk. It returns a fully
// assembled *pb.HttpRequest when the final chunk arrives, or nil if more
// chunks are still expected.
func (ra *requestAssembler) handleChunk(chunk *pb.HttpRequest) *pb.HttpRequest {
	if chunk.ChunkIndex == 0 && chunk.IsFinal {
		return chunk
	}

	ra.mu.Lock()
	defer ra.mu.Unlock()

	if chunk.ChunkIndex == 0 {
		if ra.maxPending > 0 && len(ra.pending) >= ra.maxPending {
			evicted := ra.order[0]
			ra.order = ra.order[1:]
			delete(ra.pending, evicted)
			ra.logger.Warn("Request assembler at cap; evicting oldest pending",
				zap.String("evictedRequestId", evicted),
				zap.Int("maxPending", ra.maxPending),
			)
		}
		ra.pending[chunk.RequestId] = &pendingRequest{
			method:    chunk.Method,
			path:      chunk.Path,
			headers:   chunk.Headers,
			body:      append([]byte(nil), chunk.Body...),
			timeoutMs: chunk.TimeoutMs,
		}
		ra.order = append(ra.order, chunk.RequestId)
		return nil
	}

	pr, ok := ra.pending[chunk.RequestId]
	if !ok {
		return nil
	}
	pr.body = append(pr.body, chunk.Body...)
	if !chunk.IsFinal {
		return nil
	}
	delete(ra.pending, chunk.RequestId)
	for i, id := range ra.order {
		if id == chunk.RequestId {
			ra.order = append(ra.order[:i], ra.order[i+1:]...)
			break
		}
	}
	return &pb.HttpRequest{
		RequestId: chunk.RequestId,
		Method:    pr.method,
		Path:      pr.path,
		Headers:   pr.headers,
		Body:      pr.body,
		TimeoutMs: pr.timeoutMs,
		IsFinal:   true,
	}
}

func (ra *requestAssembler) discardAll() {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	ra.pending = make(map[string]*pendingRequest)
	ra.order = nil
}

type TunnelClientParams struct {
	fx.In
	Lifecycle       fx.Lifecycle `optional:"true"`
	Config          config.AgentConfig
	Logger          *zap.Logger
	IntegrationInfo common.IntegrationInfo
	HttpServer      cortexHttp.Server
	Registration    snykbroker.Registration
	HttpClient      *http.Client         `optional:"true"`
	Registry        *prometheus.Registry `optional:"true"`
}

func NewTunnelClient(p TunnelClientParams) snykbroker.RelayInstanceManager {
	httpClient := p.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		}
	}

	tc := &tunnelClient{
		config:             p.Config,
		logger:             p.Logger.Named("grpc-tunnel"),
		integrationInfo:    p.IntegrationInfo,
		registration:       p.Registration,
		httpClient:         httpClient,
		serverStreamCounts: make(map[string]int),
	}

	if p.Config.MaxInflightRequests > 0 {
		tc.inflightSem = make(chan struct{}, p.Config.MaxInflightRequests)
	}

	tc.connectionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "grpc_tunnel_connections_active", Help: "Active gRPC tunnel streams"},
		[]string{"server_id"},
	)
	tc.requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_requests_total", Help: "Total requests dispatched through gRPC tunnel"},
		[]string{"method", "status"},
	)
	tc.requestsInflight = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "grpc_tunnel_requests_inflight", Help: "Requests currently being dispatched"},
	)
	tc.requestsRejected = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "grpc_tunnel_request_rejected_total", Help: "Requests rejected by in-flight cap"},
	)
	tc.reconnectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_reconnects_total", Help: "Total tunnel reconnection attempts"},
		[]string{"server_id"},
	)
	tc.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_tunnel_request_duration_ms",
			Help:    "Request execution latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 12),
		},
		[]string{"method"},
	)
	tc.heartbeatTimeoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_heartbeat_timeouts_total", Help: "Heartbeat timeouts that triggered reconnect"},
		[]string{"server_id"},
	)
	tc.tokenRotationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "grpc_tunnel_token_rotations_total", Help: "Re-registrations that produced a new token"},
	)
	tc.connectErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "grpc_tunnel_connect_errors_total", Help: "Connect-phase errors"},
		[]string{"phase"},
	)

	p.HttpServer.RegisterHandler(tc)

	if p.Registry != nil {
		p.Registry.MustRegister(
			tc.connectionsActive,
			tc.requestsTotal,
			tc.requestsInflight,
			tc.requestsRejected,
			tc.reconnectsTotal,
			tc.requestDuration,
			tc.heartbeatTimeoutsTotal,
			tc.tokenRotationsTotal,
			tc.connectErrorsTotal,
		)
	}

	if p.Lifecycle != nil {
		p.Lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error { return tc.Start() },
			OnStop:  func(ctx context.Context) error { return tc.Close() },
		})
	}

	return tc
}

// RegisterRoutes implements cortexHttp.Handler. Exposes /restart and
// /systemcheck under the broker path prefix to preserve the snyk-broker URL
// shape.
func (tc *tunnelClient) RegisterRoutes(mux *mux.Router) error {
	sub := mux.PathPrefix(fmt.Sprintf("%s/broker", cortexHttp.AxonPathRoot)).Subrouter()
	sub.HandleFunc("/restart", tc.handleRestart)
	sub.HandleFunc("/systemcheck", tc.handleSystemCheck)
	return nil
}

func (tc *tunnelClient) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

func (tc *tunnelClient) handleRestart(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := tc.Restart(); err != nil {
		tc.logger.Error("Restart failed", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (tc *tunnelClient) handleSystemCheck(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	tc.mu.Lock()
	active := 0
	for _, s := range tc.slots {
		if s != nil {
			active++
		}
	}
	tc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","relay_mode":"grpc-tunnel","streams":%d}`, active)
}

func (tc *tunnelClient) Start() error {
	if !tc.running.CompareAndSwap(false, true) {
		return fmt.Errorf("already started")
	}
	tc.mu.Lock()
	tc.slots = make([]*tunnelStream, tc.config.TunnelCount)
	tc.serverStreamCounts = make(map[string]int)
	ctx, cancel := context.WithCancel(context.Background())
	tc.parentCtx = ctx
	tc.cancelAll = cancel
	tc.mu.Unlock()

	go tc.startAsync()
	return nil
}

func (tc *tunnelClient) startAsync() {
	// Allow tests to pre-populate the executor and skip accept-file rendering.
	if tc.executor == nil {
		if err := tc.setupExecutor(); err != nil {
			tc.logger.Error("Failed to set up request executor", zap.Error(err))
			return
		}
	}

	if err := tc.initialRegister(); err != nil {
		tc.logger.Warn("Initial registration aborted", zap.Error(err))
		return
	}

	// Refuse to start if TLS is configured but the CA cert is unreadable.
	// buildTransportCredentials is also called inside each dial; we call it
	// here once eagerly to surface fatal config issues at startup.
	if _, err := tc.buildTransportCredentials(); err != nil {
		tc.logger.Error("Transport credentials invalid; refusing to start tunnel", zap.Error(err))
		return
	}

	for i := 0; i < tc.config.TunnelCount; i++ {
		tc.wg.Add(1)
		go tc.manageStream(i)
	}

	tc.wg.Add(1)
	go tc.periodicReregister()

	tc.logger.Info("gRPC tunnel started",
		zap.Int("tunnelCount", tc.config.TunnelCount),
		zap.Int("maxStreamsPerServer", tc.config.MaxStreamsPerServer),
		zap.Int("maxInflightRequests", tc.config.MaxInflightRequests),
		zap.Duration("maxRequestTimeout", tc.config.MaxRequestTimeout),
	)
}

func (tc *tunnelClient) setupExecutor() error {
	af, err := tc.integrationInfo.ToAcceptFile(tc.config, tc.logger)
	if err != nil {
		return fmt.Errorf("error creating accept file: %w", err)
	}
	rendered, err := af.Render(tc.logger)
	if err != nil {
		return fmt.Errorf("error rendering accept file: %w", err)
	}
	af2, err := acceptfile.NewAcceptFile(rendered, tc.config, tc.logger)
	if err != nil {
		return fmt.Errorf("error parsing rendered accept file: %w", err)
	}
	rules := af2.Wrapper().PrivateRules()
	tc.executor = requestexecutor.NewRequestExecutor(rules, tc.httpClient, tc.logger)
	return nil
}

// initialRegister establishes the initial server address + token, either from
// env vars (BROKER_SERVER_URL + BROKER_TOKEN) or by registering with the Cortex
// API. Retries with backoff until success or shutdown.
func (tc *tunnelClient) initialRegister() error {
	envServerURL := os.Getenv("BROKER_SERVER_URL")
	envToken := os.Getenv("BROKER_TOKEN")

	if envServerURL != "" && envToken != "" {
		tc.logger.Info("Using direct connection config (skipping registration)",
			zap.String("serverUrl", envServerURL),
		)
		tc.mu.Lock()
		tc.currentToken = envToken
		tc.currentServerAddr = stripScheme(envServerURL)
		tc.mu.Unlock()
		return nil
	}

	backoff := tc.config.FailWaitTime
	if backoff <= 0 {
		backoff = time.Second
	}

	for tc.running.Load() && tc.parentCtx.Err() == nil {
		regInfo, err := tc.registration.Register(tc.integrationInfo.Integration, tc.integrationInfo.Alias)
		if err != nil {
			tc.logger.Error("Registration failed, retrying",
				zap.Error(err), zap.Duration("backoff", backoff))
			select {
			case <-time.After(backoff):
			case <-tc.parentCtx.Done():
				return tc.parentCtx.Err()
			}
			backoff = nextBackoff(backoff)
			continue
		}

		tc.logger.Info("Registered with Cortex API", zap.String("serverUri", regInfo.ServerUri))

		serverAddr := regInfo.ServerUri
		if envServerURL != "" {
			serverAddr = envServerURL
		}

		tc.mu.Lock()
		tc.currentToken = regInfo.Token
		tc.currentServerAddr = stripScheme(serverAddr)
		tc.mu.Unlock()

		tc.registerMu.Lock()
		tc.lastRegisterAt = time.Now()
		tc.registerMu.Unlock()

		return nil
	}
	return tc.parentCtx.Err()
}

// periodicReregister wakes every RegistrationRefreshInterval and re-fetches
// the token. On change, it cycles all streams.
func (tc *tunnelClient) periodicReregister() {
	defer tc.wg.Done()

	if tc.config.RegistrationRefreshInterval <= 0 {
		return
	}

	ticker := time.NewTicker(tc.config.RegistrationRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-tc.parentCtx.Done():
			return
		case <-ticker.C:
			tc.reregister("periodic", false)
		}
	}
}

// reregister calls Cortex Registration.Register() and updates currentToken /
// currentServerAddr. When force=true, the call is rate-limited so an auth-error
// storm doesn't hammer Cortex.
func (tc *tunnelClient) reregister(reason string, force bool) {
	envServerURL := os.Getenv("BROKER_SERVER_URL")
	envToken := os.Getenv("BROKER_TOKEN")
	if envServerURL != "" && envToken != "" {
		// Direct-config mode — nothing to refresh.
		return
	}

	tc.registerMu.Lock()
	defer tc.registerMu.Unlock()

	if force && time.Since(tc.lastRegisterAt) < forcedReregisterRateLimit {
		return
	}

	regInfo, err := tc.registration.Register(tc.integrationInfo.Integration, tc.integrationInfo.Alias)
	if err != nil {
		tc.logger.Warn("Re-registration failed", zap.String("reason", reason), zap.Error(err))
		return
	}
	tc.lastRegisterAt = time.Now()

	serverAddr := regInfo.ServerUri
	if envServerURL != "" {
		serverAddr = envServerURL
	}

	tc.mu.Lock()
	oldToken := tc.currentToken
	tc.currentToken = regInfo.Token
	tc.currentServerAddr = stripScheme(serverAddr)
	tc.mu.Unlock()

	if oldToken != regInfo.Token {
		tc.tokenRotationsTotal.Inc()
		tc.logger.Info("Token rotated; cycling streams", zap.String("reason", reason))
		tc.cycleAllStreams()
	}
}

func (tc *tunnelClient) cycleAllStreams() {
	tc.mu.Lock()
	streams := make([]*tunnelStream, 0, len(tc.slots))
	for _, s := range tc.slots {
		if s != nil {
			streams = append(streams, s)
		}
	}
	tc.mu.Unlock()
	for _, s := range streams {
		s.cancel()
	}
}

func (tc *tunnelClient) getCurrentConfig() (token, serverAddr string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.currentToken, tc.currentServerAddr
}

func (tc *tunnelClient) acquireServerSlot(serverID string) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	c := tc.config.MaxStreamsPerServer
	if c > 0 && tc.serverStreamCounts[serverID] >= c {
		return false
	}
	tc.serverStreamCounts[serverID]++
	return true
}

func (tc *tunnelClient) releaseServerSlot(serverID string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.serverStreamCounts[serverID] > 0 {
		tc.serverStreamCounts[serverID]--
	}
}

func (tc *tunnelClient) setSlot(index int, ts *tunnelStream) {
	tc.mu.Lock()
	tc.slots[index] = ts
	tc.mu.Unlock()
}

func (tc *tunnelClient) clearSlot(index int, ts *tunnelStream) {
	tc.mu.Lock()
	if index < len(tc.slots) && tc.slots[index] == ts {
		tc.slots[index] = nil
	}
	tc.mu.Unlock()
	tc.releaseServerSlot(ts.serverID)
	tc.connectionsActive.WithLabelValues(ts.serverID).Dec()
}

// manageStream owns one tunnel slot and keeps a stream open for its lifetime.
// Unifies initial-connect, normal recv-loop, and reconnect into a single retry
// loop so first-connect failures recover the same way as mid-life ones.
func (tc *tunnelClient) manageStream(index int) {
	defer tc.wg.Done()

	backoff := minBackoff
	first := true

	for tc.running.Load() && tc.parentCtx.Err() == nil {
		// Apply backoff before retries (not before the first attempt).
		if !first {
			tc.reconnectsTotal.WithLabelValues("").Inc()
			select {
			case <-time.After(jittered(backoff)):
			case <-tc.parentCtx.Done():
				return
			}
			backoff = nextBackoff(backoff)
		}
		first = false

		err := tc.runOneStream(index)
		if err == nil || tc.parentCtx.Err() != nil {
			// Clean shutdown or no error path (shouldn't really happen — runOneStream
			// returns an error whenever the stream ends).
			return
		}

		// Classify error and react.
		var ce *connectError
		if errors.As(err, &ce) {
			tc.connectErrorsTotal.WithLabelValues(ce.phase).Inc()
		}

		// Server-cap hit: just back off and try again (LB may give a different instance).
		if errors.Is(err, errServerCapHit) {
			tc.logger.Debug("Server cap hit; retrying", zap.Int("index", index))
			continue
		}

		if status.Code(err) == codes.Unauthenticated || (ce != nil && status.Code(ce.cause) == codes.Unauthenticated) {
			tc.logger.Warn("Auth failure; forcing re-registration",
				zap.Int("index", index), zap.Error(err))
			tc.reregister("unauthenticated", true)
			tc.consecFailures.Store(0)
			// keep backoff short for auth recovery
			backoff = minBackoff
			continue
		}

		if tc.consecFailures.Add(1) >= failuresBeforeReregister {
			tc.logger.Warn("Repeated open failures; forcing re-registration",
				zap.Int("index", index), zap.Int32("consecutive", tc.consecFailures.Load()))
			tc.reregister("repeated-failures", true)
			tc.consecFailures.Store(0)
		}

		tc.logger.Warn("Tunnel slot stream ended; will retry",
			zap.Int("index", index), zap.Error(err), zap.Duration("nextBackoff", backoff))
	}
}

// runOneStream opens a fresh ClientConn, handshakes, and runs streamLoop until
// the stream ends. Returns the terminating error (or nil on clean shutdown).
func (tc *tunnelClient) runOneStream(index int) error {
	sc, err := tc.openSlot(index)
	if err != nil {
		return err
	}
	tc.setSlot(index, sc.ts)
	defer func() {
		tc.clearSlot(index, sc.ts)
		sc.ts.cancel()
		if sc.ts.conn != nil {
			sc.ts.conn.Close()
		}
	}()

	// Success — reset the global failure counter.
	tc.consecFailures.Store(0)

	return tc.streamLoop(sc)
}

// openSlot dials, performs the gRPC handshake, and acquires a server-id slot.
// Returns errServerCapHit if the slot cap is exceeded for the server we landed on.
func (tc *tunnelClient) openSlot(index int) (*streamCtx, error) {
	token, serverAddr := tc.getCurrentConfig()
	if token == "" || serverAddr == "" {
		return nil, newConnectErr("config", errors.New("no registration token/address"))
	}

	dialOpts, dialAddr, err := tc.buildDialOptions(serverAddr)
	if err != nil {
		return nil, newConnectErr("dial_opts", err)
	}

	conn, err := grpc.NewClient(dialAddr, dialOpts...)
	if err != nil {
		return nil, newConnectErr("dial", err)
	}

	streamCtxParent, cancel := context.WithCancel(tc.parentCtx)

	// Abort handshake if it stalls.
	handshakeTimer := time.AfterFunc(handshakeTimeout, cancel)

	client := pb.NewTunnelServiceClient(conn)
	stream, err := client.Tunnel(streamCtxParent)
	if err != nil {
		handshakeTimer.Stop()
		cancel()
		conn.Close()
		return nil, newConnectErr("open_stream", err)
	}

	hello := &pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_Hello{
			Hello: &pb.ClientHello{
				BrokerToken:    token,
				ClientVersion:  common.ClientVersion,
				TenantId:       os.Getenv("CORTEX_TENANT_ID"),
				Integration:    tc.integrationInfo.Integration.String(),
				Alias:          tc.integrationInfo.Alias,
				InstanceId:     tc.config.InstanceId,
				CortexApiToken: tc.config.CortexApiToken,
			},
		},
	}

	if err := stream.Send(hello); err != nil {
		handshakeTimer.Stop()
		cancel()
		conn.Close()
		return nil, newConnectErr("send_hello", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		handshakeTimer.Stop()
		cancel()
		conn.Close()
		return nil, newConnectErr("recv_hello", err)
	}
	handshakeTimer.Stop()

	serverHello := msg.GetHello()
	if serverHello == nil {
		cancel()
		conn.Close()
		return nil, newConnectErr("recv_hello", errors.New("expected ServerHello"))
	}

	if !tc.acquireServerSlot(serverHello.ServerId) {
		tc.logger.Info("Server stream cap reached; will retry to land on a different instance",
			zap.String("serverId", serverHello.ServerId),
			zap.Int("index", index),
			zap.Int("cap", tc.config.MaxStreamsPerServer),
		)
		cancel()
		conn.Close()
		return nil, errServerCapHit
	}

	ts := &tunnelStream{
		index:    index,
		streamID: serverHello.StreamId,
		serverID: serverHello.ServerId,
		conn:     conn,
		cancel:   cancel,
	}

	tc.connectionsActive.WithLabelValues(ts.serverID).Inc()
	tc.logger.Info("Tunnel stream established",
		zap.String("streamId", ts.streamID),
		zap.String("serverId", ts.serverID),
		zap.Int32("heartbeatIntervalMs", serverHello.HeartbeatIntervalMs),
		zap.Int("index", index),
	)

	return &streamCtx{
		ts:     ts,
		stream: stream,
		sendFn: tc.makeSendFunc(ts, stream),
		hbMs:   serverHello.HeartbeatIntervalMs,
	}, nil
}

// makeSendFunc wraps stream.Send with a mutex (so multiple goroutines can call
// it safely) and a one-shot cancel that fires on Send error. The cancel makes
// streamLoop's next Recv return, which propagates to reconnect — preventing
// the half-broken zombie-stream state we used to get on partial-response failures.
func (tc *tunnelClient) makeSendFunc(ts *tunnelStream, stream pb.TunnelService_TunnelClient) sendFunc {
	var (
		sendMu     sync.Mutex
		cancelOnce sync.Once
	)
	return func(msg *pb.TunnelClientMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		err := stream.Send(msg)
		if err != nil {
			cancelOnce.Do(func() {
				tc.logger.Warn("Send error; cancelling stream for reconnect",
					zap.String("streamId", ts.streamID),
					zap.Error(err),
				)
				ts.cancel()
			})
		}
		return err
	}
}

func (tc *tunnelClient) streamLoop(sc *streamCtx) error {
	assembler := newRequestAssembler(defaultMaxPending, tc.logger)
	defer assembler.discardAll()

	var heartbeatTimer *time.Timer
	if sc.hbMs > 0 {
		timeout := 2 * time.Duration(sc.hbMs) * time.Millisecond
		heartbeatTimer = time.AfterFunc(timeout, func() {
			tc.logger.Warn("Heartbeat timeout; cancelling stream",
				zap.String("serverId", sc.ts.serverID),
				zap.String("streamId", sc.ts.streamID),
				zap.Duration("timeout", timeout),
			)
			tc.heartbeatTimeoutsTotal.WithLabelValues(sc.ts.serverID).Inc()
			sc.ts.cancel()
		})
		defer heartbeatTimer.Stop()
	}

	for {
		msg, err := sc.stream.Recv()
		if err != nil {
			return err
		}

		if heartbeatTimer != nil {
			heartbeatTimer.Reset(2 * time.Duration(sc.hbMs) * time.Millisecond)
		}

		switch m := msg.Message.(type) {
		case *pb.TunnelServerMessage_Heartbeat:
			if err := sc.sendFn(&pb.TunnelClientMessage{
				Message: &pb.TunnelClientMessage_Heartbeat{
					Heartbeat: &pb.Heartbeat{TimestampMs: time.Now().UnixMilli()},
				},
			}); err != nil {
				// makeSendFunc has already cancelled the stream; the next Recv
				// will exit. No need to log again here.
				_ = err
			}
		case *pb.TunnelServerMessage_HttpRequest:
			assembled := assembler.handleChunk(m.HttpRequest)
			if assembled != nil {
				go tc.handleRequest(sc.sendFn, assembled)
			}
		case *pb.TunnelServerMessage_Hello:
			tc.logger.Warn("Unexpected ServerHello after handshake")
		}
	}
}

func (tc *tunnelClient) handleRequest(sendFn sendFunc, req *pb.HttpRequest) {
	// Non-blocking acquire on the in-flight semaphore. If full, reject fast
	// rather than queueing requests that could OOM the agent during a
	// downstream incident.
	if tc.inflightSem != nil {
		select {
		case tc.inflightSem <- struct{}{}:
			tc.requestsInflight.Inc()
			defer func() {
				<-tc.inflightSem
				tc.requestsInflight.Dec()
			}()
		default:
			tc.requestsRejected.Inc()
			tc.requestsTotal.WithLabelValues(req.Method, "503").Inc()
			tc.sendErrorResponse(sendFn, req.RequestId, 503, "agent at in-flight cap")
			return
		}
	}

	if tc.executor == nil {
		tc.sendErrorResponse(sendFn, req.RequestId, 503, "executor not ready")
		return
	}

	start := time.Now()

	// Always cap the request: the server's TimeoutMs is honored when smaller,
	// but MaxRequestTimeout prevents a buggy/missing server timeout from
	// leaking goroutines indefinitely against slow downstreams.
	maxTimeout := tc.config.MaxRequestTimeout
	if maxTimeout <= 0 {
		maxTimeout = 5 * time.Minute
	}
	timeout := maxTimeout
	if req.TimeoutMs > 0 && time.Duration(req.TimeoutMs)*time.Millisecond < maxTimeout {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := tc.executor.Execute(ctx, req.Method, req.Path, req.Headers, req.Body)
	if err != nil {
		tc.logger.Error("Request execution failed",
			zap.String("requestId", req.RequestId),
			zap.String("method", req.Method),
			zap.String("path", req.Path),
			zap.Error(err),
		)
		statusCode := 502
		if err == requestexecutor.ErrNoMatchingRule {
			statusCode = 404
		}
		tc.requestsTotal.WithLabelValues(req.Method, fmt.Sprintf("%d", statusCode)).Inc()
		tc.sendErrorResponse(sendFn, req.RequestId, int32(statusCode), err.Error())
		return
	}

	duration := time.Since(start)
	tc.requestsTotal.WithLabelValues(req.Method, fmt.Sprintf("%d", resp.StatusCode)).Inc()
	tc.requestDuration.WithLabelValues(req.Method).Observe(float64(duration.Milliseconds()))

	tc.sendResponse(sendFn, req.RequestId, resp)
}

func (tc *tunnelClient) sendResponse(sendFn sendFunc, requestID string, resp *requestexecutor.ExecutorResponse) {
	for i := 0; i < len(resp.Body); i += maxChunkSize {
		end := i + maxChunkSize
		if end > len(resp.Body) {
			end = len(resp.Body)
		}
		chunkIndex := int32(i / maxChunkSize)

		httpResp := &pb.HttpResponse{
			RequestId:  requestID,
			Body:       resp.Body[i:end],
			ChunkIndex: chunkIndex,
			IsFinal:    end == len(resp.Body),
		}
		if chunkIndex == 0 {
			httpResp.StatusCode = int32(resp.StatusCode)
			httpResp.Headers = resp.Headers
		}

		if err := sendFn(&pb.TunnelClientMessage{
			Message: &pb.TunnelClientMessage_HttpResponse{HttpResponse: httpResp},
		}); err != nil {
			tc.logger.Warn("Failed to send response chunk; aborting remaining chunks",
				zap.String("requestId", requestID),
				zap.Int32("chunkIndex", chunkIndex),
				zap.Error(err),
			)
			return
		}
	}
}

func (tc *tunnelClient) sendErrorResponse(sendFn sendFunc, requestID string, statusCode int32, message string) {
	_ = sendFn(&pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_HttpResponse{
			HttpResponse: &pb.HttpResponse{
				RequestId:        requestID,
				StatusCode:       statusCode,
				Headers:          map[string]string{"Content-Type": "text/plain"},
				Body:             []byte(message),
				ChunkIndex:       0,
				IsFinal:          true,
				IsFailedDispatch: true,
			},
		},
	})
}

func (tc *tunnelClient) Restart() error {
	tc.logger.Info("Restarting gRPC tunnel")
	tc.restartMu.Lock()
	defer tc.restartMu.Unlock()
	if err := tc.Close(); err != nil {
		tc.logger.Error("Error closing tunnel on restart", zap.Error(err))
	}
	return tc.Start()
}

func (tc *tunnelClient) Close() error {
	if !tc.running.CompareAndSwap(true, false) {
		return nil
	}

	tc.mu.Lock()
	cancel := tc.cancelAll
	streams := make([]*tunnelStream, 0, len(tc.slots))
	for _, s := range tc.slots {
		if s != nil {
			streams = append(streams, s)
		}
	}
	tc.cancelAll = nil
	tc.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, s := range streams {
		s.cancel()
	}

	tc.wg.Wait()

	tc.mu.Lock()
	tc.slots = nil
	tc.serverStreamCounts = make(map[string]int)
	tc.mu.Unlock()

	tc.logger.Info("gRPC tunnel closed")
	return nil
}

func (tc *tunnelClient) buildDialOptions(targetAddr string) ([]grpc.DialOption, string, error) {
	creds, err := tc.buildTransportCredentials()
	if err != nil {
		return nil, "", err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepaliveInterval,
			Timeout:             keepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxGrpcMsgSize),
			grpc.MaxCallSendMsgSize(maxGrpcMsgSize),
		),
	}

	dialAddr := targetAddr
	if proxyURL := proxyURLFromEnv(targetAddr, tc.config.GrpcInsecure); proxyURL != nil {
		tc.logger.Info("Using HTTP proxy for gRPC connection",
			zap.String("proxy", proxyURL.Host),
			zap.String("target", targetAddr),
		)
		opts = append(opts, grpc.WithContextDialer(newProxyDialer(proxyURL, tc.logger)))
		// Use passthrough scheme to skip local DNS resolution; the custom dialer
		// will let the proxy resolve the hostname instead.
		dialAddr = "passthrough:///" + targetAddr
	}
	return opts, dialAddr, nil
}

// buildTransportCredentials returns the gRPC transport credentials. If a CA
// cert is configured but can't be read or parsed, it returns an error rather
// than silently downgrading to plaintext.
func (tc *tunnelClient) buildTransportCredentials() (credentials.TransportCredentials, error) {
	if tc.config.GrpcInsecure {
		return insecure.NewCredentials(), nil
	}

	tlsConfig := &tls.Config{}

	if tc.config.HttpCaCertFilePath != "" {
		caCert, err := os.ReadFile(tc.config.HttpCaCertFilePath)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %q: %w", tc.config.HttpCaCertFilePath, err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("CA cert %q contains no parseable certificates", tc.config.HttpCaCertFilePath)
		}
		tlsConfig.RootCAs = caCertPool
	}

	return credentials.NewTLS(tlsConfig), nil
}

func stripScheme(addr string) string {
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimPrefix(addr, "http://")
	return addr
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoffDuration {
		d = maxBackoffDuration
	}
	return d
}

// jittered adds ±20% jitter to d so reconnect storms from many agents don't
// align on the same wall-clock instants.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	n := int64(d) / 5
	if n <= 0 {
		return d
	}
	j := rand.Int64N(2*n+1) - n
	return d + time.Duration(j)
}

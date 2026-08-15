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
	handshakeTimeout          = 30 * time.Second
	keepaliveInterval         = 20 * time.Second
	keepaliveTimeout          = 10 * time.Second
	minBackoff                = time.Second
	maxBackoffDuration        = 30 * time.Second
	forcedReregisterRateLimit = time.Minute
	failuresBeforeReregister  = 5

	// idleStreamsPerServer is how many streams are kept idle and ready on
	// each server instance the agent is connected to. It sets how large a
	// burst is absorbed without waiting for a stream to open — a latency
	// property, not a capacity one, which is why it is a constant and not a
	// knob.
	idleStreamsPerServer = 2

	// maxStreams is the concurrency backstop. Reaching it is not a failure:
	// the agent stops offering idle streams, the server's dispatch blocks,
	// and the wait reaches the caller as latency. Without a ceiling the
	// agent would accept work it cannot do and the queue would move into its
	// heap, where nothing can see it.
	maxStreams = 256

	// sizeInterval is how often the stream count is re-evaluated.
	sizeInterval = time.Second
)

var errPoolClosed = errors.New("connection pool closed")

// roundRobinServiceConfig spreads a connection's streams across every server
// instance DNS resolves to, instead of pinning it to one (grpc's pick_first
// default). Spread then comes from the balancer rather than from opening more
// connections and hoping the load balancer scatters them.
const roundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

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
	router          *Router
	backend         Backend
	httpClient      *http.Client

	running   atomic.Bool
	parentCtx context.Context
	cancelAll context.CancelFunc

	mu                sync.Mutex
	streams           map[*tunnelStream]struct{} // connected streams
	runningWorkers    int                        // live manageStream goroutines
	slotSeq           int                        // worker id generator (logging)
	currentToken      string
	currentServerAddr string

	// Stream sizing. targetSlots is the desired stream count, busySlots
	// counts calls in flight across all streams, serverMaxStreams is the
	// ServerHello-announced per-token cap (0 = none), and observedServers is
	// how many distinct server instances our streams are spread over.
	targetSlots      atomic.Int32
	busySlots        atomic.Int32
	serverMaxStreams atomic.Int32
	observedServers  atomic.Int32

	// restartMu serializes Restart() so two concurrent calls do not race the
	// Close→Start handoff. Held only across the Close+Start; never with mu.
	restartMu sync.Mutex

	// registerMu serializes Register() calls and protects lastRegisterAt.
	registerMu     sync.Mutex
	lastRegisterAt time.Time

	consecFailures atomic.Int32

	// inflightSem caps concurrent in-flight requests. nil means unbounded.
	inflightSem chan struct{}

	// pool holds the fixed set of connections every stream rides on.
	pool *connPool

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
	id       int // worker id, for logging
	streamID string
	serverID string
	conn     *grpc.ClientConn
	cancel   context.CancelFunc

	// inflight counts calls currently running on this stream — always 0 or
	// 1, since a stream carries one call at a time. lastCallAt is the
	// UnixNano timestamp of the last call start or completion.
	inflight   atomic.Int32
	lastCallAt atomic.Int64
}

// streamCtx bundles the per-stream values needed to run streamLoop after a
// successful handshake.
type streamCtx struct {
	ts            *tunnelStream
	stream        pb.TunnelService_TunnelClient
	sendFn        sendFunc
	hbMs          int32
	maxFrameBytes int32
}

// sendFunc serializes Send() calls on a gRPC stream. Multiple goroutines
// (heartbeat responses, HTTP response handlers) may send concurrently; the
// mutex prevents data races. On a Send error the func cancels the stream so
// the recv loop notices and reconnects.
type sendFunc func(msg *pb.ClientFrame) error

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
		// Mirrors the pooling of the injected client (see createHttpTransport):
		// the default transport's 2 idle connections per host would serialize
		// concurrent calls behind fresh handshakes.
		maxConns := p.Config.UpstreamMaxConnsPerHost
		if maxConns < 1 {
			maxConns = 128
		}
		httpClient = &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        maxConns * 2,
				MaxIdleConnsPerHost: maxConns,
				MaxConnsPerHost:     maxConns,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	tc := &tunnelClient{
		config:          p.Config,
		logger:          p.Logger.Named("grpc-tunnel"),
		integrationInfo: p.IntegrationInfo,
		registration:    p.Registration,
		httpClient:      httpClient,
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
	active := len(tc.streams)
	tc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","relay_mode":"grpc-tunnel","streams":%d,"busy":%d,"target":%d}`,
		active, tc.busySlots.Load(), tc.targetSlots.Load())
}

func (tc *tunnelClient) Start() error {
	if !tc.running.CompareAndSwap(false, true) {
		return fmt.Errorf("already started")
	}
	tc.mu.Lock()
	tc.streams = make(map[*tunnelStream]struct{})
	tc.runningWorkers = 0
	tc.pool = newConnPool(tc.config.TunnelConns)
	tc.busySlots.Store(0)
	tc.serverMaxStreams.Store(0)
	tc.observedServers.Store(0)
	tc.targetSlots.Store(int32(tc.idleReserve()))
	ctx, cancel := context.WithCancel(context.Background())
	tc.parentCtx = ctx
	tc.cancelAll = cancel
	tc.mu.Unlock()

	go tc.startAsync()
	return nil
}

func (tc *tunnelClient) startAsync() {
	// Allow tests to pre-populate the router/backend and skip accept-file
	// rendering.
	if tc.router == nil {
		if err := tc.setupRouter(); err != nil {
			tc.logger.Error("Failed to set up call router", zap.Error(err))
			return
		}
	}
	if tc.backend == nil {
		tc.backend = NewHttpBackend(tc.httpClient, tc.logger)
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

	tc.ensureWorkers()

	tc.wg.Add(1)
	go tc.periodicReregister()

	tc.wg.Add(1)
	go tc.sizeLoop()

	tc.logger.Info("gRPC tunnel started",
		zap.Int("conns", tc.config.TunnelConns),
		zap.Int("idleStreamsPerServer", idleStreamsPerServer),
		zap.Int("maxStreams", maxStreams),
		zap.Int("maxInflightRequests", tc.config.MaxInflightRequests),
		zap.Int("upstreamMaxConnsPerHost", tc.config.UpstreamMaxConnsPerHost),
		zap.Duration("maxRequestTimeout", tc.config.MaxRequestTimeout),
	)
}

func (tc *tunnelClient) setupRouter() error {
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
	tc.router = NewRouter(af2.Wrapper().PrivateRules(), tc.logger)
	return nil
}

// resolveRegistration returns the tunnel token and server address, either
// from the BROKER_SERVER_URL/BROKER_TOKEN env override (fromEnv=true, no
// API call) or from a Cortex API registration. Shared by initialRegister
// and reregister so env-var handling and address precedence live in one
// place.
func (tc *tunnelClient) resolveRegistration() (token, serverAddr string, fromEnv bool, err error) {
	envServerURL := os.Getenv("BROKER_SERVER_URL")
	envToken := os.Getenv("BROKER_TOKEN")

	if envServerURL != "" && envToken != "" {
		return envToken, envServerURL, true, nil
	}

	regInfo, err := tc.registration.Register(tc.integrationInfo.Integration, tc.integrationInfo.Alias)
	if err != nil {
		return "", "", false, err
	}
	tc.logger.Info("Registered with Cortex API", zap.String("serverUri", regInfo.ServerUri))

	serverAddr = regInfo.ServerUri
	if envServerURL != "" {
		serverAddr = envServerURL
	}
	return regInfo.Token, serverAddr, false, nil
}

// storeRegistration records the resolved token/address and reports whether
// the token rotated. It also stamps lastRegisterAt for the forced-
// re-register rate limit.
func (tc *tunnelClient) storeRegistration(token, serverAddr string) (rotated bool) {
	tc.mu.Lock()
	rotated = tc.currentToken != "" && tc.currentToken != token
	tc.currentToken = token
	tc.currentServerAddr = stripScheme(serverAddr)
	tc.mu.Unlock()

	tc.registerMu.Lock()
	tc.lastRegisterAt = time.Now()
	tc.registerMu.Unlock()
	return rotated
}

// initialRegister establishes the initial server address + token, retrying
// with backoff until success or shutdown.
func (tc *tunnelClient) initialRegister() error {
	backoff := tc.config.FailWaitTime
	if backoff <= 0 {
		backoff = time.Second
	}

	for tc.running.Load() && tc.parentCtx.Err() == nil {
		token, serverAddr, fromEnv, err := tc.resolveRegistration()
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

		if fromEnv {
			tc.logger.Info("Using direct connection config (skipping registration)",
				zap.String("serverUrl", serverAddr))
		}
		tc.storeRegistration(token, serverAddr)
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

// reregister refreshes the registration and, on token rotation, cycles all
// streams so they reconnect with the new token. When force=true, the call
// is rate-limited so an auth-error storm doesn't hammer Cortex.
func (tc *tunnelClient) reregister(reason string, force bool) {
	tc.registerMu.Lock()
	rateLimited := force && time.Since(tc.lastRegisterAt) < forcedReregisterRateLimit
	tc.registerMu.Unlock()
	if rateLimited {
		return
	}

	token, serverAddr, fromEnv, err := tc.resolveRegistration()
	if err != nil {
		tc.logger.Warn("Re-registration failed", zap.String("reason", reason), zap.Error(err))
		return
	}
	if fromEnv {
		// Direct-config mode — nothing to refresh.
		return
	}

	if tc.storeRegistration(token, serverAddr) {
		tc.tokenRotationsTotal.Inc()
		tc.logger.Info("Token rotated; cycling streams", zap.String("reason", reason))
		tc.cycleAllStreams()
	}
}

func (tc *tunnelClient) cycleAllStreams() {
	tc.mu.Lock()
	streams := make([]*tunnelStream, 0, len(tc.streams))
	for s := range tc.streams {
		streams = append(streams, s)
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

func (tc *tunnelClient) addStream(ts *tunnelStream) {
	tc.mu.Lock()
	tc.streams[ts] = struct{}{}
	tc.mu.Unlock()
}

func (tc *tunnelClient) removeStream(ts *tunnelStream) {
	tc.mu.Lock()
	delete(tc.streams, ts)
	tc.mu.Unlock()
	tc.connectionsActive.WithLabelValues(ts.serverID).Dec()
}

// idleReserve is how many streams to keep idle and ready: a fixed number for
// each server instance we currently hold streams on.
//
// Per-server because a server dispatches only onto the streams registered
// with itself, so it makes callers wait as soon as its own share of the
// reserve is taken — however idle the agent is overall. A flat reserve
// spread over S servers leaves each with reserve/S, which thins toward zero
// as the fleet grows, and the symptom is latency with no visible cause.
func (tc *tunnelClient) idleReserve() int {
	servers := int(tc.observedServers.Load())
	if servers < 1 {
		servers = 1
	}
	return idleStreamsPerServer * servers
}

// refreshObservedServers recounts the distinct server instances our streams
// are spread over. Called on the sizing tick rather than per call: it
// allocates, and the count only moves when the fleet does.
func (tc *tunnelClient) refreshObservedServers() {
	tc.mu.Lock()
	seen := make(map[string]struct{}, len(tc.streams))
	for s := range tc.streams {
		if s.serverID != "" {
			seen[s.serverID] = struct{}{}
		}
	}
	tc.mu.Unlock()
	tc.observedServers.Store(int32(len(seen)))
}

// streamCap is the ceiling on concurrent streams, lowered to the
// server-announced per-token cap when there is one.
func (tc *tunnelClient) streamCap() int {
	limit := maxStreams
	if announced := int(tc.serverMaxStreams.Load()); announced > 0 && announced < limit {
		limit = announced
	}
	return limit
}

// resize is the entire sizing rule: hold enough streams for the calls in
// flight, plus the idle reserve, bounded by the cap.
//
// Growth and shrink both fall out of it, so there is no watermark, no growth
// step and no cooldown to tune. A stream exists because a call needs one or
// because the reserve wants one ready; nothing else opens or closes them.
func (tc *tunnelClient) resize() {
	// busySlots is never negative, so this is already at least the reserve —
	// no separate floor, and none that could override the cap below.
	desired := int(tc.busySlots.Load()) + tc.idleReserve()

	// The cap is clamped last so it always wins. A server announcing a cap
	// lower than our reserve must still be obeyed, or every stream past its
	// limit is rejected in a loop.
	if limit := tc.streamCap(); desired > limit {
		desired = limit
	}
	if desired < 1 {
		desired = 1
	}

	tc.targetSlots.Store(int32(desired))
	tc.ensureWorkers()
}

// ensureWorkers spawns stream workers until the live worker count meets
// targetSlots.
func (tc *tunnelClient) ensureWorkers() {
	if !tc.running.Load() {
		return
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	target := int(tc.targetSlots.Load())
	for tc.runningWorkers < target {
		tc.runningWorkers++
		tc.slotSeq++
		id := tc.slotSeq
		tc.wg.Add(1)
		go tc.manageStream(id)
	}
}

// tryRetireWorker reports whether the calling worker should exit because the
// live worker count exceeds the target; it deregisters the worker when
// retiring.
func (tc *tunnelClient) tryRetireWorker() bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.runningWorkers > int(tc.targetSlots.Load()) {
		tc.runningWorkers--
		return true
	}
	return false
}

// workerExited deregisters a worker that is exiting for reasons other than
// retirement (shutdown).
func (tc *tunnelClient) workerExited() {
	tc.mu.Lock()
	tc.runningWorkers--
	tc.mu.Unlock()
}

// sizeLoop re-evaluates the stream count once per tick. resize() also runs at
// call admission, which reacts to load the instant it arrives; this catches
// what admission cannot — the fleet changing size, and streams left over
// after a burst that no call completion is coming to clean up.
func (tc *tunnelClient) sizeLoop() {
	defer tc.wg.Done()
	ticker := time.NewTicker(sizeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-tc.parentCtx.Done():
			return
		case <-ticker.C:
			tc.refreshObservedServers()
			tc.resize()
			tc.retireExcess()
		}
	}
}

// retireExcess cancels idle streams while more are open than the target
// wants. A healthy stream sits blocked in Recv and would otherwise never
// notice that the target dropped.
//
// Only streams idle for a full tick are eligible, which keeps the window
// narrow in which the server dispatches onto a stream we are cancelling; the
// dispatcher fails such a call back to its caller.
func (tc *tunnelClient) retireExcess() {
	cutoff := time.Now().Add(-sizeInterval).UnixNano()

	tc.mu.Lock()
	excess := tc.runningWorkers - int(tc.targetSlots.Load())
	var doomed []*tunnelStream
	for s := range tc.streams {
		if len(doomed) >= excess {
			break
		}
		if s.inflight.Load() == 0 && s.lastCallAt.Load() < cutoff {
			doomed = append(doomed, s)
		}
	}
	tc.mu.Unlock()

	for _, s := range doomed {
		s.cancel()
	}
}

func (tc *tunnelClient) manageStream(id int) {
	defer tc.wg.Done()

	backoff := minBackoff
	first := true

	for tc.running.Load() && tc.parentCtx.Err() == nil {
		if tc.tryRetireWorker() {
			tc.logger.Debug("Tunnel slot retired", zap.Int("slot", id))
			return
		}

		// Apply backoff before retries (not before the first attempt).
		if !first {
			tc.reconnectsTotal.WithLabelValues("").Inc()
			select {
			case <-time.After(jittered(backoff)):
			case <-tc.parentCtx.Done():
				tc.workerExited()
				return
			}
			backoff = nextBackoff(backoff)
		}
		first = false

		err := tc.runOneStream(id)
		if err == nil || tc.parentCtx.Err() != nil {
			// Clean shutdown or no error path (shouldn't really happen — runOneStream
			// returns an error whenever the stream ends).
			tc.workerExited()
			return
		}

		// A stream cancelled by retireExcess ends with a Canceled error;
		// retire quietly instead of logging a reconnect warning.
		if tc.tryRetireWorker() {
			tc.logger.Debug("Tunnel slot retired", zap.Int("slot", id))
			return
		}

		// Classify error and react.
		var ce *connectError
		if errors.As(err, &ce) {
			tc.connectErrorsTotal.WithLabelValues(ce.phase).Inc()
		}

		// Per-token cap: the server refuses more streams for this token.
		// Clamp the pool so we stop asking, and retire if now above target.
		if status.Code(err) == codes.ResourceExhausted || (ce != nil && status.Code(ce.cause) == codes.ResourceExhausted) {
			tc.handleTokenCap(id)
			continue
		}

		if status.Code(err) == codes.Unauthenticated || (ce != nil && status.Code(ce.cause) == codes.Unauthenticated) {
			tc.logger.Warn("Auth failure; forcing re-registration",
				zap.Int("slot", id), zap.Error(err))
			tc.reregister("unauthenticated", true)
			tc.consecFailures.Store(0)
			// keep backoff short for auth recovery
			backoff = minBackoff
			continue
		}

		if tc.consecFailures.Add(1) >= failuresBeforeReregister {
			tc.logger.Warn("Repeated open failures; forcing re-registration",
				zap.Int("slot", id), zap.Int32("consecutive", tc.consecFailures.Load()))
			tc.reregister("repeated-failures", true)
			tc.consecFailures.Store(0)
		}

		tc.logger.Warn("Tunnel slot stream ended; will retry",
			zap.Int("slot", id), zap.Error(err), zap.Duration("nextBackoff", backoff))
	}
	tc.workerExited()
}

// handleTokenCap reacts to a ResourceExhausted stream rejection by recording
// the ceiling the server is actually enforcing, so resize stops asking for
// more streams than it will grant.
func (tc *tunnelClient) handleTokenCap(id int) {
	tc.mu.Lock()
	connected := len(tc.streams)
	tc.mu.Unlock()
	if connected < 1 {
		connected = 1
	}

	if cur := tc.serverMaxStreams.Load(); cur == 0 || int(cur) > connected {
		tc.serverMaxStreams.Store(int32(connected))
		tc.logger.Info("Server per-token stream cap hit; clamping",
			zap.Int("slot", id), zap.Int("cap", connected))
	}
	tc.resize()
}

// runOneStream opens a stream on a pooled connection, handshakes, and runs
// streamLoop until the stream ends. Returns the terminating error (or nil on
// clean shutdown).
func (tc *tunnelClient) runOneStream(id int) error {
	sc, err := tc.openSlot(id)
	if err != nil {
		return err
	}
	tc.addStream(sc.ts)
	defer func() {
		tc.removeStream(sc.ts)
		sc.ts.cancel()
		// The connection belongs to the pool and outlives the stream.
	}()

	// Success — reset the global failure counter.
	tc.consecFailures.Store(0)

	return tc.streamLoop(sc)
}

// openSlot borrows a pooled connection, opens a stream on it and performs
// the gRPC handshake.
func (tc *tunnelClient) openSlot(id int) (*streamCtx, error) {
	token, serverAddr := tc.getCurrentConfig()
	if token == "" || serverAddr == "" {
		return nil, newConnectErr("config", errors.New("no registration token/address"))
	}

	dialOpts, dialAddr, err := tc.buildDialOptions(serverAddr)
	if err != nil {
		return nil, newConnectErr("dial_opts", err)
	}

	// Streams are spread over the pool by worker id. The connection is the
	// pool's to close, never the stream's.
	conn, err := tc.pool.get(id-1, func() (*grpc.ClientConn, error) {
		return grpc.NewClient(dialAddr, dialOpts...)
	})
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
		return nil, newConnectErr("open_stream", err)
	}

	hello := &pb.ClientFrame{
		Msg: &pb.ClientFrame_Hello{
			Hello: &pb.ClientHello{
				BrokerToken: token,
				// Everything below is informational (logs/metrics on the
				// server); the broker token alone is the credential.
				ClientVersion: common.ClientVersion,
				TenantId:      os.Getenv("CORTEX_TENANT_ID"),
				Integration:   tc.integrationInfo.Integration.String(),
				Alias:         tc.integrationInfo.Alias,
				InstanceId:    tc.config.InstanceId,
			},
		},
	}

	if err := stream.Send(hello); err != nil {
		handshakeTimer.Stop()
		cancel()
		return nil, newConnectErr("send_hello", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		handshakeTimer.Stop()
		cancel()
		return nil, newConnectErr("recv_hello", err)
	}
	handshakeTimer.Stop()

	serverHello := msg.GetHello()
	if serverHello == nil {
		cancel()
		return nil, newConnectErr("recv_hello", errors.New("expected ServerHello"))
	}

	if serverHello.MaxStreams > 0 {
		tc.serverMaxStreams.Store(serverHello.MaxStreams)
	}

	// The per-server cap counts connections, not streams: on a shared
	// connection only the first stream takes a slot, since every stream on it
	// talks to the same backend. That keeps MaxStreamsPerServer meaning "how
	// many connections may land on one server instance" in every mode.
	ts := &tunnelStream{
		id:       id,
		streamID: serverHello.StreamId,
		serverID: serverHello.ServerId,
		conn:     conn,
		cancel:   cancel,
	}
	ts.lastCallAt.Store(time.Now().UnixNano())

	tc.connectionsActive.WithLabelValues(ts.serverID).Inc()
	tc.logger.Info("Tunnel stream established",
		zap.String("streamId", ts.streamID),
		zap.String("serverId", ts.serverID),
		zap.Int32("heartbeatIntervalMs", serverHello.HeartbeatIntervalMs),
		zap.Int("slot", id),
	)

	return &streamCtx{
		ts:            ts,
		stream:        stream,
		sendFn:        tc.makeSendFunc(ts, stream),
		hbMs:          serverHello.HeartbeatIntervalMs,
		maxFrameBytes: serverHello.MaxFrameBytes,
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
	return func(msg *pb.ClientFrame) error {
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
	table := newCallTable()
	defer table.cancelAll()

	var heartbeatTimer *time.Timer
	if sc.hbMs > 0 {
		timeout := 2 * time.Duration(sc.hbMs) * time.Millisecond
		heartbeatTimer = time.AfterFunc(timeout, func() {
			// While a call is active the recv loop may be blocked delivering
			// body bytes to a slow upstream, starving heartbeat reads without
			// the connection being dead. Transport-level gRPC keepalive
			// covers dead TCP during calls.
			if table.active() {
				return
			}
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

		switch m := msg.Msg.(type) {
		case *pb.ServerFrame_Heartbeat:
			if err := sc.sendFn(&pb.ClientFrame{
				Msg: &pb.ClientFrame_Heartbeat{
					Heartbeat: &pb.Heartbeat{TimestampMs: time.Now().UnixMilli()},
				},
			}); err != nil {
				// makeSendFunc has already cancelled the stream; the next Recv
				// will exit. No need to log again here.
				_ = err
			}
		case *pb.ServerFrame_Call:
			tc.handleCallFrame(sc, table, m.Call)
		case *pb.ServerFrame_Hello:
			tc.logger.Warn("Unexpected ServerHello after handshake")
		}
	}
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
	streams := make([]*tunnelStream, 0, len(tc.streams))
	for s := range tc.streams {
		streams = append(streams, s)
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

	if tc.pool != nil {
		tc.pool.closeAll()
	}
	if tc.pool != nil {
		tc.pool.closeAll()
	}

	tc.mu.Lock()
	tc.streams = nil
	tc.runningWorkers = 0
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

	opts = append(opts, grpc.WithDefaultServiceConfig(roundRobinServiceConfig))

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

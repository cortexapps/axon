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
	growCooldown              = time.Second
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
	router          *Router
	backend         Backend
	httpClient      *http.Client

	running   atomic.Bool
	parentCtx context.Context
	cancelAll context.CancelFunc

	mu                 sync.Mutex
	streams            map[*tunnelStream]struct{} // connected streams
	runningWorkers     int                        // live manageStream goroutines
	slotSeq            int                        // worker id generator (logging)
	serverStreamCounts map[string]int
	currentToken       string
	currentServerAddr  string

	// Watermark pool state. targetSlots is the desired worker count in
	// [MinTunnelSlots, effectiveMaxSlots]; busySlots counts in-flight calls
	// across all streams; serverMaxStreams is the ServerHello-announced
	// per-token cap (0 = none); lastGrowAt rate-limits growth steps.
	targetSlots      atomic.Int32
	busySlots        atomic.Int32
	serverMaxStreams atomic.Int32
	lastGrowAt       atomic.Int64

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
	id       int // worker id, for logging
	streamID string
	serverID string
	conn     *grpc.ClientConn
	cancel   context.CancelFunc

	// inflight counts calls currently running on this stream (0 or 1 in
	// the slot-pool model); lastCallAt is the UnixNano timestamp of the
	// last call start/completion, used for idle-shrink.
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
	tc.serverStreamCounts = make(map[string]int)
	tc.busySlots.Store(0)
	tc.serverMaxStreams.Store(0)
	tc.targetSlots.Store(int32(tc.minSlots()))
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
	go tc.watermarkLoop()

	tc.logger.Info("gRPC tunnel started",
		zap.Int("minSlots", tc.minSlots()),
		zap.Int("maxSlots", tc.config.MaxTunnelSlots),
		zap.Int("maxStreamsPerServer", tc.config.MaxStreamsPerServer),
		zap.Int("maxInflightRequests", tc.config.MaxInflightRequests),
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

func (tc *tunnelClient) addStream(ts *tunnelStream) {
	tc.mu.Lock()
	tc.streams[ts] = struct{}{}
	tc.mu.Unlock()
}

func (tc *tunnelClient) removeStream(ts *tunnelStream) {
	tc.mu.Lock()
	delete(tc.streams, ts)
	tc.mu.Unlock()
	tc.releaseServerSlot(ts.serverID)
	tc.connectionsActive.WithLabelValues(ts.serverID).Dec()
}

// minSlots returns the configured pool floor (at least 1).
func (tc *tunnelClient) minSlots() int {
	if tc.config.MinTunnelSlots < 1 {
		return 1
	}
	return tc.config.MinTunnelSlots
}

// effectiveMaxSlots is the configured ceiling clamped to the
// server-announced per-token cap, never below the floor.
func (tc *tunnelClient) effectiveMaxSlots() int {
	max := tc.config.MaxTunnelSlots
	if max < 1 {
		max = 1
	}
	if serverMax := int(tc.serverMaxStreams.Load()); serverMax > 0 && serverMax < max {
		max = serverMax
	}
	if min := tc.minSlots(); max < min {
		max = min
	}
	return max
}

// ensureWorkers spawns manageStream workers until the live worker count
// meets targetSlots.
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

// tryRetireWorker reports whether the calling worker should exit because
// the live worker count exceeds the target; it deregisters the worker when
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

// maybeGrow is called when a call is admitted (the moment a slot goes
// busy). It keeps a free-capacity watermark: when fewer than 25% of
// connected streams (and fewer than one) are idle, the pool grows by
// ~half its size toward the effective max, at most once per growCooldown.
//
// This is deliberately client-side: a slot only becomes busy because the
// server dispatched onto it, so the client sees saturation at the same
// instant the server does — no negotiation round-trip needed.
func (tc *tunnelClient) maybeGrow() {
	last := tc.lastGrowAt.Load()
	now := time.Now().UnixNano()
	if now-last < int64(growCooldown) {
		return
	}

	tc.mu.Lock()
	active := len(tc.streams)
	running := tc.runningWorkers
	tc.mu.Unlock()

	busy := int(tc.busySlots.Load())
	idle := active - busy
	watermark := active / 4
	if watermark < 1 {
		watermark = 1
	}
	if idle >= watermark {
		return
	}

	effMax := tc.effectiveMaxSlots()
	if running >= effMax {
		return
	}
	step := active / 2
	if step < 1 {
		step = 1
	}
	newTarget := running + step
	if newTarget > effMax {
		newTarget = effMax
	}

	if !tc.lastGrowAt.CompareAndSwap(last, now) {
		return // another call won the growth race within the cooldown
	}
	tc.targetSlots.Store(int32(newTarget))
	tc.logger.Info("Growing tunnel pool",
		zap.Int("connected", active),
		zap.Int("busy", busy),
		zap.Int("newTarget", newTarget),
	)
	tc.ensureWorkers()
}

// watermarkLoop re-evaluates the free-capacity watermark once per
// growth-cooldown period. Admission-time checks (maybeGrow in startCall)
// react instantly to new load, but under sustained saturation with
// long-running calls no new calls are admitted — this loop keeps growth
// converging toward max until idle capacity reappears.
func (tc *tunnelClient) watermarkLoop() {
	defer tc.wg.Done()
	ticker := time.NewTicker(growCooldown)
	defer ticker.Stop()
	for {
		select {
		case <-tc.parentCtx.Done():
			return
		case <-ticker.C:
			tc.maybeGrow()
		}
	}
}

// noteIdleRetire is called by a slot watchdog when its stream has been
// idle past SlotIdleTimeout. It lowers the target (never below min) so
// the calling worker retires via tryRetireWorker.
func (tc *tunnelClient) noteIdleRetire() bool {
	min := int32(tc.minSlots())
	for {
		t := tc.targetSlots.Load()
		if t <= min {
			return false
		}
		if tc.targetSlots.CompareAndSwap(t, t-1) {
			return true
		}
	}
}

// manageStream owns one tunnel slot worker: it keeps a stream open,
// reconnecting on failure, until shutdown or retirement (the watermark
// pool lowered targetSlots below the live worker count).
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

		// A stream cancelled for retirement (idle watchdog or watermark
		// shrink) ends with a Canceled error; retire quietly instead of
		// logging a reconnect warning.
		if tc.tryRetireWorker() {
			tc.logger.Debug("Tunnel slot retired", zap.Int("slot", id))
			return
		}

		// Classify error and react.
		var ce *connectError
		if errors.As(err, &ce) {
			tc.connectErrorsTotal.WithLabelValues(ce.phase).Inc()
		}

		// Server-cap hit: just back off and try again (LB may give a different instance).
		if errors.Is(err, errServerCapHit) {
			tc.logger.Debug("Server cap hit; retrying", zap.Int("slot", id))
			continue
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

// handleTokenCap reacts to a ResourceExhausted stream rejection: lower the
// pool target to the number of currently connected streams (never below
// min) so workers above it retire instead of hammering the server.
func (tc *tunnelClient) handleTokenCap(id int) {
	tc.mu.Lock()
	connected := len(tc.streams)
	tc.mu.Unlock()

	min := tc.minSlots()
	newTarget := connected
	if newTarget < min {
		newTarget = min
	}
	if int(tc.targetSlots.Load()) > newTarget {
		tc.targetSlots.Store(int32(newTarget))
		tc.logger.Info("Server per-token stream cap hit; clamping pool",
			zap.Int("slot", id), zap.Int("newTarget", newTarget))
	}
	// Remember the cap so future growth respects it even before the next
	// ServerHello announcement.
	if cur := tc.serverMaxStreams.Load(); cur == 0 || int(cur) > newTarget {
		tc.serverMaxStreams.Store(int32(newTarget))
	}
}

// runOneStream opens a fresh ClientConn, handshakes, and runs streamLoop until
// the stream ends. Returns the terminating error (or nil on clean shutdown).
func (tc *tunnelClient) runOneStream(id int) error {
	sc, err := tc.openSlot(id)
	if err != nil {
		return err
	}
	tc.addStream(sc.ts)
	watchdogDone := make(chan struct{})
	defer func() {
		close(watchdogDone)
		tc.removeStream(sc.ts)
		sc.ts.cancel()
		if sc.ts.conn != nil {
			sc.ts.conn.Close()
		}
	}()

	// Success — reset the global failure counter.
	tc.consecFailures.Store(0)

	go tc.idleWatchdog(sc.ts, watchdogDone)

	return tc.streamLoop(sc)
}

// idleWatchdog retires a slot whose stream has carried no call for
// SlotIdleTimeout while the pool is above its floor, and also enforces a
// lowered target promptly (a healthy stream otherwise only re-checks
// retirement after it ends). Jittered so slots don't all retire at once
// after a burst.
func (tc *tunnelClient) idleWatchdog(ts *tunnelStream, done <-chan struct{}) {
	idleTimeout := tc.config.SlotIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 10 * time.Minute
	}
	interval := idleTimeout / 4
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}

	// Random initial phase so slots created in the same growth burst don't
	// all evaluate the idle threshold in the same instant and retire as a
	// thundering herd.
	select {
	case <-done:
		return
	case <-tc.parentCtx.Done():
		return
	case <-time.After(time.Duration(rand.Int64N(int64(interval)))):
	}

	ticker := time.NewTicker(jittered(interval))
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-tc.parentCtx.Done():
			return
		case <-ticker.C:
			if ts.inflight.Load() > 0 {
				continue
			}

			// Target already below the live worker count (watermark shrink
			// or token-cap clamp): cancel so the worker loop re-checks
			// retirement without waiting for the stream to die naturally.
			tc.mu.Lock()
			excess := tc.runningWorkers > int(tc.targetSlots.Load())
			tc.mu.Unlock()
			if excess {
				ts.cancel()
				return
			}

			idleFor := time.Since(time.Unix(0, ts.lastCallAt.Load()))
			if idleFor < idleTimeout {
				continue
			}
			if !tc.noteIdleRetire() {
				continue // already at the floor
			}
			// Small race: the server may dispatch onto this stream between
			// the inflight check and cancel; the dispatcher fails that call
			// over to its caller. Idle-retired slots are the least likely
			// dispatch targets (AcquireIdleStream prefers recent success),
			// so this window is acceptably rare.
			tc.logger.Info("Retiring idle tunnel slot",
				zap.Int("slot", ts.id),
				zap.Duration("idleFor", idleFor),
			)
			ts.cancel()
			return
		}
	}
}

// openSlot dials, performs the gRPC handshake, and acquires a server-id slot.
// Returns errServerCapHit if the slot cap is exceeded for the server we landed on.
func (tc *tunnelClient) openSlot(id int) (*streamCtx, error) {
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

	if serverHello.MaxStreams > 0 {
		tc.serverMaxStreams.Store(serverHello.MaxStreams)
	}

	if !tc.acquireServerSlot(serverHello.ServerId) {
		tc.logger.Info("Server stream cap reached; will retry to land on a different instance",
			zap.String("serverId", serverHello.ServerId),
			zap.Int("slot", id),
			zap.Int("cap", tc.config.MaxStreamsPerServer),
		)
		cancel()
		conn.Close()
		return nil, errServerCapHit
	}

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

	tc.mu.Lock()
	tc.streams = nil
	tc.runningWorkers = 0
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

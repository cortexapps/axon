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
	"sort"
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
	"github.com/cortexapps/axon/util"
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
	maxChunkSize   = 1024 * 1024
	maxGrpcMsgSize = 8 * 1024 * 1024
	// maxAcceptedFrameBytes bounds the per-call read buffer the agent will
	// size from the server's hello. The frame size is announced by the
	// server, not configured by whoever runs the agent, so without a ceiling
	// here a server-side change to MAX_FRAME_BYTES silently multiplies the
	// memory ceiling of every agent in the fleet: the buffer is allocated per
	// in-flight call, so the agent's peak is this value times its in-flight
	// cap. Clamping costs nothing when the server is sane -- frames larger
	// than the clamp are still carried, just read in more than one chunk.
	maxAcceptedFrameBytes = 4 * 1024 * 1024
	handshakeTimeout      = 30 * time.Second
	// keepaliveInterval is deliberately long. Behind a load balancer the
	// client's HTTP/2 peer is the load balancer, not the tunnel server, and
	// its ping policy is not ours to set: GCLB answers pings it considers
	// too frequent with GOAWAY ENHANCE_YOUR_CALM / too_many_pings and drops
	// the connection. What that policy actually rate-limits is pings sent
	// with no DATA frames flowing, and an anchored connection always carries
	// a stream with heartbeats on it, so these pings are a backstop for a
	// dead TCP path rather than the liveness mechanism.
	keepaliveInterval         = 60 * time.Second
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

	// streamIdleWindow is how long a stream may go without carrying a call
	// before it counts as surplus. A stream that has carried one inside the
	// window is kept, whether or not it is busy right now.
	//
	// It exists because a stream costs more to open than to hold: the open is
	// a round trip, and it makes the server notify the dispatcher, which
	// writes redis. Sizing on instantaneous concurrency meant one open and one
	// close per call — in staging, 349 opens and 316 closes in forty minutes
	// at a few requests a minute, each paying that cost for a stream thrown
	// away seconds later. A minute is long enough that ordinary traffic reuses
	// a stream instead of reopening it, and short enough that a burst does not
	// pin capacity for the rest of the day.
	streamIdleWindow = time.Minute
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

	// anchor marks the one stream that holds its pooled connection open.
	// Anchors are never retired, so retireExcess must not pick them.
	anchor bool

	// inflight counts calls currently running on this stream — always 0 or
	// 1, since a stream carries one call at a time. lastCallAt is the
	// UnixNano timestamp of the last call start or completion, and is zero
	// until the stream carries its first call.
	//
	// openedAt is when the handshake finished, and it is a separate field
	// because the two answer different questions. Sizing asks whether a
	// stream has carried a call, so opening one must not answer yes;
	// retirement asks how long a stream has been quiet, and for a stream
	// that has never carried a call the answer is "since it opened", not
	// "since the epoch".
	inflight   atomic.Int32
	lastCallAt atomic.Int64
	openedAt   atomic.Int64

	// retiring marks a stream the agent is cancelling on purpose, set before
	// the cancel so the worker running it can tell a retirement from a
	// failure. Without it a deliberate cancel arrived as "context canceled"
	// and was handled as a broken stream: logged at Warn, counted as a
	// reconnect, and — the part that actually hurt — allowed to escalate the
	// worker's backoff, so a slot retired for being surplus came back slower
	// every time it happened.
	retiring atomic.Bool
}

// quietSince is the UnixNano instant this stream last did anything worth
// counting: carried a call, or failing that, came into existence.
func (ts *tunnelStream) quietSince() int64 {
	if last := ts.lastCallAt.Load(); last != 0 {
		return last
	}
	return ts.openedAt.Load()
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
	tc.resetForStart()

	go tc.startAsync()
	return nil
}

// resetForStart clears the state that described the connection we are no longer
// on, so a fresh Start does not inherit it.
//
// serverMaxStreams is deliberately kept. The server's per-token cap is fixed
// for the life of a server process — read from config at startup, never
// mutated — and every instance in the fleet is configured alike, so it is a
// property of the fleet rather than of any one connection. Zeroing it here
// meant streamCap fell back to the local maxStreams ceiling after every
// reconnect, and a reconnect opens streams as fast as there is work for them:
// the agent would sail past the real cap before the first ServerHello was read
// and learn it again from a burst of ResourceExhausted rejections. Keeping it
// costs nothing in staleness, because a handshake that announces a different
// cap overwrites it (openSlot, on every handshake) and the value cannot change under
// a live connection.
func (tc *tunnelClient) resetForStart() {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.streams = make(map[*tunnelStream]struct{})
	tc.runningWorkers = 0
	tc.pool = newConnPool(tc.config.TunnelConns)
	tc.busySlots.Store(0)
	tc.observedServers.Store(0)
	tc.targetSlots.Store(int32(tc.idleReserve()))
	ctx, cancel := context.WithCancel(context.Background())
	tc.parentCtx = ctx
	tc.cancelAll = cancel
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
	router, err := NewRouter(af2.Wrapper().PrivateRules(), tc.logger)
	if err != nil {
		return fmt.Errorf("error building accept file router: %w", err)
	}
	tc.router = router
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

// streamCap is the ceiling on concurrent streams, which is also the ceiling on
// concurrent calls: a stream carries exactly one call at a time.
//
// maxStreams and the announced cap are different units, and the difference is
// the whole point. maxStreams is this agent's own policy — how many calls it
// will serve at once, anywhere. The announced cap is how many streams a single
// server instance tolerates from one token, a defensive limit so that one agent
// cannot stampede one pod. Taking the lower of the two compared those units
// directly, which let a per-pod guard become the agent's fleet-wide policy: it
// ran at 64 where its policy is 256, and any reconnect that wanted more met a
// burst of rejections.
//
// What actually bounds us is the per-server limit times the servers we are
// spread over. With one server that degrades to the announced cap, which is the
// case the old clamp was really defending; before any handshake has told us how
// wide the fleet is we assume one server, so the reconnect window stays
// conservative rather than optimistic.
//
// This is an upper bound on what the servers will grant, not a licence to
// exceed our own policy, so maxStreams still wins when the fleet is wide.
func (tc *tunnelClient) streamCap() int {
	limit := maxStreams
	announced := int(tc.serverMaxStreams.Load())
	if announced <= 0 {
		return limit
	}
	servers := int(tc.observedServers.Load())
	if servers < 1 {
		servers = 1
	}
	if capacity := announced * servers; capacity < limit {
		limit = capacity
	}
	return limit
}

// anchorSlots is how many workers are anchors: one per pooled connection,
// bounded by whatever stream cap is in force.
//
// An anchor exists so its connection is never left carrying zero streams.
// That matters for two reasons that turn out to be the same reason. The
// dispatcher learns a server holds this token from the first stream to
// register and forgets it when the last one closes, so a connection that
// empties deregisters the agent from that server — with nothing wrong on
// either end. And a connection with no streams has no heartbeats, so its
// keepalive pings are pings with no data, which is the kind a load balancer
// answers with GOAWAY. Holding one stream per connection removes both.
func (tc *tunnelClient) anchorSlots() int {
	if tc.pool == nil {
		return 0
	}
	n := tc.pool.size()
	if limit := tc.streamCap(); n > limit {
		n = limit
	}
	return n
}

// resize is the sizing rule: hold a stream for every stream that has carried
// a call inside streamIdleWindow, plus the idle reserve, bounded by the cap.
//
// Growth is immediate — a call sets its stream's timestamp as it starts, and
// resize runs at admission, so the tick that sees the load opens the streams
// and the reserve is never what a caller waits on. Shrink is what waits, and
// it waits on the streams themselves rather than on a global counter: sizing
// both directions on instantaneous concurrency meant every call raised the
// target and every completion lowered it, so a stream was opened and retired
// per request.
//
// Recency subsumes concurrency, which is why busySlots does not appear here:
// a call stamps lastCallAt when it starts, so a stream carrying one is inside
// the window by definition. One signal, living on the thing it describes.
func (tc *tunnelClient) resize() {
	// usedWithin is never negative, so this is already at least the reserve —
	// no separate floor, and none that could override the cap below.
	desired := tc.usedWithin(streamIdleWindow) + tc.idleReserve()

	// Never ask for fewer streams than there are connections to anchor.
	// Without this the target can sit below the anchor count, and every
	// tick retireExcess would try to cancel streams that refuse to retire.
	if anchors := tc.anchorSlots(); desired < anchors {
		desired = anchors
	}

	// The cap is clamped last so it always wins. A server announcing a cap
	// lower than our reserve must still be obeyed, or every stream past its
	// limit is rejected in a loop. anchorSlots is already bounded by the
	// same cap, so the floor above cannot survive it.
	if limit := tc.streamCap(); desired > limit {
		desired = limit
	}
	if desired < 1 {
		desired = 1
	}

	tc.targetSlots.Store(int32(desired))
	tc.ensureWorkers()
}

// usedWithin counts the streams that carried a call inside the window, or are
// carrying one now. Takes mu and releases it before returning, so callers may
// go on to take it again.
func (tc *tunnelClient) usedWithin(window time.Duration) int {
	cutoff := time.Now().Add(-window).UnixNano()

	tc.mu.Lock()
	defer tc.mu.Unlock()
	n := 0
	for st := range tc.streams {
		if st.inflight.Load() != 0 || st.lastCallAt.Load() >= cutoff {
			n++
		}
	}
	return n
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
func (tc *tunnelClient) tryRetireWorker(id int) bool {
	// Anchors outlive every sizing decision. Worker ids 1..N map onto pool
	// connections 0..N-1 exactly, so the first N workers are the anchors.
	if id <= tc.anchorSlots() {
		return false
	}
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
// How many to cancel and which ones to cancel are separate questions, and
// keeping them apart is the whole of this function. The count comes from the
// fleet: streams open, less the target. The choice comes from the per-server
// budgets, because that is where a surplus can be spent without costing
// coverage.
//
// Letting the per-server budgets decide the count as well is what churned. A
// pod holding one stream more than its share is the ordinary state of an
// evenly-sized fleet — placement is the balancer's business, not ours — so
// there was always something over budget to cancel, and cancelling it left
// the agent below target, so a replacement opened at once and the balancer
// was just as free to put it back on the same pod. The result was a worker
// cancelled and reopened every tick for as long as the agent ran, each cycle
// paying a round trip, a dispatcher notification and a redis write for no
// change in the fleet.
//
// Only streams idle for a full tick are eligible, which keeps the window
// narrow in which the server dispatches onto a stream we are cancelling; the
// dispatcher fails such a call back to its caller.
func (tc *tunnelClient) retireExcess() {
	now := time.Now()
	staleBefore := now.Add(-streamIdleWindow).UnixNano()
	// A prunable stream must also have been quiet for a full tick, which keeps
	// narrow the window in which the server dispatches onto a stream we are
	// cancelling; the dispatcher fails such a call back to its caller.
	cutoff := now.Add(-sizeInterval).UnixNano()

	tc.mu.Lock()

	surplus := tc.runningWorkers - int(tc.targetSlots.Load())
	if surplus <= 0 {
		tc.mu.Unlock()
		return
	}

	// Grouped by server, because that is the unit the choice belongs to. A
	// server dispatches only onto the streams registered with itself, so its
	// own recent traffic says how many it needs, and its last stream is its
	// entire ability to reach this agent — retire that and the server
	// deregisters the token and answers "no tunnel available" for everything
	// the dispatcher sends there until a stream lands back on it.
	//
	// Judging the fleet in aggregate cannot see either fact. The surplus is
	// real, but it belongs to whichever server has been busy, and spending it
	// from an idle server strips that server's coverage while leaving the busy
	// one fat.
	groups := make(map[string]*serverStreams, len(tc.streams))
	for st := range tc.streams {
		g := groups[st.serverID]
		if g == nil {
			g = &serverStreams{}
			groups[st.serverID] = g
		}
		switch {
		case st.inflight.Load() != 0 || st.lastCallAt.Load() >= staleBefore:
			g.used++
		case st.anchor || st.quietSince() >= cutoff:
			g.pinned++
		default:
			g.prunable = append(g.prunable, st)
		}
	}

	// The order the surplus is spent in. Streams with no server yet go first:
	// they protect no coverage, because no dispatcher knows about them. Then
	// whatever sits above a server's own budget, which is the surplus wherever
	// the fleet count came from. Only then the stalest anywhere, for the case
	// the first two do not cover the count — a cap dropping under us, or every
	// server sitting exactly at budget while the fleet is still over target.
	var spend, overBudget, rest []*tunnelStream
	for serverID, g := range groups {
		sortStalestFirst(g.prunable)
		if serverID == "" {
			spend = append(spend, g.prunable...)
			continue
		}

		// What this server needs: what it used, plus its share of the reserve
		// so the next call does not wait on an open. Never below one, which is
		// what keeps it able to reach us at all.
		keep := g.used + idleStreamsPerServer
		if keep < 1 {
			keep = 1
		}
		over := g.used + g.pinned + len(g.prunable) - keep
		if over > len(g.prunable) {
			over = len(g.prunable)
		}
		if over < 0 {
			over = 0
		}
		overBudget = append(overBudget, g.prunable[:over]...)
		rest = append(rest, g.prunable[over:]...)
	}
	sortStalestFirst(overBudget)
	sortStalestFirst(rest)
	spend = append(spend, overBudget...)
	spend = append(spend, rest...)

	if surplus > len(spend) {
		surplus = len(spend)
	}
	doomed := spend[:surplus]

	// Marked under the lock, so the worker running the stream cannot see the
	// cancel before the reason for it and report a deliberate retirement as a
	// broken stream.
	for _, st := range doomed {
		st.retiring.Store(true)
	}
	tc.mu.Unlock()

	for _, st := range doomed {
		st.cancel()
	}
}

// serverStreams buckets one server's streams by what may be done with them.
type serverStreams struct {
	used     int             // carried a call inside the window
	pinned   int             // anchors, and streams quiet for under a tick
	prunable []*tunnelStream // the rest, stalest first once sorted
}

// sortStalestFirst orders streams by how long they have been quiet, longest
// first, so pruning spends the least useful stream a server has. This is what
// makes the gradient usable: the server hands work to the most recently used
// idle stream it has, so the ones at this end of the order are the ones its
// traffic has stopped reaching for.
func sortStalestFirst(streams []*tunnelStream) {
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].quietSince() < streams[j].quietSince()
	})
}

func (tc *tunnelClient) manageStream(id int) {
	defer tc.wg.Done()

	backoff := minBackoff
	first := true
	// Set once a stream has failed, so the reconnection that follows says so
	// and then goes quiet again.
	recovering := false

	for tc.running.Load() && tc.parentCtx.Err() == nil {
		if tc.tryRetireWorker(id) {
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

		retired, err := tc.runOneStream(id, recovering)
		recovering = false

		// We cancelled this stream ourselves. Nothing failed, so nothing here
		// escalates: no warning, no reconnect counted, and above all no
		// backoff growth — that is what made a slot retired for being surplus
		// come back slower every time, until it was spending more of its life
		// waiting than serving.
		if retired {
			if tc.tryRetireWorker(id) {
				tc.logger.Debug("Tunnel slot retired", zap.Int("slot", id))
				return
			}
			// The target moved back up while the cancel was in flight, or this
			// slot anchors a connection and cannot leave. Reopen from a clean
			// slate rather than from a failure's backoff.
			backoff = minBackoff
			continue
		}

		if err == nil || tc.parentCtx.Err() != nil {
			// Clean shutdown or no error path (shouldn't really happen — runOneStream
			// returns an error whenever the stream ends).
			tc.workerExited()
			return
		}

		// A stream cancelled by retireExcess ends with a Canceled error;
		// retire quietly instead of logging a reconnect warning.
		if tc.tryRetireWorker(id) {
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

		recovering = true
		tc.logger.Warn("Tunnel slot stream ended; will retry",
			zap.Int("slot", id), zap.Error(err), zap.Duration("nextBackoff", backoff))
	}
	tc.workerExited()
}

// handleTokenCap reacts to a ResourceExhausted stream rejection.
//
// It deliberately does not revise the announced cap. That value is a per-server
// limit which cannot change under a live connection, so a refusal while we hold
// fewer streams than it says nothing about our own usage — the likeliest cause
// is the previous generation's streams still registered on that pod and not yet
// reaped by heartbeat timeout. Clamping the ceiling down to whatever we happened
// to hold turned that into a self-inflicted throttle, as low as a single stream,
// during exactly the window the agent was trying to recover in.
//
// A refusal is instead left to be what it already is: per-connection back
// pressure. The worker that was refused backs off and retries on the manageStream
// loop, and sizing is recomputed in case the fleet looks different now.
func (tc *tunnelClient) handleTokenCap(id int) {
	tc.logger.Debug("Server refused another stream for this token; backing off",
		zap.Int("slot", id))
	tc.resize()
}

// runOneStream opens a stream on a pooled connection, handshakes, and runs
// streamLoop until the stream ends. Returns the terminating error (or nil on
// clean shutdown), and whether the stream ended because retireExcess
// cancelled it — which reaches the worker as an ordinary Canceled error and
// is otherwise indistinguishable from the connection breaking.
func (tc *tunnelClient) runOneStream(id int, recovering bool) (retired bool, err error) {
	sc, err := tc.openSlot(id, recovering)
	if err != nil {
		return false, err
	}
	tc.addStream(sc.ts)
	defer func() {
		tc.removeStream(sc.ts)
		sc.ts.cancel()
		// The connection belongs to the pool and outlives the stream.
	}()

	// Success — reset the global failure counter.
	tc.consecFailures.Store(0)

	err = tc.streamLoop(sc)
	return sc.ts.retiring.Load(), err
}

// openSlot borrows a pooled connection, opens a stream on it and performs
// the gRPC handshake.
func (tc *tunnelClient) openSlot(id int, recovering bool) (*streamCtx, error) {
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
				// Sent so the server can eventually authenticate the agent
				// directly and read the tenant from the token, which is the
				// only thing the registration round-trip currently exists to
				// do. Nothing validates it yet; sending it from the start is
				// what lets that work be built against real agents. Empty
				// when the agent was handed a broker token directly and never
				// registered.
				CortexApiToken: tc.config.CortexApiToken,
				// Everything below is informational (logs/metrics on the
				// server); the broker token alone is the credential today.
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
	ts := newTunnelStream(id, serverHello, conn, cancel, id <= tc.anchorSlots())

	tc.connectionsActive.WithLabelValues(ts.serverID).Inc()

	// Opening a stream is routine — the sizing rule does it whenever a call
	// needs one — so at Info it is pure noise. Coming back after a failure
	// is not routine: it is the other half of the warning that was already
	// logged, and without it the warning has no visible end.
	log := tc.logger.Debug
	logMsg := "Tunnel stream established"
	if recovering {
		log = tc.logger.Info
		logMsg = "Tunnel stream re-established after failure"
	}
	log(logMsg,
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
		maxFrameBytes: clampFrameBytes(serverHello.MaxFrameBytes, tc.logger),
	}, nil
}

// newTunnelStream records a stream the handshake has just established.
//
// It stamps openedAt and deliberately leaves lastCallAt at zero. Stamping
// lastCallAt here made a new stream indistinguishable from one that had just
// carried a call, for a full streamIdleWindow — so every stream the reserve
// opened counted itself as used on the next sizing tick, and the target grew
// by another whole reserve. The ramp fed on itself: it needed no traffic,
// climbed to the cap at a reserve per second, collapsed when the stamps aged
// out together, and started again.
func newTunnelStream(id int, hello *pb.ServerHello, conn *grpc.ClientConn, cancel context.CancelFunc, anchor bool) *tunnelStream {
	ts := &tunnelStream{
		id:       id,
		streamID: hello.StreamId,
		serverID: hello.ServerId,
		conn:     conn,
		cancel:   cancel,
		anchor:   anchor,
	}
	ts.openedAt.Store(time.Now().UnixNano())
	return ts
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
			Time:    keepaliveInterval,
			Timeout: keepaliveTimeout,
			// Never ping a connection with no streams on it. Such a ping is
			// exactly the "ping without data" a load balancer punishes, and
			// with an anchor per connection there is no streamless
			// connection worth keeping alive anyway.
			PermitWithoutStream: false,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxGrpcMsgSize),
			grpc.MaxCallSendMsgSize(maxGrpcMsgSize),
		),
	}

	opts = append(opts, grpc.WithDefaultServiceConfig(roundRobinServiceConfig))

	dialAddr := targetAddr
	if proxyURL := proxyURLFromEnv(targetAddr, tc.config.GrpcInsecure); proxyURL != nil {
		// Debug, not Info: this runs on every stream open, while the pool
		// usually hands back a connection that is already up. At Info it
		// reads as "a new proxy connection was just made", which is false
		// almost every time it prints. newProxyDialer logs the real dials.
		tc.logger.Debug("Using HTTP proxy for gRPC connection",
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

	// Shared with the HTTP client's transport: CA_CERT_PATH may be a file or
	// a directory of *.pem, and reading it two different ways is how the
	// tunnel ended up rejecting a path the rest of the agent accepted.
	caCert, err := util.ReadCACertPEM(tc.config.HttpCaCertFilePath)
	if err != nil {
		return nil, err
	}
	if len(caCert) > 0 {
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

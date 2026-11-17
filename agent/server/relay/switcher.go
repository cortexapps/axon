// Package relay runs the relay tunnel the agent selects, and moves the agent
// to a different tunnel when Cortex selects a different mode at runtime.
package relay

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/relaymode"
	"github.com/cortexapps/axon/server/grpctunnel"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// initialModeTimeout bounds the registration that selects the first mode.
// It must stay below fx's start timeout (15s), because Start runs in an
// OnStart hook. If registration does not answer in time, the agent starts
// with the default mode and switches when a later registration answers.
// A package variable so tests can shorten it.
var initialModeTimeout = 10 * time.Second

// defaultModeCheckInterval is how often the switcher asks Cortex for the
// mode when no registration from the running tunnel told it recently, and
// AutoRegisterFrequency is not set.
const defaultModeCheckInterval = 5 * time.Minute

// tunnel is a relay implementation that also serves the /broker routes.
type tunnel interface {
	snykbroker.RelayInstanceManager
	cortexHttp.RegisterableHandler
}

// tunnelFactory builds the tunnel for one mode. The tunnel must not mount
// its routes or hook into the fx lifecycle; the Switcher does both.
type tunnelFactory func(reg snykbroker.Registration) tunnel

type SwitcherParams struct {
	fx.In
	Lifecycle       fx.Lifecycle `optional:"true"`
	Config          config.AgentConfig
	Logger          *zap.Logger
	IntegrationInfo common.IntegrationInfo
	HttpServer      cortexHttp.Server
	Registration    snykbroker.Registration
	Status          *relaymode.Status
	Transport       *http.Transport                   `optional:"true"`
	Registry        *prometheus.Registry              `optional:"true"`
	Reflector       *snykbroker.RegistrationReflector `optional:"true"`
}

// Switcher runs one relay tunnel at a time and selects it by the relaymode
// precedence. When the configuration does not pin the mode, it reads the
// tunnelMode of every registration response, and it closes the running
// tunnel and starts the other one when the mode changes.
//
// Both tunnels serve the same /broker routes, so the Switcher mounts the
// routes one time and sends each request to the running tunnel.
type Switcher struct {
	config          config.AgentConfig
	logger          *zap.Logger
	integrationInfo common.IntegrationInfo
	registration    snykbroker.Registration
	status          *relaymode.Status
	factories       map[config.RelayMode]tunnelFactory

	// mu serializes Start, Restart, Close and switches. ServeHTTP holds
	// it for read, so a /broker request cannot restart a tunnel that a
	// switch closes.
	mu      sync.RWMutex
	running bool
	tunnels map[config.RelayMode]tunnel
	active  config.RelayMode
	routes  *mux.Router

	// wanted is the mode the last registration response selected.
	wanted       atomic.Value // config.RelayMode
	lastObserved atomic.Int64 // unix nanos of the last registration response
	wake         chan struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewSwitcher(p SwitcherParams) *Switcher {
	factories := map[config.RelayMode]tunnelFactory{
		config.RelayModeSnykBroker: func(reg snykbroker.Registration) tunnel {
			return snykbroker.NewRelayInstanceManager(snykbroker.RelayInstanceManagerParams{
				Config:          p.Config,
				Logger:          p.Logger,
				IntegrationInfo: p.IntegrationInfo,
				Registration:    reg,
				Transport:       p.Transport,
				Registry:        p.Registry,
				Reflector:       p.Reflector,
			}).(tunnel)
		},
		config.RelayModeGrpcTunnel: func(reg snykbroker.Registration) tunnel {
			return grpctunnel.NewTunnelClient(grpctunnel.TunnelClientParams{
				Config:          p.Config,
				Logger:          p.Logger,
				IntegrationInfo: p.IntegrationInfo,
				Registration:    reg,
				Transport:       p.Transport,
				Registry:        p.Registry,
			}).(tunnel)
		},
	}

	s := newSwitcher(p.Config, p.Logger, p.IntegrationInfo, p.Registration, p.Status, factories)
	p.HttpServer.RegisterHandler(s)

	if p.Lifecycle != nil {
		p.Lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error { return s.Start() },
			OnStop:  func(ctx context.Context) error { return s.Close() },
		})
	}
	return s
}

func newSwitcher(
	cfg config.AgentConfig,
	logger *zap.Logger,
	info common.IntegrationInfo,
	reg snykbroker.Registration,
	status *relaymode.Status,
	factories map[config.RelayMode]tunnelFactory,
) *Switcher {
	s := &Switcher{
		config:          cfg,
		logger:          logger.Named("relay-mode"),
		integrationInfo: info,
		status:          status,
		factories:       factories,
		tunnels:         map[config.RelayMode]tunnel{},
		wake:            make(chan struct{}, 1),
	}
	s.registration = &observedRegistration{inner: reg, observe: s.observe}
	return s
}

// dynamic reports whether the mode can change at runtime: the configuration
// does not pin it, and the agent registers with Cortex.
func (s *Switcher) dynamic() bool {
	if s.config.RelayModePinned() || s.config.DryRun {
		return false
	}
	// The tunnels skip registration when both of these are set.
	return os.Getenv("BROKER_SERVER_URL") == "" || os.Getenv("BROKER_TOKEN") == ""
}

func (s *Switcher) modeCheckInterval() time.Duration {
	if s.config.AutoRegisterFrequency > 0 {
		return s.config.AutoRegisterFrequency
	}
	return defaultModeCheckInterval
}

// Start selects the first mode and starts its tunnel.
func (s *Switcher) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return fmt.Errorf("already started")
	}
	s.running = true

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	mode, source := s.initialMode(ctx)
	s.logger.Info("Selected relay mode",
		zap.String("mode", string(mode)), zap.String("source", source))

	// A failed first start fails the agent start, as it did before the
	// switcher, so there is nothing to check later.
	if err := s.startLocked(mode); err != nil {
		cancel()
		s.running = false
		return err
	}

	if s.dynamic() {
		s.wg.Add(1)
		go s.loop(ctx)
	}
	return nil
}

// initialMode returns the first mode and a description of where it came
// from, for the log.
func (s *Switcher) initialMode(ctx context.Context) (config.RelayMode, string) {
	if m, ok := relaymode.Pinned(s.config); ok {
		return m, "configuration"
	}
	if !s.dynamic() {
		return relaymode.Default, "default"
	}

	ctx, cancel := context.WithTimeout(ctx, initialModeTimeout)
	defer cancel()
	if _, err := s.registration.Register(ctx, s.integrationInfo.Integration, s.integrationInfo.Alias); err != nil {
		s.logger.Warn("Registration failed, starting with the default relay mode", zap.Error(err))
		return relaymode.Default, "default"
	}
	// observe stored the mode from the response.
	return s.wantedMode(), "registration"
}

// startLocked starts the tunnel for mode, and builds it on first use. The
// caller holds mu.
func (s *Switcher) startLocked(mode config.RelayMode) error {
	t, ok := s.tunnels[mode]
	if !ok {
		t = s.factories[mode](s.registration)
		s.tunnels[mode] = t
	}

	routes := mux.NewRouter()
	if err := t.RegisterRoutes(routes); err != nil {
		return fmt.Errorf("error mounting %s routes: %w", mode, err)
	}

	s.active = mode
	s.routes = routes
	s.status.Set(mode)
	return t.Start()
}

// observe records the mode of a registration response. It runs on the
// goroutine that registered, which can be a tunnel goroutine that Close
// waits for, so it must not block or switch here.
func (s *Switcher) observe(resp *snykbroker.RegistrationInfoResponse) {
	mode, known := relaymode.FromRegistration(resp.TunnelMode)
	if !known {
		s.logger.Warn("Ignoring unknown tunnelMode from registration, using the default",
			zap.String("tunnelMode", resp.TunnelMode), zap.String("default", string(relaymode.Default)))
	}
	s.lastObserved.Store(time.Now().UnixNano())
	if prev, _ := s.wanted.Swap(mode).(config.RelayMode); prev != mode {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Switcher) wantedMode() config.RelayMode {
	if m, ok := s.wanted.Load().(config.RelayMode); ok {
		return m
	}
	return relaymode.Default
}

// loop switches the tunnel when the mode changes. The running tunnel
// re-registers on its own schedule. When it has not done so for a check
// interval, the loop registers to read the mode.
func (s *Switcher) loop(ctx context.Context) {
	defer s.wg.Done()

	interval := s.modeCheckInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastObserved.Load())) >= interval {
				if _, err := s.registration.Register(ctx, s.integrationInfo.Integration, s.integrationInfo.Alias); err != nil {
					s.logger.Warn("Registration to check the relay mode failed", zap.Error(err))
				}
			}
		}
		s.reconcile()
	}
}

// reconcile moves to the wanted mode if a different one runs.
func (s *Switcher) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := s.wantedMode()
	if !s.running || want == s.active {
		return
	}
	from := s.active

	// Close the running tunnel before the other one starts, so the agent
	// never holds both connections for the same token at the same time.
	s.logger.Info("Switching relay mode",
		zap.String("from", string(from)), zap.String("to", string(want)))
	if err := s.tunnels[from].Close(); err != nil {
		s.logger.Error("Error closing relay tunnel", zap.String("mode", string(from)), zap.Error(err))
	}

	err := s.startLocked(want)
	if err == nil {
		return
	}

	// Go back to the tunnel that ran. The next reconcile tries again.
	s.logger.Error("Error starting relay tunnel, restarting the previous one",
		zap.String("mode", string(want)), zap.Error(err))
	if err := s.tunnels[want].Close(); err != nil {
		s.logger.Error("Error closing relay tunnel", zap.String("mode", string(want)), zap.Error(err))
	}
	if err := s.startLocked(from); err != nil {
		s.logger.Error("Error restarting relay tunnel", zap.String("mode", string(from)), zap.Error(err))
	}
}

// Restart restarts the running tunnel.
func (s *Switcher) Restart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return fmt.Errorf("not started")
	}
	return s.tunnels[s.active].Restart()
}

// Close stops the mode checks and closes the running tunnel.
func (s *Switcher) Close() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	cancel := s.cancel
	s.mu.Unlock()

	// The loop takes mu in reconcile, so wait for it without holding mu.
	cancel()
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tunnels[s.active].Close()
}

// RegisterRoutes mounts the /broker routes. Each request goes to the
// running tunnel.
func (s *Switcher) RegisterRoutes(mux *mux.Router) error {
	mux.PathPrefix(fmt.Sprintf("%s/broker", cortexHttp.AxonPathRoot)).Handler(s)
	return nil
}

func (s *Switcher) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.routes == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.routes.ServeHTTP(w, req)
}

// observedRegistration gives each successful registration response to
// observe before it returns it.
type observedRegistration struct {
	inner   snykbroker.Registration
	observe func(*snykbroker.RegistrationInfoResponse)
}

func (r *observedRegistration) Register(ctx context.Context, integration common.Integration, alias string) (*snykbroker.RegistrationInfoResponse, error) {
	resp, err := r.inner.Register(ctx, integration, alias)
	if err == nil && resp != nil {
		r.observe(resp)
	}
	return resp, err
}

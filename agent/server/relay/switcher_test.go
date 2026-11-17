package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/relaymode"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeRegistration answers with the tunnelMode the test sets.
type fakeRegistration struct {
	mu         sync.Mutex
	tunnelMode string
	err        error
	calls      int
}

func (f *fakeRegistration) Register(ctx context.Context, _ common.Integration, _ string) (*snykbroker.RegistrationInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &snykbroker.RegistrationInfoResponse{ServerUri: "https://relay", Token: "t", TunnelMode: f.tunnelMode}, nil
}

func (f *fakeRegistration) set(tunnelMode string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tunnelMode = tunnelMode
	f.err = err
}

func (f *fakeRegistration) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeTunnel records its lifecycle and keeps the registration it was built
// with, so a test can register through it like a real tunnel does.
type fakeTunnel struct {
	mode     config.RelayMode
	reg      snykbroker.Registration
	startErr error

	mu       sync.Mutex
	running  bool
	starts   int
	closes   int
	restarts int
}

func (f *fakeTunnel) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}

func (f *fakeTunnel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.running = false
	return nil
}

func (f *fakeTunnel) Restart() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return nil
}

func (f *fakeTunnel) isRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeTunnel) RegisterRoutes(m *mux.Router) error {
	m.HandleFunc(cortexHttp.AxonPathRoot+"/broker/systemcheck", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(f.mode))
	})
	return nil
}

func (f *fakeTunnel) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

type harness struct {
	switcher *Switcher
	reg      *fakeRegistration
	status   *relaymode.Status
	mu       sync.Mutex
	tunnels  map[config.RelayMode]*fakeTunnel
	startErr map[config.RelayMode]error
}

func newHarness(t *testing.T, cfg config.AgentConfig, reg *fakeRegistration) *harness {
	t.Helper()
	t.Setenv("BROKER_SERVER_URL", "")
	t.Setenv("BROKER_TOKEN", "")

	h := &harness{
		reg:      reg,
		status:   relaymode.NewStatus(),
		tunnels:  map[config.RelayMode]*fakeTunnel{},
		startErr: map[config.RelayMode]error{},
	}
	factory := func(mode config.RelayMode) tunnelFactory {
		return func(r snykbroker.Registration) tunnel {
			h.mu.Lock()
			defer h.mu.Unlock()
			ft := &fakeTunnel{mode: mode, reg: r, startErr: h.startErr[mode]}
			h.tunnels[mode] = ft
			return ft
		}
	}
	h.switcher = newSwitcher(cfg, zap.NewNop(), common.IntegrationInfo{Integration: common.IntegrationGithub, Alias: "a"},
		reg, h.status, map[config.RelayMode]tunnelFactory{
			config.RelayModeSnykBroker: factory(config.RelayModeSnykBroker),
			config.RelayModeGrpcTunnel: factory(config.RelayModeGrpcTunnel),
		})
	t.Cleanup(func() { h.switcher.Close() })
	return h
}

func (h *harness) tunnel(mode config.RelayMode) *fakeTunnel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tunnels[mode]
}

func (h *harness) running(mode config.RelayMode) bool {
	t := h.tunnel(mode)
	return t != nil && t.isRunning()
}

func (h *harness) systemcheck(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m := mux.NewRouter()
	require.NoError(t, h.switcher.RegisterRoutes(m))
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cortexHttp.AxonPathRoot+"/broker/systemcheck", nil))
	return rec.Body.String()
}

func dynamicConfig() config.AgentConfig {
	return config.AgentConfig{AutoRegisterFrequency: time.Hour}
}

func TestSwitcher_ConfigurationPinsTheMode(t *testing.T) {
	cfg := dynamicConfig()
	cfg.RelayMode = string(config.RelayModeSnykBroker)
	reg := &fakeRegistration{tunnelMode: "grpc-tunnel"}
	h := newHarness(t, cfg, reg)

	require.NoError(t, h.switcher.Start())
	assert.True(t, h.running(config.RelayModeSnykBroker))
	assert.Equal(t, 0, reg.callCount(), "a pinned mode needs no registration to select it")

	// A registration from the tunnel that asks for the other mode changes nothing.
	_, err := h.tunnel(config.RelayModeSnykBroker).reg.Register(context.Background(), common.IntegrationGithub, "a")
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	assert.Nil(t, h.tunnel(config.RelayModeGrpcTunnel))
	assert.Equal(t, config.RelayModeSnykBroker, h.status.Active())
}

func TestSwitcher_RegistrationSelectsTheFirstMode(t *testing.T) {
	h := newHarness(t, dynamicConfig(), &fakeRegistration{tunnelMode: "grpc-tunnel"})

	require.NoError(t, h.switcher.Start())
	assert.True(t, h.running(config.RelayModeGrpcTunnel))
	assert.Nil(t, h.tunnel(config.RelayModeSnykBroker), "the other tunnel is not built")
	assert.Equal(t, config.RelayModeGrpcTunnel, h.status.Active())
	assert.Equal(t, "grpc-tunnel", h.systemcheck(t))
}

func TestSwitcher_NoPreferenceUsesTheDefault(t *testing.T) {
	h := newHarness(t, dynamicConfig(), &fakeRegistration{})

	require.NoError(t, h.switcher.Start())
	assert.True(t, h.running(relaymode.Default))
}

func TestSwitcher_UnknownModeUsesTheDefault(t *testing.T) {
	h := newHarness(t, dynamicConfig(), &fakeRegistration{tunnelMode: "carrier-pigeon"})

	require.NoError(t, h.switcher.Start())
	assert.True(t, h.running(relaymode.Default))
}

func TestSwitcher_FailedRegistrationStartsTheDefault(t *testing.T) {
	reg := &fakeRegistration{err: errors.New("cortex is down")}
	h := newHarness(t, dynamicConfig(), reg)

	require.NoError(t, h.switcher.Start())
	assert.True(t, h.running(relaymode.Default))

	// When the tunnel's own registration succeeds later, the agent follows it.
	reg.set("grpc-tunnel", nil)
	_, err := h.tunnel(relaymode.Default).reg.Register(context.Background(), common.IntegrationGithub, "a")
	require.NoError(t, err)

	require.Eventually(t, func() bool { return h.running(config.RelayModeGrpcTunnel) }, time.Second, 5*time.Millisecond)
	assert.False(t, h.running(relaymode.Default))
}

func TestSwitcher_FollowsAChangeAndBack(t *testing.T) {
	reg := &fakeRegistration{}
	h := newHarness(t, dynamicConfig(), reg)
	require.NoError(t, h.switcher.Start())
	require.True(t, h.running(config.RelayModeSnykBroker))

	reg.set("grpc-tunnel", nil)
	_, err := h.tunnel(config.RelayModeSnykBroker).reg.Register(context.Background(), common.IntegrationGithub, "a")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.running(config.RelayModeGrpcTunnel) }, time.Second, 5*time.Millisecond)
	assert.False(t, h.running(config.RelayModeSnykBroker))
	assert.Equal(t, config.RelayModeGrpcTunnel, h.status.Active())
	assert.Equal(t, "grpc-tunnel", h.systemcheck(t), "routes follow the running tunnel")

	// Clearing the flag in Cortex returns the agent to the default.
	reg.set("", nil)
	_, err = h.tunnel(config.RelayModeGrpcTunnel).reg.Register(context.Background(), common.IntegrationGithub, "a")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.running(config.RelayModeSnykBroker) }, time.Second, 5*time.Millisecond)
	assert.False(t, h.running(config.RelayModeGrpcTunnel))

	snyk := h.tunnel(config.RelayModeSnykBroker)
	assert.Equal(t, 2, snyk.starts, "the switcher reuses a tunnel it built before")
}

func TestSwitcher_PollsWhenTheTunnelDoesNotRegister(t *testing.T) {
	reg := &fakeRegistration{}
	cfg := dynamicConfig()
	cfg.AutoRegisterFrequency = 20 * time.Millisecond
	h := newHarness(t, cfg, reg)
	require.NoError(t, h.switcher.Start())
	require.True(t, h.running(config.RelayModeSnykBroker))

	reg.set("grpc-tunnel", nil)
	require.Eventually(t, func() bool { return h.running(config.RelayModeGrpcTunnel) }, 2*time.Second, 5*time.Millisecond)
}

func TestSwitcher_FailedStartReturnsToThePreviousMode(t *testing.T) {
	reg := &fakeRegistration{}
	h := newHarness(t, dynamicConfig(), reg)
	h.startErr[config.RelayModeGrpcTunnel] = errors.New("no tunnel for you")
	require.NoError(t, h.switcher.Start())

	reg.set("grpc-tunnel", nil)
	_, err := h.tunnel(config.RelayModeSnykBroker).reg.Register(context.Background(), common.IntegrationGithub, "a")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		snyk := h.tunnel(config.RelayModeSnykBroker)
		snyk.mu.Lock()
		defer snyk.mu.Unlock()
		return snyk.starts == 2 && snyk.running
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, config.RelayModeSnykBroker, h.status.Active())
	assert.Equal(t, "snyk-broker", h.systemcheck(t))
}

func TestSwitcher_DryRunDoesNotRegister(t *testing.T) {
	cfg := dynamicConfig()
	cfg.DryRun = true
	reg := &fakeRegistration{tunnelMode: "grpc-tunnel"}
	h := newHarness(t, cfg, reg)

	require.NoError(t, h.switcher.Start())
	assert.True(t, h.running(relaymode.Default))
	assert.Equal(t, 0, reg.callCount())
}

func TestSwitcher_RestartAndClose(t *testing.T) {
	h := newHarness(t, dynamicConfig(), &fakeRegistration{tunnelMode: "grpc-tunnel"})
	require.NoError(t, h.switcher.Start())

	require.NoError(t, h.switcher.Restart())
	assert.Equal(t, 1, h.tunnel(config.RelayModeGrpcTunnel).restarts)

	require.NoError(t, h.switcher.Close())
	assert.False(t, h.running(config.RelayModeGrpcTunnel))
	require.NoError(t, h.switcher.Close(), "a second Close does nothing")
}

func TestSwitcher_RoutesBeforeStart(t *testing.T) {
	h := newHarness(t, dynamicConfig(), &fakeRegistration{})
	rec := httptest.NewRecorder()
	h.switcher.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cortexHttp.AxonPathRoot+"/broker/systemcheck", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestSwitcher_FailedFirstStartFailsStart(t *testing.T) {
	h := newHarness(t, dynamicConfig(), &fakeRegistration{tunnelMode: "grpc-tunnel"})
	h.startErr[config.RelayModeGrpcTunnel] = errors.New("no tunnel for you")

	require.Error(t, h.switcher.Start())
	require.NoError(t, h.switcher.Close(), "Close after a failed Start does nothing")
	assert.Equal(t, 0, h.tunnel(config.RelayModeGrpcTunnel).closes)
}

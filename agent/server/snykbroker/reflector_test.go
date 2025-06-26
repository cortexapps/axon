package snykbroker

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

type testReflectorEnv struct {
	Reflector *RegistrationReflector
	Server    *httptest.Server
	Router    *mux.Router
}

func newTestReflectorEnv(t *testing.T) *testReflectorEnv {
	logger := zaptest.NewLogger(t)
	rr := NewRegistrationReflector(RegistrationReflectorParams{
		Logger:   logger,
		Registry: prometheus.NewRegistry(),
	})
	router := mux.NewRouter()
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	t.Cleanup(func() { rr.Stop() })
	return &testReflectorEnv{
		Reflector: rr,
		Server:    server,
		Router:    router,
	}
}

func TestGetProxyAndProxyURI(t *testing.T) {
	env := newTestReflectorEnv(t)
	target := env.Server.URL
	proxyEntry, err := env.Reflector.getProxy(target, false, nil)
	require.NoError(t, err)
	require.NotNil(t, proxyEntry)
	require.Equal(t, target, proxyEntry.TargetURI)

	uri := env.Reflector.ProxyURI(target)
	require.Contains(t, uri, "localhost")
}

func TestDefaultProxyURI(t *testing.T) {
	env := newTestReflectorEnv(t)
	target := env.Server.URL
	uri := env.Reflector.ProxyURI(target, WithDefault(true))
	require.Equal(t, fmt.Sprintf("http://localhost:%d", env.Reflector.server.Port()), uri)
}

func TestServeHTTP_Proxy(t *testing.T) {
	env := newTestReflectorEnv(t)

	env.Router.Handle("/get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("Proxy response"))
	}))

	target := env.Server.URL
	// Build a request to the proxy path
	proxyUri := env.Reflector.ProxyURI(target)
	req, _ := http.NewRequest("GET", proxyUri+"/get", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
}

func TestServeHTTP_InvalidTarget(t *testing.T) {
	env := newTestReflectorEnv(t)
	env.Reflector.Start()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/!99999999!/foo", env.Reflector.server.Port()), nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestServeHTTP_DefaultTarget(t *testing.T) {
	env := newTestReflectorEnv(t)

	env.Router.Handle("/get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("Proxy response"))
	}))

	target := env.Server.URL
	// Build a request to the proxy path
	proxyUri := env.Reflector.ProxyURI(target, WithDefault(true))
	req, _ := http.NewRequest("GET", proxyUri+"/get", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
}

func TestGetHash(t *testing.T) {
	env := newTestReflectorEnv(t)
	hash := env.Reflector.extractHash("!12345!")
	require.Equal(t, "12345", hash)
	require.Equal(t, "", env.Reflector.extractHash("12345"))
	require.Equal(t, "", env.Reflector.extractHash("!bad"))
}

func TestStop(t *testing.T) {
	env := newTestReflectorEnv(t)
	err := env.Reflector.Stop()
	require.NoError(t, err)
}

func TestStartTwice(t *testing.T) {
	env := newTestReflectorEnv(t)
	port1, err1 := env.Reflector.Start()
	port2, err2 := env.Reflector.Start()
	require.NoError(t, err1)
	require.NoError(t, err2)
	require.Equal(t, port1, port2)
}

func TestModuleStartup(t *testing.T) {

	cases := []struct {
		name     string
		cfg      config.AgentConfig
		expected bool
	}{
		{
			name:     "Disabled",
			cfg:      config.AgentConfig{HttpRelayReflectorMode: config.RelayReflectorDisabled},
			expected: false,
		},
		{
			name:     "All",
			cfg:      config.AgentConfig{HttpRelayReflectorMode: config.RelayReflectorAllTraffic},
			expected: true,
		},
		{
			name:     "All",
			cfg:      config.AgentConfig{HttpRelayReflectorMode: config.RelayReflectorRegistrationOnly},
			expected: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.AgentConfig{
				HttpRelayReflectorMode: tc.cfg.HttpRelayReflectorMode,
			}
			result := MaybeNewRegistrationReflector(cfg, RegistrationReflectorParams{
				Logger: zaptest.NewLogger(t),
			})
			if tc.expected {
				require.NotNil(t, result, "Expected reflector to be created")
			} else {
				require.Nil(t, result, "Expected reflector to be nil")
			}
		})
	}
}

package snykbroker

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
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
	return newTestReflectorEnvWithConfig(t, config.AgentConfig{
		ReflectorWebSocketUpgrade: true,
	})
}

func newTestReflectorEnvWithConfig(t *testing.T, cfg config.AgentConfig) *testReflectorEnv {
	logger := zaptest.NewLogger(t)
	rr := NewRegistrationReflector(RegistrationReflectorParams{
		Logger:   logger,
		Registry: prometheus.NewRegistry(),
		Config:   cfg,
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
			name:     "AllTraffic",
			cfg:      config.AgentConfig{HttpRelayReflectorMode: config.RelayReflectorAllTraffic},
			expected: true,
		},
		{
			name:     "RegistrationOnly",
			cfg:      config.AgentConfig{HttpRelayReflectorMode: config.RelayReflectorRegistrationOnly},
			expected: true,
		},
		{
			name:     "TrafficOnly",
			cfg:      config.AgentConfig{HttpRelayReflectorMode: config.RelayReflectorTrafficOnly},
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

// WebSocket upgrader for test server
var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestWebSocketUpgradeDetection(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Test that WebSocket upgrade is detected
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	require.True(t, env.Reflector.isWebSocketUpgrade(req))

	// Test that normal request is not detected as WebSocket
	req2, _ := http.NewRequest("GET", "/test", nil)
	require.False(t, env.Reflector.isWebSocketUpgrade(req2))

	// Test case insensitivity
	req3, _ := http.NewRequest("GET", "/test", nil)
	req3.Header.Set("Connection", "upgrade")
	req3.Header.Set("Upgrade", "WebSocket")
	require.True(t, env.Reflector.isWebSocketUpgrade(req3))
}

func TestWebSocketUpgradeDisabled(t *testing.T) {
	env := newTestReflectorEnvWithConfig(t, config.AgentConfig{
		ReflectorWebSocketUpgrade: false,
	})

	// WebSocket upgrade headers present but feature disabled
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	require.False(t, env.Reflector.isWebSocketUpgrade(req), "WebSocket upgrade should be disabled")
}

func TestWebSocketProxyFullFlow(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Create a WebSocket echo server
	env.Router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// Echo messages back
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if err := conn.WriteMessage(messageType, message); err != nil {
				break
			}
		}
	})

	// Get proxy URI for the WebSocket server
	proxyURI := env.Reflector.ProxyURI(env.Server.URL)

	// Connect to WebSocket through the reflector
	wsURL := "ws" + proxyURI[4:] + "/ws" // Convert http:// to ws://
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err, "WebSocket dial should succeed")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	// Send a message
	testMessage := []byte("Hello WebSocket!")
	err = conn.WriteMessage(websocket.TextMessage, testMessage)
	require.NoError(t, err, "WebSocket write should succeed")

	// Receive the echo
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err, "WebSocket read should succeed")
	require.Equal(t, websocket.TextMessage, messageType)
	require.Equal(t, testMessage, message, "Echo message should match")
}

func TestWebSocketProxyWithDefaultTarget(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Create a WebSocket echo server
	env.Router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// Echo with prefix
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				break
			}
			reply := append([]byte("echo: "), message...)
			if err := conn.WriteMessage(messageType, reply); err != nil {
				break
			}
		}
	})

	// Use default proxy (no hash in path)
	proxyURI := env.Reflector.ProxyURI(env.Server.URL, WithDefault(true))

	// Connect to WebSocket through the reflector
	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err, "WebSocket dial should succeed")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	// Send and receive
	testMessage := []byte("test")
	err = conn.WriteMessage(websocket.TextMessage, testMessage)
	require.NoError(t, err)

	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, []byte("echo: test"), message)
}

func TestWebSocketProxyConnectionRefused(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Register a proxy to a non-existent server (connection refused)
	proxyURI := env.Reflector.ProxyURI("http://127.0.0.1:59999") // Port that's not listening

	// Try to connect via WebSocket
	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	_, resp, err := dialer.Dial(wsURL, nil)

	// Should get an error with a 502 response
	require.Error(t, err)
	require.NotNil(t, resp, "Expected HTTP response with status code")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "Expected 502 Bad Gateway")
}

func TestRecordTrafficAndLastTrafficTime(t *testing.T) {
	env := newTestReflectorEnv(t)

	// LastTrafficTime should be initialized (set in constructor)
	initial := env.Reflector.LastTrafficTime()
	require.False(t, initial.IsZero(), "LastTrafficTime should be initialized")

	// Wait briefly and record traffic
	time.Sleep(10 * time.Millisecond)
	env.Reflector.RecordTraffic()
	updated := env.Reflector.LastTrafficTime()
	require.True(t, updated.After(initial), "LastTrafficTime should advance after RecordTraffic")
}

func TestServeHTTPUpdatesLastTrafficTime(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Set up a target server
	env.Router.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	proxyURI := env.Reflector.ProxyURI(env.Server.URL)

	initial := env.Reflector.LastTrafficTime()
	time.Sleep(10 * time.Millisecond)

	// Make a request through the reflector
	resp, err := http.Get(proxyURI + "/hello")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// LastTrafficTime should have been updated
	afterRequest := env.Reflector.LastTrafficTime()
	require.True(t, afterRequest.After(initial), "LastTrafficTime should update on ServeHTTP")
}

func TestWebSocketProxyInvalidTarget(t *testing.T) {
	env := newTestReflectorEnv(t)
	env.Reflector.Start()

	// Make a WebSocket request to an invalid hash (no registered target)
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/!invalid!/ws", env.Reflector.server.Port()), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should get 502 Bad Gateway for invalid target
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestWSTunnelTracking(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Initially, tunnel should not be connected
	require.False(t, env.Reflector.IsWSTunnelConnected())

	// Create a WebSocket echo server
	env.Router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				break
			}
			conn.WriteMessage(messageType, message)
		}
	})

	proxyURI := env.Reflector.ProxyURI(env.Server.URL, WithDefault(true))

	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	// Tunnel should now be connected
	time.Sleep(50 * time.Millisecond)
	require.True(t, env.Reflector.IsWSTunnelConnected())

	// Close the WebSocket connection
	conn.Close()

	// Wait for the tunnel to detect the close
	time.Sleep(100 * time.Millisecond)
	require.False(t, env.Reflector.IsWSTunnelConnected())
}

func TestWSTunnelCloseCallback(t *testing.T) {
	env := newTestReflectorEnv(t)
	// Use a short min duration so the test doesn't need to wait 30s
	env.Reflector.wsTunnelMinDurationOverride = 50 * time.Millisecond

	callbackCalled := make(chan struct{}, 1)
	env.Reflector.SetOnWSTunnelClose(func() {
		callbackCalled <- struct{}{}
	})

	env.Router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	})

	proxyURI := env.Reflector.ProxyURI(env.Server.URL, WithDefault(true))
	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)

	// Keep tunnel open past the min duration override
	time.Sleep(100 * time.Millisecond)
	require.True(t, env.Reflector.IsWSTunnelConnected())

	// Close the connection — should trigger callback
	conn.Close()

	select {
	case <-callbackCalled:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Expected tunnel close callback to be called")
	}

	require.False(t, env.Reflector.IsWSTunnelConnected())
}

func TestWSTunnelCloseCallbackSkippedForShortLivedTunnel(t *testing.T) {
	env := newTestReflectorEnv(t)
	// Set a long min duration so the tunnel is considered short-lived
	env.Reflector.wsTunnelMinDurationOverride = 10 * time.Second

	callbackCalled := make(chan struct{}, 1)
	env.Reflector.SetOnWSTunnelClose(func() {
		callbackCalled <- struct{}{}
	})

	env.Router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	})

	proxyURI := env.Reflector.ProxyURI(env.Server.URL, WithDefault(true))
	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	require.True(t, env.Reflector.IsWSTunnelConnected())

	// Close immediately — should NOT trigger callback (tunnel too short-lived)
	conn.Close()

	select {
	case <-callbackCalled:
		t.Fatal("Callback should not be called for short-lived tunnel")
	case <-time.After(500 * time.Millisecond):
		// Expected: callback was not called
	}

	require.False(t, env.Reflector.IsWSTunnelConnected())
}

func TestWebSocketProxyServerRejectsUpgrade(t *testing.T) {
	env := newTestReflectorEnv(t)

	// Create a server that rejects WebSocket upgrades
	env.Router.HandleFunc("/reject", func(w http.ResponseWriter, r *http.Request) {
		// Don't upgrade, just return a normal HTTP response
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("WebSocket not supported"))
	})

	proxyURI := env.Reflector.ProxyURI(env.Server.URL)
	wsURL := "ws" + proxyURI[4:] + "/reject"

	dialer := websocket.Dialer{}
	_, _, err := dialer.Dial(wsURL, nil)

	// Should get an error because server rejected the upgrade
	require.Error(t, err, "Expected error when server rejects WebSocket upgrade")
}

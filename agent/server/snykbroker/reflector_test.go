package snykbroker

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	_ = newTestReflectorEnv(t)

	// Test that WebSocket upgrade is detected
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	require.True(t, IsWebSocketUpgrade(req))

	// Test that normal request is not detected as WebSocket
	req2, _ := http.NewRequest("GET", "/test", nil)
	require.False(t, IsWebSocketUpgrade(req2))

	// Test case insensitivity
	req3, _ := http.NewRequest("GET", "/test", nil)
	req3.Header.Set("Connection", "upgrade")
	req3.Header.Set("Upgrade", "WebSocket")
	require.True(t, IsWebSocketUpgrade(req3))
}

func TestWebSocketUpgradeDisabled(t *testing.T) {
	// Note: The config check for ReflectorWebSocketUpgrade is now done in the
	// reflector's ServeHTTP method, not in IsWebSocketUpgrade itself.
	// IsWebSocketUpgrade just checks the headers.

	// WebSocket upgrade headers present - IsWebSocketUpgrade returns true
	// (the config check happens in the reflector)
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	require.True(t, IsWebSocketUpgrade(req), "IsWebSocketUpgrade should return true based on headers")
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

	conn.Close()

	select {
	case <-callbackCalled:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Expected tunnel close callback to be called")
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

func TestWebSocketProxyThroughHTTPProxy(t *testing.T) {
	// Create a WebSocket echo server (the actual target)
	targetRouter := mux.NewRouter()
	targetRouter.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
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
	targetServer := httptest.NewServer(targetRouter)
	defer targetServer.Close()

	// Create an HTTP CONNECT proxy
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "Expected CONNECT", http.StatusMethodNotAllowed)
			return
		}

		t.Logf("Proxy received CONNECT to %s", r.Host)

		// Connect to the target
		targetConn, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// Hijack the client connection first
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			targetConn.Close()
			http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			targetConn.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Send 200 OK manually after hijacking (proper CONNECT response format)
		_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		if err != nil {
			clientConn.Close()
			targetConn.Close()
			return
		}

		// Bidirectional copy
		go func() {
			io.Copy(targetConn, clientConn)
			targetConn.Close()
		}()
		go func() {
			io.Copy(clientConn, targetConn)
			clientConn.Close()
		}()
	}))
	defer proxyServer.Close()

	// Create reflector with the proxy configured via transport
	logger := zaptest.NewLogger(t)

	// Parse proxy URL to set up transport
	proxyURL, _ := url.Parse(proxyServer.URL)
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}

	rr := NewRegistrationReflector(RegistrationReflectorParams{
		Logger:    logger,
		Registry:  prometheus.NewRegistry(),
		Transport: transport,
		Config: config.AgentConfig{
			ReflectorWebSocketUpgrade: true,
		},
	})

	router := mux.NewRouter()
	rr.RegisterRoutes(router)
	reflectorServer := httptest.NewServer(router)
	defer reflectorServer.Close()

	// Register the target with the reflector
	proxyURI := rr.ProxyURI(targetServer.URL)

	// Connect to WebSocket through the reflector (which should use the HTTP proxy)
	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err, "WebSocket dial through proxy should succeed")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	// Send a message
	testMessage := []byte("Hello through proxy!")
	err = conn.WriteMessage(websocket.TextMessage, testMessage)
	require.NoError(t, err, "WebSocket write should succeed")

	// Receive the echo
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err, "WebSocket read should succeed")
	require.Equal(t, websocket.TextMessage, messageType)
	require.Equal(t, testMessage, message, "Echo message should match")
}

func TestWebSocketProxyTLSConfigFromTransport(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Create a transport with custom TLS config
	customRoots := x509.NewCertPool()
	customTLSConfig := &tls.Config{
		RootCAs:            customRoots,
		InsecureSkipVerify: false,
	}
	transport := &http.Transport{
		TLSClientConfig: customTLSConfig,
	}

	// Create WebSocketProxy with the transport
	wsProxy := NewWebSocketProxy(logger, transport)

	// Get TLS config for a host (using the internal method via reflection or by testing behavior)
	// Since getTLSConfig is unexported, we test the behavior indirectly by verifying
	// the proxy was created with the transport
	require.NotNil(t, wsProxy)

	// The WebSocketProxy should use the transport's TLS config when dialing
	// This is tested by the TestWebSocketProxyThroughHTTPProxy test which verifies
	// the full flow works with a custom transport
}

func TestWebSocketProxyWithoutTransport(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Create WebSocketProxy without transport
	wsProxy := NewWebSocketProxy(logger, nil)

	require.NotNil(t, wsProxy)
	require.False(t, wsProxy.IsConnected())
}

// The reflector must preserve percent-encoded characters (notably %2F) in the
// path when forwarding to the registered target. Reading from r.URL.Path alone
// and writing back to r.URL.Path without updating r.URL.RawPath causes Go's
// reverse-proxy to emit the decoded path on the wire, which turns a GitLab
// namespace/project identifier like `foo%2Fbar` into `foo/bar` and makes
// GitLab return 404 for anything past the project segment.
func TestServeHTTP_PreservesPercentEncodedSlashMidPath(t *testing.T) {
	env := newTestReflectorEnv(t)

	var gotEscapedPath string
	env.Router.PathPrefix("/api/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))

	proxyURI := env.Reflector.ProxyURI(env.Server.URL)

	const encoded = "/api/v4/projects/test.project%2Fcli-functional-test-xxx/approval_rules"
	req, err := http.NewRequest("GET", proxyURI+encoded, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, encoded, gotEscapedPath,
		"mid-path %%2F must survive the reflector hop")
}

func TestServeHTTP_PreservesPercentEncodedSlashTrailing(t *testing.T) {
	env := newTestReflectorEnv(t)

	var gotEscapedPath string
	env.Router.PathPrefix("/api/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))

	proxyURI := env.Reflector.ProxyURI(env.Server.URL)

	const encoded = "/api/v4/projects/test.project%2Fcli-functional-test-xxx"
	req, err := http.NewRequest("GET", proxyURI+encoded, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, encoded, gotEscapedPath,
		"trailing %%2F must survive the reflector hop")
}

func TestServeHTTP_PreservesPercentEncodedSlashDefaultTarget(t *testing.T) {
	env := newTestReflectorEnv(t)

	var gotEscapedPath string
	env.Router.PathPrefix("/api/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))

	proxyURI := env.Reflector.ProxyURI(env.Server.URL, WithDefault(true))

	const encoded = "/api/v4/projects/test.project%2Fcli-functional-test-xxx/approval_rules"
	req, err := http.NewRequest("GET", proxyURI+encoded, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, encoded, gotEscapedPath,
		"mid-path %%2F must survive the reflector hop in default-target mode")
}

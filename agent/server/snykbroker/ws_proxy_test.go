package snykbroker

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestNewWebSocketProxy(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("with transport", func(t *testing.T) {
		transport := &http.Transport{}
		wsProxy := NewWebSocketProxy(logger, transport)

		require.NotNil(t, wsProxy)
		assert.Equal(t, 30*time.Second, wsProxy.DialTimeout)
		assert.Equal(t, 30*time.Second, wsProxy.HandshakeTimeout)
		assert.Equal(t, 5*time.Minute, wsProxy.IdleTimeout)
		assert.False(t, wsProxy.IsConnected())
	})

	t.Run("without transport", func(t *testing.T) {
		wsProxy := NewWebSocketProxy(logger, nil)

		require.NotNil(t, wsProxy)
		assert.False(t, wsProxy.IsConnected())
	})
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		wantResult bool
	}{
		{
			name: "valid websocket upgrade",
			headers: map[string]string{
				"Upgrade":    "websocket",
				"Connection": "Upgrade",
			},
			wantResult: true,
		},
		{
			name: "case insensitive upgrade header",
			headers: map[string]string{
				"Upgrade":    "WebSocket",
				"Connection": "upgrade",
			},
			wantResult: true,
		},
		{
			name: "connection with keep-alive",
			headers: map[string]string{
				"Upgrade":    "websocket",
				"Connection": "keep-alive, Upgrade",
			},
			wantResult: true,
		},
		{
			name: "missing upgrade header",
			headers: map[string]string{
				"Connection": "Upgrade",
			},
			wantResult: false,
		},
		{
			name: "missing connection header",
			headers: map[string]string{
				"Upgrade": "websocket",
			},
			wantResult: false,
		},
		{
			name: "wrong upgrade value",
			headers: map[string]string{
				"Upgrade":    "h2c",
				"Connection": "Upgrade",
			},
			wantResult: false,
		},
		{
			name:       "no headers",
			headers:    map[string]string{},
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/ws", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			result := IsWebSocketUpgrade(req)
			assert.Equal(t, tt.wantResult, result)
		})
	}
}

func TestWebSocketProxyResolveTargetAddr(t *testing.T) {
	logger := zaptest.NewLogger(t)
	wsProxy := NewWebSocketProxy(logger, nil)

	tests := []struct {
		name     string
		targetURI string
		wantAddr string
	}{
		{
			name:      "http with explicit port",
			targetURI: "http://example.com:8080",
			wantAddr:  "example.com:8080",
		},
		{
			name:      "http default port",
			targetURI: "http://example.com",
			wantAddr:  "example.com:80",
		},
		{
			name:      "https default port",
			targetURI: "https://example.com",
			wantAddr:  "example.com:443",
		},
		{
			name:      "wss default port",
			targetURI: "wss://example.com",
			wantAddr:  "example.com:443",
		},
		{
			name:      "ws default port",
			targetURI: "ws://example.com",
			wantAddr:  "example.com:80",
		},
		{
			name:      "https with explicit port",
			targetURI: "https://example.com:9443",
			wantAddr:  "example.com:9443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetURL, err := url.Parse(tt.targetURI)
			require.NoError(t, err)

			addr := wsProxy.resolveTargetAddr(targetURL)
			assert.Equal(t, tt.wantAddr, addr)
		})
	}
}

func TestWebSocketProxyGetTLSConfig(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("no TLS required", func(t *testing.T) {
		wsProxy := NewWebSocketProxy(logger, nil)
		cfg := wsProxy.getTLSConfig("example.com", false)
		assert.Nil(t, cfg)
	})

	t.Run("TLS required without transport", func(t *testing.T) {
		wsProxy := NewWebSocketProxy(logger, nil)
		cfg := wsProxy.getTLSConfig("example.com", true)

		require.NotNil(t, cfg)
		assert.Equal(t, "example.com", cfg.ServerName)
	})

	t.Run("TLS required with transport but no TLS config", func(t *testing.T) {
		transport := &http.Transport{}
		wsProxy := NewWebSocketProxy(logger, transport)
		cfg := wsProxy.getTLSConfig("example.com", true)

		require.NotNil(t, cfg)
		assert.Equal(t, "example.com", cfg.ServerName)
	})

	t.Run("TLS required with custom transport TLS config", func(t *testing.T) {
		customRoots := x509.NewCertPool()
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            customRoots,
				InsecureSkipVerify: true, // just for testing
			},
		}
		wsProxy := NewWebSocketProxy(logger, transport)
		cfg := wsProxy.getTLSConfig("example.com", true)

		require.NotNil(t, cfg)
		assert.Equal(t, "example.com", cfg.ServerName)
		assert.True(t, cfg.InsecureSkipVerify, "should inherit InsecureSkipVerify")
		assert.Equal(t, customRoots, cfg.RootCAs, "should inherit RootCAs")
	})
}

func TestWebSocketProxyGetProxyURL(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("transport with proxy function", func(t *testing.T) {
		proxyURL, _ := url.Parse("http://proxy.example.com:8080")
		transport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		wsProxy := NewWebSocketProxy(logger, transport)

		targetURL, _ := url.Parse("http://target.example.com")
		result, err := wsProxy.getProxyURL(targetURL)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "proxy.example.com:8080", result.Host)
	})

	t.Run("transport with nil proxy function", func(t *testing.T) {
		transport := &http.Transport{
			Proxy: nil,
		}
		wsProxy := NewWebSocketProxy(logger, transport)

		targetURL, _ := url.Parse("http://target.example.com")
		result, err := wsProxy.getProxyURL(targetURL)

		require.NoError(t, err)
		// Result depends on environment variables, but shouldn't error
		_ = result
	})

	t.Run("nil transport falls back to environment", func(t *testing.T) {
		wsProxy := NewWebSocketProxy(logger, nil)

		targetURL, _ := url.Parse("http://target.example.com")
		result, err := wsProxy.getProxyURL(targetURL)

		require.NoError(t, err)
		// Result depends on environment variables
		_ = result
	})
}

func TestWebSocketProxyDialDirect(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("successful connection", func(t *testing.T) {
		// Create a simple TCP server
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()

		go func() {
			conn, _ := listener.Accept()
			if conn != nil {
				conn.Close()
			}
		}()

		wsProxy := NewWebSocketProxy(logger, nil)
		wsProxy.DialTimeout = 5 * time.Second

		conn, err := wsProxy.dialDirect(listener.Addr().String(), nil)
		require.NoError(t, err)
		require.NotNil(t, conn)
		conn.Close()
	})

	t.Run("connection refused", func(t *testing.T) {
		wsProxy := NewWebSocketProxy(logger, nil)
		wsProxy.DialTimeout = 1 * time.Second

		conn, err := wsProxy.dialDirect("127.0.0.1:59999", nil)
		require.Error(t, err)
		assert.Nil(t, conn)
	})
}

func TestWebSocketProxyDialThroughProxy(t *testing.T) {
	logger := zaptest.NewLogger(t)

	t.Run("successful CONNECT through proxy", func(t *testing.T) {
		// Create target server
		targetListener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer targetListener.Close()

		var targetConnected atomic.Bool
		go func() {
			conn, _ := targetListener.Accept()
			if conn != nil {
				targetConnected.Store(true)
				io.Copy(io.Discard, conn)
				conn.Close()
			}
		}()

		// Create HTTP CONNECT proxy
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "CONNECT" {
				http.Error(w, "Expected CONNECT", http.StatusMethodNotAllowed)
				return
			}

			targetConn, err := net.Dial("tcp", r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}

			hijacker, ok := w.(http.Hijacker)
			if !ok {
				targetConn.Close()
				http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
				return
			}

			clientConn, _, err := hijacker.Hijack()
			if err != nil {
				targetConn.Close()
				return
			}

			clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

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

		proxyURL, _ := url.Parse(proxyServer.URL)
		wsProxy := NewWebSocketProxy(logger, nil)
		wsProxy.DialTimeout = 5 * time.Second
		wsProxy.HandshakeTimeout = 5 * time.Second

		conn, err := wsProxy.dialThroughProxy(proxyURL, targetListener.Addr().String(), nil)
		require.NoError(t, err)
		require.NotNil(t, conn)

		// Write something to trigger the target connection
		conn.Write([]byte("test"))
		conn.Close()

		time.Sleep(100 * time.Millisecond)
		assert.True(t, targetConnected.Load(), "should have connected to target through proxy")
	})

	t.Run("proxy rejects CONNECT", func(t *testing.T) {
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}))
		defer proxyServer.Close()

		proxyURL, _ := url.Parse(proxyServer.URL)
		wsProxy := NewWebSocketProxy(logger, nil)
		wsProxy.DialTimeout = 5 * time.Second
		wsProxy.HandshakeTimeout = 5 * time.Second

		conn, err := wsProxy.dialThroughProxy(proxyURL, "target.example.com:443", nil)
		require.Error(t, err)
		assert.Nil(t, conn)
		assert.Contains(t, err.Error(), "proxy rejected CONNECT")
	})

	t.Run("proxy with basic auth", func(t *testing.T) {
		var receivedAuth string
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuth = r.Header.Get("Proxy-Authorization")
			http.Error(w, "Auth checked", http.StatusForbidden)
		}))
		defer proxyServer.Close()

		proxyURL, _ := url.Parse(proxyServer.URL)
		proxyURL.User = url.UserPassword("user", "pass")

		wsProxy := NewWebSocketProxy(logger, nil)
		wsProxy.DialTimeout = 5 * time.Second
		wsProxy.HandshakeTimeout = 5 * time.Second

		_, err := wsProxy.dialThroughProxy(proxyURL, "target.example.com:443", nil)
		require.Error(t, err) // Will fail auth, but we just check auth was sent

		assert.NotEmpty(t, receivedAuth, "should have sent Proxy-Authorization header")
		assert.Contains(t, receivedAuth, "Basic ")
	})
}

func TestWebSocketProxyCallbacks(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Create a simple WebSocket echo server
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	router := mux.NewRouter()
	router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Just close immediately for this test
		time.Sleep(50 * time.Millisecond)
	})
	targetServer := httptest.NewServer(router)
	defer targetServer.Close()

	// Create reflector with the WebSocket proxy
	wsProxy := NewWebSocketProxy(logger, nil)
	wsProxy.IdleTimeout = 100 * time.Millisecond

	var establishedTarget string
	var closedTarget string
	var closedDuration time.Duration

	wsProxy.OnTunnelEstablished = func(target string) {
		establishedTarget = target
	}
	wsProxy.OnTunnelClosed = func(target string, duration time.Duration) {
		closedTarget = target
		closedDuration = duration
	}

	// Create a reflector to test through
	rr := NewRegistrationReflector(RegistrationReflectorParams{
		Logger: logger,
		Config: config.AgentConfig{
			ReflectorWebSocketUpgrade: true,
		},
	})

	reflectorRouter := mux.NewRouter()
	rr.RegisterRoutes(reflectorRouter)
	reflectorServer := httptest.NewServer(reflectorRouter)
	defer reflectorServer.Close()

	// Inject our WebSocket proxy with callbacks
	rr.wsProxy = wsProxy

	// Register target
	proxyURI := rr.ProxyURI(targetServer.URL)

	// Connect
	wsURL := "ws" + proxyURI[4:] + "/ws"
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)

	// Wait for tunnel to establish
	time.Sleep(50 * time.Millisecond)
	assert.NotEmpty(t, establishedTarget, "OnTunnelEstablished should have been called")

	// Close connection
	conn.Close()

	// Wait for tunnel to close
	time.Sleep(200 * time.Millisecond)
	assert.NotEmpty(t, closedTarget, "OnTunnelClosed should have been called")
	assert.True(t, closedDuration > 0, "duration should be positive")
}

func TestWebSocketProxyIsConnected(t *testing.T) {
	logger := zaptest.NewLogger(t)

	wsProxy := NewWebSocketProxy(logger, nil)
	assert.False(t, wsProxy.IsConnected(), "should not be connected initially")
	assert.Equal(t, int32(0), wsProxy.ActiveConnections(), "should have 0 active connections")

	// Manually increment active connections for testing
	wsProxy.activeConnections.Add(1)
	assert.True(t, wsProxy.IsConnected(), "should report connected with 1 connection")
	assert.Equal(t, int32(1), wsProxy.ActiveConnections(), "should have 1 active connection")

	// Add another connection
	wsProxy.activeConnections.Add(1)
	assert.True(t, wsProxy.IsConnected(), "should report connected with 2 connections")
	assert.Equal(t, int32(2), wsProxy.ActiveConnections(), "should have 2 active connections")

	// Remove one connection
	wsProxy.activeConnections.Add(-1)
	assert.True(t, wsProxy.IsConnected(), "should still report connected with 1 connection")
	assert.Equal(t, int32(1), wsProxy.ActiveConnections(), "should have 1 active connection")

	// Remove last connection
	wsProxy.activeConnections.Add(-1)
	assert.False(t, wsProxy.IsConnected(), "should report disconnected with 0 connections")
	assert.Equal(t, int32(0), wsProxy.ActiveConnections(), "should have 0 active connections")
}

func TestIsTimeoutError(t *testing.T) {
	t.Run("timeout error", func(t *testing.T) {
		// Create a connection and set a very short deadline
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()

		go func() {
			conn, _ := listener.Accept()
			if conn != nil {
				// Don't write anything, let it timeout
				time.Sleep(time.Second)
				conn.Close()
			}
		}()

		conn, err := net.Dial("tcp", listener.Addr().String())
		require.NoError(t, err)
		defer conn.Close()

		conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		time.Sleep(5 * time.Millisecond)

		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		require.Error(t, err)
		assert.True(t, isTimeoutError(err), "should be a timeout error")
	})

	t.Run("non-timeout error", func(t *testing.T) {
		// io.EOF is not a timeout error
		assert.False(t, isTimeoutError(io.EOF))
	})

	t.Run("nil error", func(t *testing.T) {
		assert.False(t, isTimeoutError(nil))
	})
}

func TestWebSocketProxyFullFlowThroughHTTPProxy(t *testing.T) {
	// Create a WebSocket echo server (the actual target)
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	targetRouter := mux.NewRouter()
	targetRouter.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
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

		targetConn, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			targetConn.Close()
			http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			targetConn.Close()
			return
		}

		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

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

	// Create WebSocketProxy with the proxy configured via transport
	logger := zaptest.NewLogger(t)
	proxyURL, _ := url.Parse(proxyServer.URL)
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}

	wsProxy := NewWebSocketProxy(logger, transport)

	// Test dialTarget directly
	targetURL, _ := url.Parse(targetServer.URL)
	targetAddr := wsProxy.resolveTargetAddr(targetURL)

	conn, err := wsProxy.dialTarget(targetURL, targetAddr)
	require.NoError(t, err, "dialTarget through proxy should succeed")
	require.NotNil(t, conn)
	conn.Close()
}

func TestWebSocketProxyFullFlowThroughHTTPProxyWithTLS(t *testing.T) {
	// Create a TLS WebSocket echo server (the actual target)
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	targetRouter := mux.NewRouter()
	targetRouter.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Target received request: %s %s", r.Method, r.URL)
		t.Logf("Target headers: Host=%s Upgrade=%s Connection=%s",
			r.Host, r.Header.Get("Upgrade"), r.Header.Get("Connection"))

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// Echo messages back
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				t.Logf("Target read error: %v", err)
				break
			}
			t.Logf("Target received message: %s", message)
			if err := conn.WriteMessage(messageType, message); err != nil {
				t.Logf("Target write error: %v", err)
				break
			}
		}
	})

	// Use NewTLSServer for HTTPS target
	targetServer := httptest.NewTLSServer(targetRouter)
	defer targetServer.Close()

	t.Logf("TLS Target server at: %s", targetServer.URL)

	// Create an HTTP CONNECT proxy (transparent tunnel - doesn't inspect TLS)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "Expected CONNECT", http.StatusMethodNotAllowed)
			return
		}

		t.Logf("Proxy received CONNECT to %s", r.Host)

		// Connect to the target (TLS server)
		targetConn, err := net.Dial("tcp", r.Host)
		if err != nil {
			t.Logf("Proxy failed to connect to target: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			targetConn.Close()
			http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			targetConn.Close()
			return
		}

		// Send 200 OK to establish the tunnel
		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		t.Log("Proxy sent 200 Connection Established")

		// Bidirectional copy - TLS traffic passes through transparently
		// Note: Don't use t.Logf in goroutines - test may complete before they finish
		done := make(chan struct{}, 2)
		go func() {
			io.Copy(targetConn, clientConn)
			targetConn.Close()
			done <- struct{}{}
		}()
		go func() {
			io.Copy(clientConn, targetConn)
			clientConn.Close()
			done <- struct{}{}
		}()
		<-done
		<-done
	}))
	defer proxyServer.Close()

	t.Logf("Proxy server at: %s", proxyServer.URL)

	// Create WebSocketProxy with the proxy configured via transport
	// Use InsecureSkipVerify since the target uses a self-signed test certificate
	logger := zaptest.NewLogger(t)
	proxyURL, _ := url.Parse(proxyServer.URL)
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	wsProxy := NewWebSocketProxy(logger, transport)

	// Test dialTarget - this establishes TCP -> CONNECT -> TLS
	targetURL, _ := url.Parse(targetServer.URL)
	targetAddr := wsProxy.resolveTargetAddr(targetURL)

	t.Logf("Dialing target URL: %s, addr: %s", targetServer.URL, targetAddr)

	conn, err := wsProxy.dialTarget(targetURL, targetAddr)
	require.NoError(t, err, "dialTarget through proxy to TLS target should succeed")
	require.NotNil(t, conn)

	// Verify it's a TLS connection
	tlsConn, ok := conn.(*tls.Conn)
	require.True(t, ok, "connection should be a TLS connection")

	state := tlsConn.ConnectionState()
	t.Logf("TLS state: version=%d, handshakeComplete=%v, serverName=%s",
		state.Version, state.HandshakeComplete, state.ServerName)
	require.True(t, state.HandshakeComplete, "TLS handshake should be complete")

	// Now send a WebSocket upgrade request through the TLS tunnel
	upgradeReq := "GET /ws HTTP/1.1\r\n" +
		"Host: " + targetURL.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"

	t.Log("Sending WebSocket upgrade request")
	_, err = conn.Write([]byte(upgradeReq))
	require.NoError(t, err, "write upgrade request should succeed")

	// Read the response
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	require.NoError(t, err, "read upgrade response should succeed")

	response := string(buf[:n])
	t.Logf("Received response:\n%s", response)
	require.Contains(t, response, "101 Switching Protocols", "should receive WebSocket upgrade response")
	require.Contains(t, response, "Upgrade: websocket", "response should have Upgrade header")

	conn.Close()
}

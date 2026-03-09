package snykbroker

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// WebSocketProxy handles proxying WebSocket connections through HTTP CONNECT proxies.
// It manages the TCP tunnel between client and target, respecting proxy settings
// and TLS configuration from the provided http.Transport.
type WebSocketProxy struct {
	logger    *zap.Logger
	transport *http.Transport

	// Timeouts
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration

	// Callbacks
	OnTunnelEstablished func(target string)
	OnTunnelClosed      func(target string, duration time.Duration)

	// State tracking - use Int32 to properly track multiple concurrent tunnels
	activeConnections atomic.Int32
}

// NewWebSocketProxy creates a WebSocket proxy that uses the given transport's
// proxy and TLS settings.
func NewWebSocketProxy(logger *zap.Logger, transport *http.Transport) *WebSocketProxy {
	return &WebSocketProxy{
		logger:           logger,
		transport:        transport,
		DialTimeout:      30 * time.Second,
		HandshakeTimeout: 30 * time.Second,
		IdleTimeout:      5 * time.Minute,
	}
}

// IsConnected returns true if any WebSocket tunnel is currently active.
func (wp *WebSocketProxy) IsConnected() bool {
	return wp.activeConnections.Load() > 0
}

// ActiveConnections returns the number of currently active WebSocket tunnels.
func (wp *WebSocketProxy) ActiveConnections() int32 {
	return wp.activeConnections.Load()
}

// Proxy handles a WebSocket upgrade request by establishing a tunnel to the target.
// It hijacks the client connection and proxies bidirectionally.
func (wp *WebSocketProxy) Proxy(w http.ResponseWriter, r *http.Request, targetURI string) error {
	targetURL, err := url.Parse(targetURI)
	if err != nil {
		return fmt.Errorf("invalid target URI: %w", err)
	}

	// Determine target address
	targetAddr := wp.resolveTargetAddr(targetURL)

	// Dial the target (through proxy if configured)
	targetConn, err := wp.dialTarget(targetURL, targetAddr)
	if err != nil {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return fmt.Errorf("dial failed: %w", err)
	}

	// Forward the upgrade request to target
	if err := wp.forwardUpgradeRequest(r, targetConn, targetURL); err != nil {
		targetConn.Close()
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return err
	}

	// Hijack client connection
	clientConn, err := wp.hijackConnection(w)
	if err != nil {
		targetConn.Close()
		return err
	}

	// Run the bidirectional tunnel
	wp.runTunnel(clientConn, targetConn, targetAddr)
	return nil
}

func (wp *WebSocketProxy) resolveTargetAddr(targetURL *url.URL) string {
	host := targetURL.Hostname()
	port := targetURL.Port()
	if port == "" {
		if targetURL.Scheme == "https" || targetURL.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

func (wp *WebSocketProxy) dialTarget(targetURL *url.URL, targetAddr string) (net.Conn, error) {
	proxyURL, err := wp.getProxyURL(targetURL)
	if err != nil {
		return nil, err
	}

	requiresTLS := targetURL.Scheme == "https" || targetURL.Scheme == "wss"
	tlsConfig := wp.getTLSConfig(targetURL.Hostname(), requiresTLS)

	if proxyURL != nil {
		wp.logger.Info("Connecting to WebSocket target through proxy",
			zap.String("proxy", proxyURL.Host),
			zap.String("target", targetAddr))
		return wp.dialThroughProxy(proxyURL, targetAddr, tlsConfig)
	}

	wp.logger.Info("Connecting to WebSocket target directly",
		zap.String("target", targetAddr),
		zap.Bool("tls", requiresTLS))
	return wp.dialDirect(targetAddr, tlsConfig)
}

func (wp *WebSocketProxy) getProxyURL(targetURL *url.URL) (*url.URL, error) {
	if wp.transport != nil && wp.transport.Proxy != nil {
		return wp.transport.Proxy(&http.Request{URL: targetURL})
	}
	return http.ProxyFromEnvironment(&http.Request{URL: targetURL})
}

func (wp *WebSocketProxy) getTLSConfig(serverName string, requiresTLS bool) *tls.Config {
	if !requiresTLS {
		return nil
	}

	if wp.transport != nil && wp.transport.TLSClientConfig != nil {
		cfg := wp.transport.TLSClientConfig.Clone()
		cfg.ServerName = serverName
		return cfg
	}

	return &tls.Config{ServerName: serverName}
}

func (wp *WebSocketProxy) dialDirect(addr string, tlsConfig *tls.Config) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: wp.DialTimeout}

	if tlsConfig != nil {
		return tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	}
	return dialer.Dial("tcp", addr)
}

func (wp *WebSocketProxy) dialThroughProxy(proxyURL *url.URL, targetAddr string, tlsConfig *tls.Config) (net.Conn, error) {
	// Connect to proxy
	proxyAddr := wp.resolveProxyAddr(proxyURL)
	dialer := &net.Dialer{Timeout: wp.DialTimeout}

	var proxyConn net.Conn
	var err error
	if proxyURL.Scheme == "https" {
		proxyTLS := wp.getTLSConfig(proxyURL.Hostname(), true)
		proxyConn, err = tls.DialWithDialer(dialer, "tcp", proxyAddr, proxyTLS)
	} else {
		proxyConn, err = dialer.Dial("tcp", proxyAddr)
	}
	if err != nil {
		return nil, fmt.Errorf("proxy connect failed: %w", err)
	}

	// Send CONNECT request
	if err := wp.sendConnectRequest(proxyConn, targetAddr, proxyURL); err != nil {
		proxyConn.Close()
		return nil, err
	}

	// Upgrade to TLS if needed
	if tlsConfig != nil {
		tlsConn := tls.Client(proxyConn, tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("TLS handshake failed: %w", err)
		}
		return tlsConn, nil
	}

	return proxyConn, nil
}

func (wp *WebSocketProxy) resolveProxyAddr(proxyURL *url.URL) string {
	host := proxyURL.Hostname()
	port := proxyURL.Port()
	if port == "" {
		if proxyURL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

func (wp *WebSocketProxy) sendConnectRequest(conn net.Conn, targetAddr string, proxyURL *url.URL) error {
	conn.SetDeadline(time.Now().Add(wp.HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	req := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}

	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		// Use Proxy-Authorization header for CONNECT requests to proxies
		req.Header.Set("Proxy-Authorization", basicAuth(proxyURL.User.Username(), password))
	}

	if err := req.Write(conn); err != nil {
		return fmt.Errorf("CONNECT request failed: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("CONNECT response failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy rejected CONNECT: %s", resp.Status)
	}

	return nil
}

func (wp *WebSocketProxy) forwardUpgradeRequest(r *http.Request, targetConn net.Conn, targetURL *url.URL) error {
	r.URL.Scheme = targetURL.Scheme
	r.URL.Host = targetURL.Host
	r.Host = targetURL.Host

	targetConn.SetWriteDeadline(time.Now().Add(wp.HandshakeTimeout))
	if err := r.Write(targetConn); err != nil {
		return fmt.Errorf("forward upgrade failed: %w", err)
	}
	targetConn.SetWriteDeadline(time.Time{})

	return nil
}

func (wp *WebSocketProxy) hijackConnection(w http.ResponseWriter) (net.Conn, error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("ResponseWriter does not support hijacking")
	}

	conn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "Hijack failed", http.StatusInternalServerError)
		return nil, fmt.Errorf("hijack failed: %w", err)
	}

	return conn, nil
}

func (wp *WebSocketProxy) runTunnel(clientConn, targetConn net.Conn, targetAddr string) {
	start := time.Now()
	wp.activeConnections.Add(1)

	if wp.OnTunnelEstablished != nil {
		wp.OnTunnelEstablished(targetAddr)
	}

	defer func() {
		wp.activeConnections.Add(-1)
		if wp.OnTunnelClosed != nil {
			wp.OnTunnelClosed(targetAddr, time.Since(start))
		}
	}()

	done := make(chan struct{}, 2)

	copy := func(dst, src net.Conn, direction string) {
		defer func() { done <- struct{}{} }()
		wp.copyWithIdleTimeout(dst, src, direction)
	}

	go copy(clientConn, targetConn, "target->client")
	go copy(targetConn, clientConn, "client->target")

	<-done
	clientConn.Close()
	targetConn.Close()
	<-done
}

func (wp *WebSocketProxy) copyWithIdleTimeout(dst, src net.Conn, direction string) {
	buf := make([]byte, 32*1024)
	for {
		src.SetReadDeadline(time.Now().Add(wp.IdleTimeout))
		n, err := src.Read(buf)
		if n > 0 {
			dst.SetWriteDeadline(time.Now().Add(wp.HandshakeTimeout))
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			if !isTimeoutError(err) && err != io.EOF {
				wp.logger.Debug("Tunnel read error", zap.String("direction", direction), zap.Error(err))
			}
			return
		}
	}
}

// IsWebSocketUpgrade checks if a request is a WebSocket upgrade request.
func IsWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// isTimeoutError checks if an error is a network timeout error.
func isTimeoutError(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout()
	}
	return false
}

// basicAuth returns the base64 encoded Basic auth string for Proxy-Authorization.
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
}

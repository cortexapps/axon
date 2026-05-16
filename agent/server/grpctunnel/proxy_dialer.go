package grpctunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"go.uber.org/zap"
)

// bufferedConn preserves any bytes that the bufio.Reader read past the CONNECT
// response headers, so the gRPC/TLS handshake that follows reads from the
// correct point in the stream. Plain net.Conn would lose those buffered bytes.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

// proxyURLFromEnv returns the configured HTTP proxy for the given target, or
// nil if none. Respects HTTP_PROXY/HTTPS_PROXY/NO_PROXY.
func proxyURLFromEnv(targetAddr string, grpcInsecure bool) *url.URL {
	scheme := "https"
	if grpcInsecure {
		scheme = "http"
	}
	fakeReq, _ := http.NewRequest("GET", fmt.Sprintf("%s://%s/", scheme, targetAddr), nil)
	proxyURL, err := http.ProxyFromEnvironment(fakeReq)
	if err != nil || proxyURL == nil {
		return nil
	}
	return proxyURL
}

// newProxyDialer returns a context dialer that tunnels through an HTTP CONNECT proxy.
func newProxyDialer(proxyURL *url.URL, logger *zap.Logger) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		proxyAddr := proxyURL.Host
		if proxyURL.Port() == "" {
			proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "8080")
		}

		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to proxy %s: %w", proxyAddr, err)
		}

		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if proxyURL.User != nil {
			username := proxyURL.User.Username()
			password, _ := proxyURL.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
			connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
		}
		connectReq += "\r\n"

		if _, err := conn.Write([]byte(connectReq)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to send CONNECT request: %w", err)
		}

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to read CONNECT response: %w", err)
		}
		// Drain and close any response body so we own the read pointer.
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT failed with status %d", resp.StatusCode)
		}

		logger.Debug("HTTP CONNECT tunnel established",
			zap.String("proxy", proxyURL.Host),
			zap.String("target", addr),
		)

		// Wrap so any bytes still buffered in br (past the CONNECT headers) are
		// not lost by the subsequent TLS/gRPC reader.
		return &bufferedConn{Conn: conn, r: br}, nil
	}
}

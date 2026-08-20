package util

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// bufferedConn preserves any bytes the bufio.Reader consumed past the
// CONNECT response headers, so whatever handshake follows (TLS, h2) reads
// from the correct point in the stream.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

// ProxyAddr returns proxyURL's host:port, defaulting the port from the
// scheme (443 for https, 80 otherwise).
func ProxyAddr(proxyURL *url.URL) string {
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

// DialProxyConnect dials an HTTP proxy and establishes a CONNECT tunnel to
// targetAddr, returning a conn positioned just past the proxy's response.
// Shared by the gRPC tunnel dialer and the relay reflector's WebSocket
// proxy so CONNECT mechanics (auth, response handling, buffered bytes)
// live in one place.
//
// proxyTLS, when non-nil, wraps the proxy connection itself in TLS
// (https:// proxies). handshakeTimeout bounds the CONNECT exchange; <= 0
// means no deadline.
func DialProxyConnect(ctx context.Context, proxyURL *url.URL, targetAddr string, proxyTLS *tls.Config, handshakeTimeout time.Duration) (net.Conn, error) {
	proxyAddr := ProxyAddr(proxyURL)

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to proxy %s: %w", proxyAddr, err)
	}

	if proxyTLS != nil {
		tlsConn := tls.Client(conn, proxyTLS)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy TLS handshake failed: %w", err)
		}
		conn = tlsConn
	}

	if handshakeTimeout > 0 {
		conn.SetDeadline(time.Now().Add(handshakeTimeout))
		defer conn.SetDeadline(time.Time{})
	}

	req := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		req.Header.Set("Proxy-Authorization", "Basic "+auth)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT request failed: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT response failed: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy rejected CONNECT: %s", resp.Status)
	}

	// Preserve any bytes the reader buffered past the CONNECT response.
	return &bufferedConn{Conn: conn, r: br}, nil
}

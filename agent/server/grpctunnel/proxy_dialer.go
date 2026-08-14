package grpctunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/cortexapps/axon/util"
	"go.uber.org/zap"
)

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

// newProxyDialer returns a context dialer that tunnels through an HTTP
// CONNECT proxy. The CONNECT mechanics are shared with the relay
// reflector's WebSocket proxy via util.DialProxyConnect.
func newProxyDialer(proxyURL *url.URL, logger *zap.Logger) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		var proxyTLS *tls.Config
		if proxyURL.Scheme == "https" {
			proxyTLS = &tls.Config{ServerName: proxyURL.Hostname()}
		}

		conn, err := util.DialProxyConnect(ctx, proxyURL, addr, proxyTLS, 0)
		if err != nil {
			return nil, err
		}

		logger.Debug("HTTP CONNECT tunnel established",
			zap.String("proxy", proxyURL.Host),
			zap.String("target", addr),
		)
		return conn, nil
	}
}

package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	gohttp "net/http"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/util"
	"go.uber.org/zap"
)

const (
	httpDialTimeout         = 30 * time.Second
	httpTLSHandshakeTimeout = 30 * time.Second
	// httpResponseHeaderTimeout matches the tunnel's MaxRequestTimeout and the
	// default handler timeout, so a request the agent would wait on anyway is
	// never cut shorter here, but nothing waits longer.
	httpResponseHeaderTimeout = 5 * time.Minute
	httpExpectContinueTimeout = 1 * time.Second
	// httpMaxIdleConnsPerHost only sizes the idle pool; it never blocks.
	// net/http's default of 2 would turn the handler path's concurrent calls
	// into fresh handshakes.
	httpMaxIdleConnsPerHost = 16
	httpIdleConnTimeout     = 90 * time.Second
)

func createHttpTransport(config config.AgentConfig, logger *zap.Logger) *gohttp.Transport {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: config.HttpDisableTLS,
	}

	util.EnsureLocalhostNoProxy(true)

	// Load custom CA cert if provided. Shared with the gRPC tunnel's
	// credentials so both read CA_CERT_PATH the same way.
	if config.HttpCaCertFilePath != "" {
		logger.Info("CA_CERT_PATH set, looking for cert files", zap.String("path", config.HttpCaCertFilePath))
	}
	caPEM, err := util.ReadCACertPEM(config.HttpCaCertFilePath)
	if err != nil {
		panic(err)
	}

	if len(caPEM) > 0 {

		if config.HttpDisableTLS {
			panic("Cannot use custom CA cert with TLS verification disabled")
		}

		roots := x509.NewCertPool()
		if ok := roots.AppendCertsFromPEM(caPEM); ok {
			tlsConfig.RootCAs = roots
			tlsConfig.InsecureSkipVerify = false
		}
	}

	// This transport is shared by the Cortex API proxy, the reflector and
	// relay registration. It is deliberately uncapped: MaxConnsPerHost queues
	// instead of failing, so one caller that leaks connections (registration
	// bodies left unclosed, in one incident) would starve every other user of
	// the same host with no error anywhere. The gRPC tunnel, whose upstream is
	// a customer service with unknown capacity, clones this transport and
	// applies UpstreamMaxConnsPerHost to its own copy; see
	// grpctunnel.NewTunnelClient.
	//
	// Every phase is bounded so a hung upstream becomes an error instead of a
	// connection held for the life of the process. Per-request contexts are
	// the primary bound; these are the backstop for any caller that forgets.
	return &gohttp.Transport{
		Proxy:                 gohttp.ProxyFromEnvironment,
		TLSClientConfig:       tlsConfig,
		DialContext:           (&net.Dialer{Timeout: httpDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   httpTLSHandshakeTimeout,
		ResponseHeaderTimeout: httpResponseHeaderTimeout,
		ExpectContinueTimeout: httpExpectContinueTimeout,
		MaxIdleConnsPerHost:   httpMaxIdleConnsPerHost,
		IdleConnTimeout:       httpIdleConnTimeout,
	}
}

func createHttpClient(config config.AgentConfig, transport *gohttp.Transport) *gohttp.Client {

	return &gohttp.Client{
		Transport: transport,
	}
}

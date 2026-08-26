package cmd

import (
	"crypto/tls"
	"crypto/x509"
	gohttp "net/http"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/util"
	"go.uber.org/zap"
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

	// Connection pooling matters more here than in a typical client: every
	// tunnelled call becomes an upstream request, and they concentrate on a
	// handful of hosts. net/http's default MaxIdleConnsPerHost of 2 would
	// mean all but two concurrent calls open a fresh TCP+TLS connection and
	// discard it on completion, turning throughput into handshakes.
	//
	// MaxConnsPerHost is the ceiling that actually protects something real:
	// the upstream is the customer's own service, whose capacity we neither
	// control nor can discover, so the agent must not fan out without limit.
	// It bounds concurrency rather than failing — requests over the line
	// wait for a connection.
	return &gohttp.Transport{
		Proxy:               gohttp.ProxyFromEnvironment,
		TLSClientConfig:     tlsConfig,
		MaxIdleConns:        config.UpstreamMaxConnsPerHost * 2,
		MaxIdleConnsPerHost: config.UpstreamMaxConnsPerHost,
		MaxConnsPerHost:     config.UpstreamMaxConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
	}
}

func createHttpClient(config config.AgentConfig, transport *gohttp.Transport) *gohttp.Client {

	return &gohttp.Client{
		Transport: transport,
	}
}

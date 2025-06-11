package snykbroker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"

	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/gorilla/mux"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type RegistrationReflector struct {
	logger        *zap.Logger
	transport     *http.Transport
	server        cortexHttp.Server
	proxyHandler  http.Handler
	targetURI     string
	serverStarted atomic.Bool
}

func NewRegistrationReflector(lifecycle fx.Lifecycle, logger *zap.Logger, transport *http.Transport) *RegistrationReflector {

	httpParams := cortexHttp.HttpServerParams{
		Logger: logger.Named("relay-reflector"),
	}

	server := cortexHttp.NewHttpServer(httpParams, cortexHttp.WithName("relay-reflector"))

	rr := &RegistrationReflector{
		transport: transport,
		server:    server,
		logger:    httpParams.Logger,
	}

	server.RegisterHandler(rr)

	if lifecycle != nil {
		lifecycle.Append(
			fx.Hook{
				OnStart: func(ctx context.Context) error {
					_, err := rr.Start()
					return err
				},
			},
		)
	}

	return rr
}

func (rr *RegistrationReflector) Start() (int, error) {

	if rr.serverStarted.CompareAndSwap(false, true) {
		_, err := rr.server.Start()
		if err != nil {
			return 0, err
		}
	}

	return rr.server.Port(), nil
}

func (rr *RegistrationReflector) Stop() error {
	if rr.server != nil {
		return rr.server.Close()
	}
	return nil
}

func (rr *RegistrationReflector) SetTargetURI(targetURI string) error {
	if targetURI == "" {
		return fmt.Errorf("target URI cannot be empty")
	}

	if rr.targetURI == targetURI {
		return nil
	}

	if rr.targetURI != "" {
		rr.proxyHandler = nil
	}

	rr.targetURI = targetURI

	asUri, err := url.Parse(targetURI)
	if err != nil {
		return fmt.Errorf("invalid target URI: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(asUri)

	// The proxy needs to override the Host and the URL host to not get erroneous 404s
	// https://stackoverflow.com/questions/23164547/golang-reverseproxy-not-working
	// https://github.com/golang/go/issues/14413
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		req.Host = asUri.Host
	}

	if rr.transport != nil {
		proxy.Transport = rr.transport
	}
	rr.proxyHandler = proxy
	rr.logger.Info("Reflecting broker calls",
		zap.String("targetURI", targetURI),
		zap.String("proxyURI", fmt.Sprintf("http://localhost:%d", rr.server.Port())))
	return nil
}

func (rr *RegistrationReflector) Target() string {
	return rr.targetURI
}

func (rr *RegistrationReflector) ProxyURI() string {
	_, err := rr.Start()
	if err != nil {
		panic(fmt.Sprintf("failed to start registration reflector: %v", err))
	}
	return fmt.Sprintf("http://localhost:%d", rr.server.Port())
}
func (rr *RegistrationReflector) RegisterRoutes(mux *mux.Router) error {
	mux.PathPrefix("/").Handler(rr)
	return nil
}

func (rr *RegistrationReflector) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	// Serve the request using the proxy handler
	rr.proxyHandler.ServeHTTP(w, r)
}

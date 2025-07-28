package server

import (
	"net/http"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/api"
	"github.com/cortexapps/axon/server/handler"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type MainHttpServerParams struct {
	fx.In
	Lifecycle      fx.Lifecycle
	Config         config.AgentConfig
	Logger         *zap.Logger
	Registry       *prometheus.Registry
	Transport      *http.Transport
	HandlerManager handler.Manager `optional:"true"`
}

func NewMainHttpServer(p MainHttpServerParams) cortexHttp.Server {

	httpServerParams := cortexHttp.HttpServerParams{
		Lifecycle: p.Lifecycle,
		Logger:    p.Logger,
		Registry:  p.Registry,
		Handlers:  []cortexHttp.RegisterableHandler{},
		Config:    p.Config,
	}

	config := p.Config

	httpServer := cortexHttp.NewHttpServer(httpServerParams, cortexHttp.WithPort(config.HttpServerPort))

	params := cortexHttp.AxonHandlerParams{
		Logger:         p.Logger,
		Config:         p.Config,
		HandlerManager: p.HandlerManager,
	}
	axonHandler := cortexHttp.NewAxonHandler(params)
	httpServer.RegisterHandler(axonHandler)

	if config.EnableApiProxy {
		proxy := api.NewApiProxyHandler(config, p.Logger, p.Transport)
		httpServer.RegisterHandler(proxy)
	}

	if p.Registry != nil {
		metricsHandler := cortexHttp.NewMetricsHandler(config, p.Logger, p.Registry)
		httpServer.RegisterHandler(metricsHandler)
	}

	return httpServer
}

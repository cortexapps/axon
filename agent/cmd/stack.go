package cmd

import (
	_ "embed"
	"fmt"
	gohttp "net/http"
	"os"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server"
	"github.com/cortexapps/axon/server/api"
	"github.com/cortexapps/axon/server/handler"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func startAgent(opts fx.Option) {
	app := fx.New(
		opts,
	)

	noBanner := os.Getenv("NO_BANNER")
	if noBanner == "" {
		fmt.Println(banner)
	}
	app.Run()
}

var AgentModule = fx.Module("agent",
	fx.Provide(cortexHttp.NewPrometheusRegistry),
	fx.Provide(createHttpTransport),
	fx.Provide(createHttpClient),
	fx.Provide(cortexHttp.NewAxonHandler),
	fx.Provide(createMainHttpServer),
	fx.Provide(func(config config.AgentConfig) *zap.Logger {

		if config.VerboseOutput {
			return zap.NewNop()
		}

		cfg := zap.NewDevelopmentConfig()

		loggingLevel := zap.InfoLevel
		if config.VerboseOutput {
			loggingLevel = zap.DebugLevel
		}

		cfg.Level = zap.NewAtomicLevelAt(loggingLevel)
		logger, err := cfg.Build()
		if err != nil {
			panic(err)
		}
		return logger
	}),
	fx.Invoke(func(config config.AgentConfig, logger *zap.Logger) {
		if config.CortexApiToken == "" && !config.DryRun {
			logger.Fatal("Cannot start agent: either CORTEX_API_TOKEN or DRYRUN is required")
		}
	}),
	fx.Invoke(server.NewAxonAgent),
)

func initStack(cmd *cobra.Command, cfg config.AgentConfig, integrationInfo common.IntegrationInfo) fx.Option {
	// This is a placeholder for the actual stack building logic
	// It should be replaced with the actual implementation
	return fx.Options(
		fx.Supply(integrationInfo),
		fx.Supply(cmd),
		fx.Supply(cfg),
	)
}

type MainHttpServerParams struct {
	fx.In
	Lifecycle      fx.Lifecycle
	Config         config.AgentConfig
	Logger         *zap.Logger
	Registry       *prometheus.Registry
	Transport      *gohttp.Transport
	HandlerManager handler.Manager `optional:"true"`
}

func createMainHttpServer(p MainHttpServerParams) cortexHttp.Server {

	httpServerParams := cortexHttp.HttpServerParams{
		Lifecycle: p.Lifecycle,
		Logger:    p.Logger,
		Registry:  p.Registry,
		Handlers:  []cortexHttp.RegisterableHandler{},
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

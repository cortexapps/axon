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
	"github.com/cortexapps/axon/server/http"
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
	fx.Provide(http.NewPrometheusRegistry),
	fx.Provide(createHttpTransport),
	fx.Provide(createHttpClient),
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

func createMainHttpServer(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger, registry *prometheus.Registry, transport *gohttp.Transport) cortexHttp.Server {

	httpServerParams := cortexHttp.HttpServerParams{
		Logger:   logger,
		Registry: registry,
		Handlers: []cortexHttp.RegisterableHandler{},
	}

	httpServer := cortexHttp.NewHttpServer(httpServerParams, cortexHttp.WithPort(config.HttpServerPort))

	if config.EnableApiProxy {
		proxy := api.NewApiProxyHandler(config, logger, transport)
		httpServer.RegisterHandler(proxy)
	}

	if registry != nil {
		metricsHandler := cortexHttp.NewMetricsHandler(config, logger, registry)
		httpServer.RegisterHandler(metricsHandler)
	}

	return httpServer
}

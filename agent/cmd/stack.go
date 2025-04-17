package cmd

import (
	"context"
	_ "embed"
	"fmt"
	"os"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server"
	"github.com/cortexapps/axon/server/api"
	"github.com/cortexapps/axon/server/handler"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker"
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

func buildCoreAgentStack(cmd *cobra.Command, cfg config.AgentConfig) fx.Option {

	if ok, _ := cmd.Flags().GetBool("verbose"); ok {
		cfg.VerboseOutput = true
	}

	options := []fx.Option{}

	if !cfg.VerboseOutput {
		options = append(options, fx.NopLogger)
	}

	stackOptions := fx.Options(
		fx.Supply(cfg),
		fx.Provide(func(config config.AgentConfig) *zap.Logger {
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
		fx.Provide(snykbroker.NewRegistration),
		fx.Provide(handler.NewHandlerManager),
		fx.Invoke(createAxonAgent),
	)

	options = append(options, stackOptions)

	return fx.Options(
		options...,
	)

}

func createAxonAgent(
	lifecycle fx.Lifecycle,
	logger *zap.Logger,
	cfg config.AgentConfig,
	manager handler.Manager,
	_ cortexHttp.Server, // we need to make sure the HTTP server is always started
) *server.AxonAgent {
	agent := server.NewAxonAgent(logger, cfg, manager)
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			return agent.Start(ctx)
		},
	})
	return agent
}

func createHttpServer(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger, handlerManager handler.Manager) cortexHttp.Server {
	httpServer := cortexHttp.NewHttpServer(logger)

	if config.EnableApiProxy {
		proxy := api.NewApiProxyHandler(config, logger)
		httpServer.RegisterHandler(proxy)
	}

	axonHandler := cortexHttp.NewAxonHandler(config, logger, handlerManager)
	httpServer.RegisterHandler(axonHandler)

	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			_, err := httpServer.Start(config.HttpServerPort)
			logger.Info("HTTP server started", zap.Int("port", config.HttpServerPort), zap.Error(err))
			return err
		},
		OnStop: func(ctx context.Context) error {
			httpServer.Close()
			return nil
		},
	})
	return httpServer
}

func createWebhookHttpServer(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger, handlerManager handler.Manager) cortexHttp.Server {

	httpServer := cortexHttp.NewHttpServer(logger)

	handler := cortexHttp.NewWebhookHandler(config, logger, handlerManager)
	httpServer.RegisterHandler(handler)

	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			_, err := httpServer.Start(config.WebhookServerPort)
			logger.Info("Webhook server started", zap.Int("port", config.WebhookServerPort), zap.Error(err))

			return err
		},
		OnStop: func(ctx context.Context) error {
			httpServer.Close()
			return nil
		},
	})
	return httpServer
}

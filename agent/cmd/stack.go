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
	"go.uber.org/zap/zapcore"
)

func getLoggingLevel(cmd *cobra.Command) zapcore.Level {

	if ok, _ := cmd.Flags().GetBool("verbose"); ok {
		return zap.DebugLevel
	}
	return zap.InfoLevel
}

func startAgent(cmd *cobra.Command, opts fx.Option) {
	options := []fx.Option{
		opts,
	}

	if getLoggingLevel(cmd) != zap.DebugLevel {
		options = append(options, fx.NopLogger)
	}

	app := fx.New(
		options...,
	)

	noBanner := os.Getenv("NO_BANNER")
	if noBanner == "" {
		fmt.Println(banner)
	}
	app.Run()
}

func buildCoreAgentStack(cmd *cobra.Command, cfg config.AgentConfig) fx.Option {
	return fx.Options(
		fx.Provide(func() *zap.Logger {
			cfg := zap.NewDevelopmentConfig()
			cfg.Level = zap.NewAtomicLevelAt(getLoggingLevel(cmd))
			logger, err := cfg.Build()
			if err != nil {
				panic(err)
			}
			return logger
		}),
		fx.Supply(cfg),
		fx.Invoke(func(config config.AgentConfig, logger *zap.Logger) {
			if config.CortexApiToken == "" && !config.DryRun {
				logger.Fatal("Cannot start agent: either CORTEX_API_TOKEN or DRYRUN is required")
			}
		}),
		fx.Provide(snykbroker.NewRegistration),
		fx.Provide(handler.NewHandlerManager),
		fx.Provide(server.NewAxonAgent),
		fx.Invoke(func(lifecycle fx.Lifecycle, logger *zap.Logger, cfg config.AgentConfig, server *server.AxonAgent, manager handler.Manager) *server.AxonAgent {
			lifecycle.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					return server.Start(ctx)
				},
			},
			)
			return server
		},
		),
	)
}

func createHttpServer(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger) cortexHttp.Server {
	httpServer := cortexHttp.NewHttpServer(logger)

	if config.EnableApiProxy {
		proxy := api.NewApiProxyHandler(config, logger)
		httpServer.RegisterHandler(proxy)
	}

	axonHandler := cortexHttp.NewAxonHandler(config, logger)
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

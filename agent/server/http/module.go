package http

import (
	"context"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Module("handler",
	fx.Provide(handler.NewHandlerManager),
	fx.Provide(cron.New),
	fx.Invoke(createWebhookHttpServer),
)

func createWebhookHttpServer(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger, handlerManager handler.Manager, registry *prometheus.Registry) Server {

	params := HttpServerParams{
		Logger:   logger,
		Registry: registry,
		Handlers: []RegisterableHandler{
			NewWebhookHandler(config, logger, handlerManager, registry),
		},
		Config: config,
	}
	httpServer := NewHttpServer(params, WithName("webhook"), WithPort(config.WebhookServerPort))

	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			_, err := httpServer.Start()
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

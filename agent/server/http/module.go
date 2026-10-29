package http

import (
	"context"
	"time"

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

// webhookReadTimeout bounds reading a webhook delivery. Senders are outside
// the agent's network and may upload large payloads slowly.
const webhookReadTimeout = 30 * time.Second

func createWebhookHttpServer(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger, handlerManager handler.Manager, registry *prometheus.Registry) Server {

	params := HttpServerParams{
		Logger:   logger,
		Registry: registry,
		Handlers: []RegisterableHandler{
			NewWebhookHandler(config, logger, handlerManager, registry),
		},
		Config: config,
	}
	httpServer := NewHttpServer(params, WithName("webhook"), WithPort(config.WebhookServerPort), WithReadTimeout(webhookReadTimeout))

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

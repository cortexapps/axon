package cmd

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/logstore"
	"github.com/spf13/cobra"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func startAgent(opts fx.Option) {
	app := fx.New(
		opts,
		fx.WithLogger(func(logger *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: logger}
		}),
	)

	noBanner := os.Getenv("NO_BANNER")
	if noBanner == "" {
		fmt.Println(banner)
	}
	app.Run()
}

var AgentModule = fx.Module("agent",
	// The captured-log store is built before the logger because the logger
	// tees into it. It registers its OnStop first, so it closes last and
	// everything else can still log while shutting down.
	fx.Provide(func(config config.AgentConfig, lc fx.Lifecycle) *logstore.Store {
		store := logstore.NewFromConfig(config)
		lc.Append(fx.Hook{OnStop: func(context.Context) error { return store.Close() }})
		return store
	}),
	fx.Provide(func(config config.AgentConfig, store *logstore.Store) *zap.Logger {

		cfg := zap.NewDevelopmentConfig()
		if os.Getenv("ENV") == "production" {
			cfg = zap.NewProductionConfig()
			cfg.EncoderConfig.TimeKey = "time"
			cfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
				enc.AppendString(t.UTC().Format("2006-01-02T15:04:05.000Z"))
			}
			cfg.EncoderConfig.NameKey = "name"
		}

		loggingLevel := zap.InfoLevel
		if config.VerboseOutput {
			loggingLevel = zap.DebugLevel
		}

		// cfg.Level governs stderr only. The store carries its own level,
		// so REMOTE_LOGS_VISIBLE=* puts debug on disk while stderr stays as
		// it was. A disabled store's option is a no-op.
		cfg.Level = zap.NewAtomicLevelAt(loggingLevel)
		logger, err := cfg.Build(store.ZapOption())
		if err != nil {
			panic(err)
		}
		logger = logger.Named("axon")
		if err := store.Err(); err != nil {
			logger.Warn("remote logs disabled", zap.Error(err))
		}
		return logger
	}),
	fx.Provide(cortexHttp.NewPrometheusRegistry),
	fx.Provide(createHttpTransport),
	fx.Provide(createHttpClient),
	fx.Provide(cortexHttp.NewAxonHandler),
	fx.Provide(server.NewMainHttpServer),

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

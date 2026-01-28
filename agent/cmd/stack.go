package cmd

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server"
	cortexHttp "github.com/cortexapps/axon/server/http"
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
	fx.Provide(func(config config.AgentConfig) *zap.Logger {

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

		cfg.Level = zap.NewAtomicLevelAt(loggingLevel)
		logger, err := cfg.Build()
		if err != nil {
			panic(err)
		}
		return logger.Named("axon")
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

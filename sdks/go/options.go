package axon

import (
	"time"

	"github.com/cortexapps/axon-go/version"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Option func(*agentOptions)

type agentOptions struct {
	host         string
	port         int
	loglevel     zapcore.Level
	loggerConfig zap.Config
	sleepOnError time.Duration
	version      string
}

func defaultAgentOptions() *agentOptions {
	return &agentOptions{
		host:         "localhost",
		port:         50051,
		loglevel:     zapcore.InfoLevel,
		loggerConfig: zap.NewDevelopmentConfig(),
		sleepOnError: time.Second * 5,
		version:      version.Client,
	}
}

func WithHostport(host string, port int) Option {
	return func(a *agentOptions) {
		a.host = host
		a.port = port
	}
}

func WithLoggerConfig(config zap.Config) Option {
	return func(a *agentOptions) {
		a.loggerConfig = config
	}
}

func WithLogLevel(level zapcore.Level) Option {
	return func(a *agentOptions) {
		a.loggerConfig.Level = zap.NewAtomicLevelAt(level)
	}
}

func WithSleepOnError(duration time.Duration) Option {
	return func(a *agentOptions) {
		a.sleepOnError = duration
	}
}

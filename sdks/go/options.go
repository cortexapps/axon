package axon

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/cortexapps/axon-go/version"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	defaultMaxReceiveMessageSize   = 4 * 1024 * 1024
	maxReceiveMessageSizeEnvVar    = "AXON_GRPC_MAX_RECEIVE_MESSAGE_SIZE"
	unlimitedMaxReceiveMessageSize = math.MaxInt32
)

type Option func(*agentOptions)

type agentOptions struct {
	host                  string
	port                  int
	loglevel              zapcore.Level
	loggerConfig          zap.Config
	sleepOnError          time.Duration
	version               string
	maxReceiveMessageSize int
}

func resolveMaxReceiveMessageSize() int {
	value := os.Getenv(maxReceiveMessageSizeEnvVar)
	if value == "" {
		return defaultMaxReceiveMessageSize
	}

	size, err := strconv.Atoi(value)
	if err != nil {
		panic(fmt.Sprintf("%s must be a whole number of bytes, or -1 for no limit, but is %q", maxReceiveMessageSizeEnvVar, value))
	}
	if size < 0 {
		return unlimitedMaxReceiveMessageSize
	}
	return size
}

func defaultAgentOptions() *agentOptions {
	return &agentOptions{
		host:         "localhost",
		port:         50051,
		loglevel:     zapcore.InfoLevel,
		loggerConfig: zap.NewDevelopmentConfig(),
		sleepOnError: time.Second * 5,
		version:      version.Client,

		maxReceiveMessageSize: resolveMaxReceiveMessageSize(),
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

// WithMaxReceiveMessageSize sets how large a gRPC message the client accepts
// from the agent, in bytes. It takes precedence over the
// AXON_GRPC_MAX_RECEIVE_MESSAGE_SIZE environment variable. Use -1 for no
// limit.
func WithMaxReceiveMessageSize(bytes int) Option {
	return func(a *agentOptions) {
		if bytes < 0 {
			bytes = unlimitedMaxReceiveMessageSize
		}
		a.maxReceiveMessageSize = bytes
	}
}

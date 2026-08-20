package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestNewLoggerConfig(t *testing.T) {
	t.Run("production logs structured JSON at info", func(t *testing.T) {
		cfg, err := newLoggerConfig("production", "")
		require.NoError(t, err)
		require.Equal(t, "json", cfg.Encoding)
		require.Equal(t, zapcore.InfoLevel, cfg.Level.Level())
		require.Equal(t, "time", cfg.EncoderConfig.TimeKey)
		require.Equal(t, "name", cfg.EncoderConfig.NameKey)
	})

	// The prod incident this guards: ENV unset in the deployment meant console
	// output at debug level from a production pod.
	t.Run("unset ENV is dev, not production", func(t *testing.T) {
		cfg, err := newLoggerConfig("", "")
		require.NoError(t, err)
		require.Equal(t, "console", cfg.Encoding)
		require.Equal(t, zapcore.DebugLevel, cfg.Level.Level())
	})

	t.Run("any other ENV value is dev", func(t *testing.T) {
		cfg, err := newLoggerConfig("development", "")
		require.NoError(t, err)
		require.Equal(t, "console", cfg.Encoding)
	})

	t.Run("LOG_LEVEL overrides the default level but not the encoding", func(t *testing.T) {
		cfg, err := newLoggerConfig("production", "debug")
		require.NoError(t, err)
		require.Equal(t, "json", cfg.Encoding)
		require.Equal(t, zapcore.DebugLevel, cfg.Level.Level())

		cfg, err = newLoggerConfig("", "warn")
		require.NoError(t, err)
		require.Equal(t, "console", cfg.Encoding)
		require.Equal(t, zapcore.WarnLevel, cfg.Level.Level())
	})

	t.Run("invalid LOG_LEVEL is an error, not a silent default", func(t *testing.T) {
		_, err := newLoggerConfig("production", "chatty")
		require.ErrorContains(t, err, "invalid LOG_LEVEL")
	})
}

func TestNewLoggerBuilds(t *testing.T) {
	t.Setenv("ENV", "production")
	logger, err := newLogger()
	require.NoError(t, err)
	require.NotNil(t, logger)
}

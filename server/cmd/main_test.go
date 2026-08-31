package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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

func TestHealthHandlerReportsBuildVersion(t *testing.T) {
	// /healthcheck is what the dispatcher probes and what an operator curls,
	// so it is where the running build has to be visible. Asserting on the
	// parsed body rather than the format string keeps this honest about the
	// JSON actually being valid -- the handler hand-rolls it with Fprintf.
	t.Setenv("AXON_BUILD_VERSION", "v0.2.12-abc1234")
	cfg := config.NewConfigFromEnv()
	logger := zap.NewNop()

	registry := tunnel.NewClientRegistry(logger)
	m := metrics.New(cfg.ServerID)
	defer m.Closer()
	dispatcher := dispatch.NewDispatcher(cfg, registry, m, logger)
	brokerClient := broker.NewClient("", cfg.ServerID, logger)

	rec := httptest.NewRecorder()
	newHealthHandler(cfg, registry, dispatcher, brokerClient)(
		rec, httptest.NewRequest(http.MethodGet, "/healthcheck", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "health body is not valid JSON: %s", rec.Body.String())
	require.Equal(t, "ok", body["status"])
	require.Equal(t, "v0.2.12-abc1234", body["build_version"])
	require.Equal(t, cfg.ServerID, body["server_id"])
}

func TestHealthHandlerReportsDevWhenUnstamped(t *testing.T) {
	// A locally built server must say so rather than emitting an empty field.
	t.Setenv("AXON_BUILD_VERSION", "")
	cfg := config.NewConfigFromEnv()
	logger := zap.NewNop()

	registry := tunnel.NewClientRegistry(logger)
	m := metrics.New(cfg.ServerID)
	defer m.Closer()
	dispatcher := dispatch.NewDispatcher(cfg, registry, m, logger)

	rec := httptest.NewRecorder()
	newHealthHandler(cfg, registry, dispatcher, broker.NewClient("", cfg.ServerID, logger))(
		rec, httptest.NewRequest(http.MethodGet, "/healthcheck", nil))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "dev", body["build_version"])
}

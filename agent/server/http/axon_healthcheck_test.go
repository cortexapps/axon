package http

import (
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func probeHealth(t *testing.T, h *axonHandler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.healthcheck(rec, httptest.NewRequest(nethttp.MethodGet, "/healthcheck", nil))
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "health body is not valid JSON: %s", rec.Body.String())
	return rec.Code, body
}

// The probe is the only thing that can restart a wedged SDK, so it must say
// 503 once the handler loop has stopped draining, and say why.
func TestHealthcheck_503WhenSDKStalled(t *testing.T) {
	mgr := handler.NewHandlerManager(zap.NewNop(), cron.New(), nil,
		handler.WithLivenessPolicy(handler.LivenessPolicy{TimeoutThreshold: 1}))
	mgr.Liveness().RecordStreamConnected()
	mgr.Liveness().RecordTimeout()
	h := NewAxonHandler(AxonHandlerParams{Logger: zap.NewNop(), Config: config.AgentConfig{}, HandlerManager: mgr}).(*axonHandler)

	code, body := probeHealth(t, h)
	require.Equal(t, nethttp.StatusServiceUnavailable, code)
	require.Equal(t, false, body["OK"])
	require.NotEmpty(t, body["reason"])
	require.NotEmpty(t, body["build_version"])
	liveness, ok := body["liveness"].(map[string]any)
	require.True(t, ok, "liveness snapshot missing: %v", body)
	require.EqualValues(t, 1, liveness["consecutive_timeouts"])

	mgr.Liveness().RecordSuccess()
	code, body = probeHealth(t, h)
	require.Equal(t, nethttp.StatusOK, code)
	require.Equal(t, true, body["OK"])
	_, hasReason := body["reason"]
	require.False(t, hasReason)
}

func TestHealthcheck_DisabledStaysHealthy(t *testing.T) {
	mgr := handler.NewHandlerManager(zap.NewNop(), cron.New(), nil,
		handler.WithLivenessPolicy(handler.LivenessPolicy{TimeoutThreshold: 0}))
	mgr.Liveness().RecordStreamConnected()
	mgr.Liveness().RecordTimeout()
	h := NewAxonHandler(AxonHandlerParams{Logger: zap.NewNop(), Config: config.AgentConfig{}, HandlerManager: mgr}).(*axonHandler)

	code, body := probeHealth(t, h)
	require.Equal(t, nethttp.StatusOK, code)
	require.Equal(t, true, body["OK"])
}

func TestHealthcheck_NoManagerStaysHealthy(t *testing.T) {
	h := NewAxonHandler(AxonHandlerParams{Logger: zap.NewNop(), Config: config.AgentConfig{}}).(*axonHandler)

	code, body := probeHealth(t, h)
	require.Equal(t, nethttp.StatusOK, code)
	require.Equal(t, true, body["OK"])
	_, hasReason := body["reason"]
	require.False(t, hasReason)
}

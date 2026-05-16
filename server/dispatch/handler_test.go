package dispatch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func newTestHandler(t *testing.T) (*Handler, *tunnel.ClientRegistry) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{DispatchTimeout: 5 * time.Second}
	h := NewHandler(cfg, registry, metrics.New("test-server"), logger)
	return h, registry
}

func registerTestStream(t *testing.T, registry *tunnel.ClientRegistry, rawToken string) broker.Token {
	t.Helper()
	token := broker.NewToken(rawToken)
	identity := tunnel.ClientIdentity{
		TenantID:    "tenant-1",
		Integration: "github",
		Alias:       "my-github",
		InstanceID:  "instance-1",
	}
	stream := &tunnel.StreamHandle{
		StreamID: "stream-1",
		Send:     func(msg *pb.TunnelServerMessage) error { return nil },
		Cancel:   func() {},
	}
	require.NoError(t, registry.Register(token, identity, stream))
	return token
}

func TestConnectionStatus_ConnectedRawToken(t *testing.T) {
	h, registry := newTestHandler(t)
	token := registerTestStream(t, registry, "token-abc")

	req := httptest.NewRequest(http.MethodGet, "/broker/connection-status/"+token.Raw(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Header().Get("x-broker-failure"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["ok"])
}

func TestConnectionStatus_ConnectedHashedToken(t *testing.T) {
	h, registry := newTestHandler(t)
	token := registerTestStream(t, registry, "token-abc")

	req := httptest.NewRequest(http.MethodGet, "/broker/connection-status/"+token.Hashed(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["ok"])
}

func TestConnectionStatus_NotConnected(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/broker/connection-status/no-such-token", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-connection", rec.Header().Get("x-broker-failure"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["ok"])
}

func TestConnectionStatus_AfterUnregister(t *testing.T) {
	h, registry := newTestHandler(t)
	token := registerTestStream(t, registry, "token-abc")

	// Confirm connected first.
	req := httptest.NewRequest(http.MethodGet, "/broker/connection-status/"+token.Hashed(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Tear it down and re-check.
	registry.Unregister(token, "stream-1")

	req = httptest.NewRequest(http.MethodGet, "/broker/connection-status/"+token.Hashed(), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "no-connection", rec.Header().Get("x-broker-failure"))
}

// TestConnectionStatus_DoesNotInterfereWithDispatch verifies the mux correctly
// routes /broker/connection-status/... separately from /broker/{token}/...
// dispatch paths — i.e., the dispatch handler isn't accidentally matching the
// status endpoint.
func TestConnectionStatus_DoesNotInterfereWithDispatch(t *testing.T) {
	h, _ := newTestHandler(t)

	// A dispatch path under an unknown token should hit handleBrokerDispatch
	// (which returns 502 "no tunnel available"), NOT getConnectionStatus.
	req := httptest.NewRequest(http.MethodGet, "/broker/some-token/some/path", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.NotEqual(t, "application/json", rec.Header().Get("Content-Type"))
}

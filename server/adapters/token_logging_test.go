package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A raw token this server holds no tunnel for used to fall back to being read
// as a hash, so the raw token was logged as "tokenHash" on every no_tunnel
// line. It must log as its real hash, with the raw token only masked.
func TestDispatch_UnknownRawTokenLogsHashAndMask(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{}
	m := metrics.New("test")
	h := NewHttpAdapter(cfg, registry, dispatch.NewDispatcher(cfg, registry, m, logger), m, logger)

	const raw = "66fa3dbe-46fa-4949-9713-89a2ead1e7c2"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/broker/"+raw+"/rest/api/1.0/projects", nil).WithContext(context.Background()))
	require.Equal(t, http.StatusNotFound, w.Code)

	entries := logs.FilterMessage("Dispatch failed").All()
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, zapcore.DebugLevel, e.Level, "no_tunnel is routine routing, not a failure")
	f := e.ContextMap()
	assert.Equal(t, broker.NewToken(raw).Hashed(), f["tokenHash"])
	assert.Equal(t, "66fa3dbe-****-****-****-89a2ead1e7c2", f["tokenMasked"])
	for _, entry := range logs.All() {
		for k, v := range entry.ContextMap() {
			if s, ok := v.(string); ok {
				assert.NotContains(t, s, raw, "raw token logged in %q of %q", k, entry.Message)
			}
		}
	}
}

// A caller that sends the hash gets it back as the hash, and nothing to mask.
func TestDispatch_HashedTokenLogsAsIs(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{}
	m := metrics.New("test")
	h := NewHttpAdapter(cfg, registry, dispatch.NewDispatcher(cfg, registry, m, logger), m, logger)

	hash := broker.NewToken("66fa3dbe-46fa-4949-9713-89a2ead1e7c2").Hashed()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/broker/"+hash+"/x", nil))

	entries := logs.FilterMessage("Dispatch failed").All()
	require.Len(t, entries, 1)
	f := entries[0].ContextMap()
	assert.Equal(t, hash, f["tokenHash"])
	assert.NotContains(t, f, "tokenMasked")
}

// Connection status must still find a token registered by its raw form when
// asked by hash, and by raw form when asked raw.
func TestConnectionStatus_ResolvesRawAndHash(t *testing.T) {
	registry := tunnel.NewClientRegistry(zap.NewNop())
	h := NewHttpAdapter(config.Config{}, registry, nil, metrics.New("test"), zap.NewNop())
	const raw = "66fa3dbe-46fa-4949-9713-89a2ead1e7c2"
	mustRegister(t, registry, broker.NewToken(raw), tunnel.ClientIdentity{InstanceID: "i1"},
		&tunnel.StreamHandle{StreamID: "s1"})

	for _, v := range []string{raw, broker.NewToken(raw).Hashed()} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connection-status/"+v, nil))
		assert.Equal(t, http.StatusOK, w.Code, v)
	}
}

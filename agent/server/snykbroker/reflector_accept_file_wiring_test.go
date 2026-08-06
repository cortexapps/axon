package snykbroker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// renderEnv is the reflector render step in isolation: a manager with only the
// fields that step reads, so the wiring is testable without the broker
// supervisor or a live registration.
type renderEnv struct {
	mgr       *relayInstanceManager
	reflector *RegistrationReflector
}

func newRenderEnv(t *testing.T, mode config.RelayReflectorMode) *renderEnv {
	logger := zaptest.NewLogger(t)
	cfg := config.AgentConfig{
		HttpRelayReflectorMode:    mode,
		ReflectorWebSocketUpgrade: true,
	}
	rr := NewRegistrationReflector(RegistrationReflectorParams{
		Logger:   logger,
		Registry: prometheus.NewRegistry(),
		Config:   cfg,
	})
	t.Cleanup(func() { rr.Stop() })
	return &renderEnv{
		mgr:       &relayInstanceManager{config: cfg, logger: logger, reflector: rr},
		reflector: rr,
	}
}

func (e *renderEnv) render(t *testing.T, content string) {
	t.Helper()
	af, err := acceptfile.NewAcceptFile([]byte(content), e.mgr.config, e.mgr.logger)
	require.NoError(t, err)
	_, err = af.Render(zap.NewNop(), e.mgr.reflectorRenderStep)
	require.NoError(t, err)
}

// The end-to-end proof that an accept file can switch retargeting on: the
// declared rule retargets, and a rule alongside it that declares nothing does
// not, from the same render pass.
func TestRenderOptsRuleIntoDynamicTargets(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	optedIn := newRecordingBackend(t, "opted-in")
	plain := newRecordingBackend(t, "plain")
	retarget := newRecordingBackend(t, "retargeted")

	env.render(t, fmt.Sprintf(`{"private": [
		{"method": "any", "origin": "%s", "path": "/*", "dynamicTargetHosts": ["127.0.0.1"]},
		{"method": "any", "origin": "%s", "path": "/*"}
	]}`, optedIn.server.URL, plain.server.URL))

	optedInURI, err := env.reflector.getUriForTarget(optedIn.server.URL)
	require.NoError(t, err)
	plainURI, err := env.reflector.getUriForTarget(plain.server.URL)
	require.NoError(t, err)

	// the opted-in rule honors the header
	req := httptest.NewRequest("GET", proxyPath(t, optedInURI)+"/v1/things", nil)
	req.Header.Set(HeaderRelayTargetHost, retarget.hostPort(t))
	rec := httptest.NewRecorder()
	env.reflector.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "retargeted", rec.Body.String())

	// the rule that declared nothing ignores it and passes it through
	req = httptest.NewRequest("GET", proxyPath(t, plainURI)+"/v1/things", nil)
	req.Header.Set(HeaderRelayTargetHost, retarget.hostPort(t))
	rec = httptest.NewRecorder()
	env.reflector.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "plain", rec.Body.String())

	hits, _, header := plain.snapshot()
	require.Equal(t, 1, hits)
	require.Equal(t, retarget.hostPort(t), header.Get(HeaderRelayTargetHost))
}

// The allowlist must come from the accept file, not from anywhere permissive
// by default.
func TestRenderDynamicTargetsEnforcesDeclaredAllowlist(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	backend := newRecordingBackend(t, "a")
	other := newRecordingBackend(t, "b")

	env.render(t, fmt.Sprintf(`{"private": [
		{"method": "any", "origin": "%s", "path": "/*", "dynamicTargetHosts": ["*.googleapis.com"]}
	]}`, backend.server.URL))

	uri, err := env.reflector.getUriForTarget(backend.server.URL)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", proxyPath(t, uri)+"/v1/things", nil)
	req.Header.Set(HeaderRelayTargetHost, other.hostPort(t))
	rec := httptest.NewRecorder()
	env.reflector.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	hits, _, _ := other.snapshot()
	require.Equal(t, 0, hits)
}

// Retargeting only exists on the reflector path, so declaring it with the
// reflector off has to fail loudly rather than be quietly dropped.
func TestRenderRejectsDynamicTargetsWithReflectorDisabled(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorDisabled)
	af, err := acceptfile.NewAcceptFile([]byte(`{"private": [
		{"method": "any", "origin": "https://a.googleapis.com", "path": "/*",
		 "dynamicTargetHosts": ["*.googleapis.com"]}
	]}`), env.mgr.config, env.mgr.logger)
	require.NoError(t, err)

	require.PanicsWithValue(t,
		"ENABLE_RELAY_REFLECTOR must be set to 'all' or 'traffic' to use dynamicTargetHosts in accept files",
		func() { _, _ = af.Render(zap.NewNop(), env.mgr.reflectorRenderStep) })
}

// A rule without the key must be untouched by any of this.
func TestRenderLeavesPlainRuleWithoutOptIn(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	backend := newRecordingBackend(t, "a")

	env.render(t, fmt.Sprintf(`{"private": [
		{"method": "any", "origin": "%s", "path": "/*"}
	]}`, backend.server.URL))

	uri, err := env.reflector.getUriForTarget(backend.server.URL)
	require.NoError(t, err)
	entry, _, err := env.reflector.parseTargetUri(proxyPath(t, uri))
	require.NoError(t, err)
	require.False(t, entry.allowsDynamicTargets())
}

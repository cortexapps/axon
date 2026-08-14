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
)

// renderEnv is the reflector render step in isolation: a manager with only the
// fields that step reads, so the wiring is testable without the broker
// supervisor or a live registration.
type renderEnv struct {
	mgr       *relayInstanceManager
	reflector *RegistrationReflector
}

func newRenderEnv(t *testing.T, mode config.RelayReflectorMode) *renderEnv {
	logger := newTestLogger(t)
	cfg := config.AgentConfig{
		HttpRelayReflectorMode:    mode,
		ReflectorWebSocketUpgrade: true,
	}
	rr := newReflectorWithDrain(t, RegistrationReflectorParams{
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

func (e *renderEnv) render(t *testing.T, content string) error {
	t.Helper()
	af, err := acceptfile.NewAcceptFile([]byte(content), e.mgr.config, e.mgr.logger)
	require.NoError(t, err)
	_, err = af.Render(zap.NewNop(), e.mgr.reflectorRenderStep)
	return err
}

func TestRenderRegistersWildcardOrigin(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	plain := newRecordingBackend(t, "plain")

	require.NoError(t, env.render(t, fmt.Sprintf(`{"private": [
		{"method": "any", "origin": "https://*.api.example.net", "path": "/*"},
		{"method": "any", "origin": "%s", "path": "/*"}
	]}`, plain.server.URL)))

	wildcardURI, err := env.reflector.getUriForTarget("https://*.api.example.net")
	require.NoError(t, err)
	wildcardEntry, _, err := env.reflector.parseTargetUri(proxyPath(t, wildcardURI))
	require.NoError(t, err)
	require.NotNil(t, wildcardEntry.wildcard)

	plainURI, err := env.reflector.getUriForTarget(plain.server.URL)
	require.NoError(t, err)
	plainEntry, _, err := env.reflector.parseTargetUri(proxyPath(t, plainURI))
	require.NoError(t, err)
	require.Nil(t, plainEntry.wildcard)
}

func TestRenderedWildcardEnforcesItsOrigin(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	other := newRecordingBackend(t, "b")

	require.NoError(t, env.render(t, `{"private": [
		{"method": "any", "origin": "https://*.api.example.net", "path": "/*"}
	]}`))

	uri, err := env.reflector.getUriForTarget("https://*.api.example.net")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", proxyPath(t, uri)+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, other.hostPort(t))
	rec := httptest.NewRecorder()
	env.reflector.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), ErrClassDestinationRejected)
	hits, _, _ := other.snapshot()
	require.Equal(t, 0, hits)
}

func TestRenderRejectsMalformedWildcardOrigin(t *testing.T) {
	for name, origin := range map[string]string{
		"public suffix": "https://*.com",
		"plaintext":     "http://*.api.example.net",
		"partial label": "https://a*.api.example.net",
	} {
		t.Run(name, func(t *testing.T) {
			env := newRenderEnv(t, config.RelayReflectorAllTraffic)
			err := env.render(t, fmt.Sprintf(`{"private": [
				{"method": "any", "origin": %q, "path": "/*"}
			]}`, origin))
			require.Error(t, err)
		})
	}
}

// With verification off, anything answering the connection can claim the
// authorized name, so a family authorizes nothing.
func TestRenderRejectsWildcardOriginWithTLSVerificationDisabled(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	env.mgr.config.HttpDisableTLS = true

	err := env.render(t, `{"private": [
		{"method": "any", "origin": "https://*.api.example.net", "path": "/*"}
	]}`)

	require.ErrorIs(t, err, ErrWildcardOriginRequiresTLSVerification)
}

// A concrete origin names one host the operator chose, so that stays their
// call and must not break existing deployments.
func TestRenderAllowsConcreteOriginWithTLSVerificationDisabled(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	env.mgr.config.HttpDisableTLS = true
	backend := newRecordingBackend(t, "a")

	require.NoError(t, env.render(t, fmt.Sprintf(`{"private": [
		{"method": "any", "origin": "%s", "path": "/*"}
	]}`, backend.server.URL)))
}

func TestRenderRejectsWildcardOriginWithReflectorDisabled(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorDisabled)
	af, err := acceptfile.NewAcceptFile([]byte(`{"private": [
		{"method": "any", "origin": "https://*.api.example.net", "path": "/*"}
	]}`), env.mgr.config, env.mgr.logger)
	require.NoError(t, err)

	require.PanicsWithValue(t,
		"ENABLE_RELAY_REFLECTOR must be set to 'all' or 'traffic' to use a wildcard origin in accept files",
		func() { _, _ = af.Render(zap.NewNop(), env.mgr.reflectorRenderStep) })
}

func TestRenderLeavesConcreteRuleAlone(t *testing.T) {
	env := newRenderEnv(t, config.RelayReflectorAllTraffic)
	backend := newRecordingBackend(t, "a")

	require.NoError(t, env.render(t, fmt.Sprintf(`{"private": [
		{"method": "any", "origin": "%s", "path": "/*"}
	]}`, backend.server.URL)))

	uri, err := env.reflector.getUriForTarget(backend.server.URL)
	require.NoError(t, err)
	entry, _, err := env.reflector.parseTargetUri(proxyPath(t, uri))
	require.NoError(t, err)
	require.Nil(t, entry.wildcard)
}

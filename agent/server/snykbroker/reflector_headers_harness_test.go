package snykbroker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Tests the shipped Harness accept file end-to-end through the reflector:
// the x-api-key header is injected from HARNESS_TOKEN on proxied requests,
// and an inbound placeholder x-api-key is replaced (not duplicated).
func TestHarnessAcceptFileInjectsApiKeyHeader(t *testing.T) {

	content, err := os.ReadFile("accept_files/accept.harness.json")
	require.NoError(t, err)

	var receivedApiKeys []string
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedApiKeys = r.Header.Values("x-api-key")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	}))
	defer backendServer.Close()

	os.Setenv("HARNESS_API", backendServer.URL)
	os.Setenv("HARNESS_TOKEN", "real-harness-api-key")
	defer func() {
		os.Unsetenv("HARNESS_API")
		os.Unsetenv("HARNESS_TOKEN")
	}()

	logger := zap.NewNop()
	cfg := config.AgentConfig{
		HttpRelayReflectorMode: config.RelayReflectorAllTraffic,
	}
	reflector := NewRegistrationReflector(RegistrationReflectorParams{
		Logger: logger,
		Config: cfg,
	})

	_, err = reflector.Start()
	require.NoError(t, err)
	defer reflector.Stop()

	af, err := acceptfile.NewAcceptFile(content, cfg, nil)
	require.NoError(t, err)

	var proxyURI string
	_, err = af.Render(
		logger,
		func(renderContext acceptfile.RenderContext) error {
			for _, entry := range renderContext.AcceptFile.PrivateRules() {
				originalURI := entry.Origin()
				if originalURI == cfg.HttpBaseUrl() {
					continue
				}
				newURI := reflector.ProxyURI(originalURI, WithHeadersResolver(entry.Headers()))
				entry.SetOrigin(newURI)
				proxyURI = newURI
			}
			return nil
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, proxyURI)

	t.Run("injects x-api-key when absent", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("%s/ng/api/projects", proxyURI))
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, []string{"real-harness-api-key"}, receivedApiKeys)
	})

	t.Run("replaces inbound placeholder x-api-key", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/ng/api/projects", proxyURI), nil)
		require.NoError(t, err)
		req.Header.Set("x-api-key", "placeholder-from-cortex")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		// exactly one value: the placeholder is replaced, not appended to
		require.Equal(t, []string{"real-harness-api-key"}, receivedApiKeys)
	})
}

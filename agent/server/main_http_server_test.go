package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
)

func TestMetricsEndpoint(t *testing.T) {

	server := createMainServer(t)
	url := fmt.Sprintf("http://localhost:%d/metrics", server.Port())
	resp, err := http.DefaultClient.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, "Expected status code 200, got %d", resp.StatusCode)
}

func TestAxonEndpoint(t *testing.T) {

	server := createMainServer(t)
	url := fmt.Sprintf("http://localhost:%d/%s/healthcheck", server.Port(), cortexHttp.AxonPathRoot)
	resp, err := http.DefaultClient.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, "Expected status code 200, got %d", resp.StatusCode)
}

func TestApiEndpoint(t *testing.T) {

	server := createMainServer(t)
	url := fmt.Sprintf("http://localhost:%d/cortex-api/v1/api/catalog", server.Port())
	resp, err := http.DefaultClient.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, "Expected status code 200, got %d", resp.StatusCode)
}

func createMainServer(t *testing.T) cortexHttp.Server {
	t.Helper()

	fakeCortexApi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte(`{"status":"ok"}`))
		require.NoError(t, err, "Failed to write response")
	}))

	logger := zap.NewNop()
	registry := prometheus.NewRegistry()
	lifecycle := fxtest.NewLifecycle(t)

	config := config.NewAgentEnvConfig()
	config.HttpServerPort = 0
	config.CortexApiBaseUrl = fakeCortexApi.URL

	params := MainHttpServerParams{
		Lifecycle: lifecycle,
		Config:    config,
		Logger:    logger,
		Registry:  registry,
	}

	server := NewMainHttpServer(params)

	err := lifecycle.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		err := lifecycle.Stop(context.Background())
		require.NoError(t, err)
	})
	t.Cleanup(fakeCortexApi.Close)
	return server
}

package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newPprofTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	handler := NewPprofHandler(config.AgentConfig{}, zap.NewNop())
	router := mux.NewRouter()
	require.NoError(t, handler.RegisterRoutes(router))
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	return ts
}

func TestPprofIndex(t *testing.T) {
	ts := newPprofTestServer(t)

	resp, err := http.Get(ts.URL + "/pprof/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "goroutine")
}

func TestPprofHeapProfile(t *testing.T) {
	ts := newPprofTestServer(t)

	resp, err := http.Get(ts.URL + "/pprof/heap")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotEmpty(t, body)
}

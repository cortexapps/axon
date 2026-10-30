package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The proxy runs on the main server, which keeps a one-second read timeout.
// A request body that arrives after that must still reach the Cortex API.
func TestServeHTTP_ReadsLateBodyPastServerReadTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "hello", string(body))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	cfg := config.AgentConfig{CortexApiBaseUrl: backend.URL, CortexApiToken: "test_token"}
	srv := cortex_http.NewHttpServer(cortex_http.HttpServerParams{
		Logger:   zap.NewNop(),
		Config:   cfg,
		Handlers: []cortex_http.RegisterableHandler{NewApiProxyHandler(cfg, zap.NewNop(), nil)},
	})
	port, err := srv.Start()
	require.NoError(t, err)
	defer srv.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	_, err = fmt.Fprintf(conn, "POST /cortex-api/test HTTP/1.1\r\nHost: localhost\r\nContent-Length: 5\r\n\r\n")
	require.NoError(t, err)
	time.Sleep(1500 * time.Millisecond)
	_, err = io.WriteString(conn, "hello")
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
}

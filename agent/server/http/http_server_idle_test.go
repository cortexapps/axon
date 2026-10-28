package http

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// sendAndRead writes one keep-alive GET on conn and reads the whole response.
func sendAndRead(t *testing.T, conn net.Conn, r *bufio.Reader) (*http.Response, error) {
	t.Helper()
	_, err := fmt.Fprintf(conn, "GET /block HTTP/1.1\r\nHost: localhost\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, nil
}

// The server set ReadTimeout without IdleTimeout, and net/http falls back to
// ReadTimeout as the keep-alive idle limit. One second is shorter than the gap
// between consecutive requests from the broker, which keeps its connections
// open (Node's agent does so by default), so the broker regularly wrote the
// next request into a socket the server had just closed and got ECONNRESET.
// A connection that sat idle for longer than ReadTimeout must still accept
// the next request.
func TestServer_KeepsKeepAliveConnectionIdlePastReadTimeout(t *testing.T) {
	srv, port := newBlockingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()
	r := bufio.NewReader(conn)

	first, err := sendAndRead(t, conn, r)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, first.StatusCode)

	time.Sleep(1500 * time.Millisecond)

	second, err := sendAndRead(t, conn, r)
	require.NoError(t, err, "server dropped the idle keep-alive connection")
	require.Equal(t, http.StatusOK, second.StatusCode)
}

// newReadTimeoutTestServer starts a server whose handler reads the whole
// request body and answers 500 if it could not.
func newReadTimeoutTestServer(t *testing.T, readTimeout time.Duration) (*httpServer, int) {
	t.Helper()
	srv := NewHttpServer(HttpServerParams{
		Logger: zap.NewNop(),
		Config: config.AgentConfig{HttpReadTimeout: readTimeout},
		Handlers: []RegisterableHandler{&blockingHandler{serve: func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.ReadAll(r.Body); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}}},
		Registry: prometheus.NewRegistry(),
	}).(*httpServer)
	port, err := srv.Start()
	require.NoError(t, err)
	return srv, port
}

// The broker opens a connection and writes its request only when its event
// loop gets to it, and that loop stalls for two to three seconds while it
// encodes a large response. A one-second ReadTimeout, which starts when the
// connection is accepted, closed the connection before the request arrived.
func TestServer_ServesLateRequestOnNewConnection(t *testing.T) {
	srv, port := newReadTimeoutTestServer(t, 0)
	defer srv.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	time.Sleep(1500 * time.Millisecond)

	resp, err := sendAndRead(t, conn, bufio.NewReader(conn))
	require.NoError(t, err, "server closed the connection before the request arrived")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// The same bound covers a request body that arrives late, such as a PUT the
// broker starts writing before a stall and finishes after it.
func TestServer_ReadsLateRequestBody(t *testing.T) {
	srv, port := newReadTimeoutTestServer(t, 0)
	defer srv.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	_, err = fmt.Fprintf(conn, "PUT /block HTTP/1.1\r\nHost: localhost\r\nContent-Length: 5\r\n\r\n")
	require.NoError(t, err)
	time.Sleep(1500 * time.Millisecond)
	_, err = io.WriteString(conn, "hello")
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// AXON_HTTP_READ_TIMEOUT replaces the default, so a configured bound shorter
// than the client's delay closes the connection.
func TestServer_UsesConfiguredReadTimeout(t *testing.T) {
	srv, port := newReadTimeoutTestServer(t, 500*time.Millisecond)
	defer srv.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	time.Sleep(1500 * time.Millisecond)

	_, err = sendAndRead(t, conn, bufio.NewReader(conn))
	require.Error(t, err, "server kept the connection open past the configured read timeout")
}

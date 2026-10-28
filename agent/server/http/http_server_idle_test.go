package http

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

// The broker opens a connection and writes its request only when its event
// loop gets to it. Under load that loop stalls for more than a second, and a
// one-second ReadTimeout, which starts when the connection is accepted, closed
// the connection before the request arrived. Every connection opened before
// the stall failed at once with ECONNRESET.
// A request that arrives more than a second after its connection opened must
// still be served.
func TestServer_ServesRequestArrivingLateOnNewConnection(t *testing.T) {
	srv, port := newBlockingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	time.Sleep(1500 * time.Millisecond)

	resp, err := sendAndRead(t, conn, bufio.NewReader(conn))
	require.NoError(t, err, "server closed the connection before the request arrived")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

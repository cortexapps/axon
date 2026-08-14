package http

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func startTestServer(t *testing.T, opts ...ServerOption) *httpServer {
	t.Helper()
	opts = append(opts, WithRegistry(prometheus.NewRegistry()))
	server := NewHttpServer(HttpServerParams{
		Logger: zap.NewNop(),
		Config: config.AgentConfig{},
	}, opts...).(*httpServer)
	_, err := server.Start()
	require.NoError(t, err)
	t.Cleanup(func() { server.Close() })
	return server
}

// nonLoopbackIPv4 returns a routable IPv4 address of this host, or "" when the
// machine has none - a CI container with only a loopback interface, say.
func nonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipv4 := ipNet.IP.To4(); ipv4 != nil {
			return ipv4.String()
		}
	}
	return ""
}

// The bind is the control, so this asserts the listener's own address rather
// than any behavior a handler could be made to skip.
func TestLoopbackOnlyBindsLoopback(t *testing.T) {
	server := startTestServer(t, WithLoopbackOnly())

	host, _, err := net.SplitHostPort(server.listener.Addr().String())
	require.NoError(t, err)
	require.True(t, net.ParseIP(host).IsLoopback(), "bound to %s, which is reachable off-host", host)
}

func TestDefaultBindsAllInterfaces(t *testing.T) {
	server := startTestServer(t)

	host, _, err := net.SplitHostPort(server.listener.Addr().String())
	require.NoError(t, err)
	require.True(t, net.ParseIP(host).IsUnspecified(), "expected the unspecified address, got %s", host)
}

// The point of the bind: a credential-injecting server must not answer anyone
// who can route to this host.
func TestLoopbackOnlyRefusesOffHostConnection(t *testing.T) {
	external := nonLoopbackIPv4()
	if external == "" {
		t.Skip("host has no non-loopback IPv4 address to test against")
	}

	loopback := startTestServer(t, WithLoopbackOnly())
	allInterfaces := startTestServer(t)

	dialer := net.Dialer{Timeout: 2 * time.Second}

	conn, err := dialer.Dial("tcp", net.JoinHostPort(external, strconv.Itoa(loopback.Port())))
	if err == nil {
		conn.Close()
		t.Fatalf("loopback-only server accepted a connection on %s", external)
	}

	// Proves the address itself is reachable, so the refusal above is the bind
	// and not an unrelated network condition.
	conn, err = dialer.Dial("tcp", net.JoinHostPort(external, strconv.Itoa(allInterfaces.Port())))
	require.NoError(t, err, "the external address should reach a server bound to all interfaces")
	conn.Close()
}

func TestLoopbackOnlyStillServesLoopbackClients(t *testing.T) {
	server := startTestServer(t, WithLoopbackOnly())

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/nope", server.Port()))
	require.NoError(t, err)
	defer resp.Body.Close()
	// Any answer proves reachability; the route does not exist, so it is a 404.
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

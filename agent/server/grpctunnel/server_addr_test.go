package grpctunnel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerDialAddr(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// The shape registration returns in production: a scheme, no port.
		// Through a proxy this became "CONNECT relay.cortex.io", which
		// proxies reject or drop.
		{"https://relay.cortex.io", "relay.cortex.io:443"},
		{"https://relay.cortex.io/", "relay.cortex.io:443"},
		{"http://relay.cortex.io", "relay.cortex.io:443"},
		{"relay.cortex.io", "relay.cortex.io:443"},
		// An explicit port is kept.
		{"https://relay.cortex.io:443", "relay.cortex.io:443"},
		{"https://relay.cortex.io:8443/", "relay.cortex.io:8443"},
		{"grpc-tunnel-server:50052", "grpc-tunnel-server:50052"},
		// IPv6 literals.
		{"https://[::1]", "[::1]:443"},
		{"[::1]:50052", "[::1]:50052"},
		// Empty stays empty, so callers still see "not registered".
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			require.Equal(t, c.want, serverDialAddr(c.in))
		})
	}
}

func TestStoreRegistrationAddsDefaultPort(t *testing.T) {
	tc := &tunnelClient{}
	tc.storeRegistration("tok", "https://relay.cortex.io")

	_, addr := tc.getCurrentConfig()
	require.Equal(t, "relay.cortex.io:443", addr)
}

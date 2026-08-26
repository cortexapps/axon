package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These pin the environment variables the tunnel actually reads. They exist
// because a refactor once silently dropped the GRPC_INSECURE parsing: the
// struct field survived, so everything still compiled and every unit test
// still passed, and the only symptom was every agent failing its TLS
// handshake against a plaintext server under load test.

func TestEnvConfig_TunnelDefaults(t *testing.T) {
	cfg := NewAgentEnvConfig()

	assert.Equal(t, 4, cfg.TunnelConns,
		"connections are the only tunnel dial; default is a blast-radius choice")
	assert.Equal(t, 128, cfg.UpstreamMaxConnsPerHost)
	assert.Equal(t, 256, cfg.MaxInflightRequests)
	assert.False(t, cfg.GrpcInsecure, "TLS stays on unless explicitly disabled")
}

func TestEnvConfig_TunnelConns(t *testing.T) {
	t.Setenv("AXON_GRPC_TUNNEL_CONNS", "9")

	assert.Equal(t, 9, NewAgentEnvConfig().TunnelConns)
}

func TestEnvConfig_TunnelConnsRejectsNonsense(t *testing.T) {
	t.Setenv("AXON_GRPC_TUNNEL_CONNS", "0")
	assert.Panics(t, func() { NewAgentEnvConfig() }, "zero connections cannot serve anything")

	t.Setenv("AXON_GRPC_TUNNEL_CONNS", "banana")
	assert.Panics(t, func() { NewAgentEnvConfig() })
}

func TestEnvConfig_UpstreamMaxConnsPerHost(t *testing.T) {
	t.Setenv("AXON_UPSTREAM_MAX_CONNS_PER_HOST", "32")
	assert.Equal(t, 32, NewAgentEnvConfig().UpstreamMaxConnsPerHost)

	t.Setenv("AXON_UPSTREAM_MAX_CONNS_PER_HOST", "0")
	assert.Panics(t, func() { NewAgentEnvConfig() })
}

// The regression that motivated this file.
func TestEnvConfig_GrpcInsecure(t *testing.T) {
	t.Setenv("GRPC_INSECURE", "true")
	assert.True(t, NewAgentEnvConfig().GrpcInsecure, "GRPC_INSECURE must disable TLS")
}

func TestEnvConfig_GrpcInsecureCanonicalName(t *testing.T) {
	t.Setenv("AXON_GRPC_TUNNEL_INSECURE", "true")
	assert.True(t, NewAgentEnvConfig().GrpcInsecure)
}

func TestEnvConfig_GrpcInsecureOnlyOnExactTrue(t *testing.T) {
	// Anything other than "true" leaves TLS on. Failing closed matters more
	// than convenience for a flag that disables transport security.
	for _, v := range []string{"", "false", "1", "yes", "TRUE"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("GRPC_INSECURE", v)
			assert.False(t, NewAgentEnvConfig().GrpcInsecure)
		})
	}
}

func TestEnvConfig_GrpcTunnelServer(t *testing.T) {
	t.Setenv("GRPC_TUNNEL_SERVER", "tunnel.example.com:443")
	assert.Equal(t, "tunnel.example.com:443", NewAgentEnvConfig().GrpcTunnelServer)
}

func TestEnvConfig_RelayModeSelectsTheTunnel(t *testing.T) {
	t.Setenv("AXON_RELAY_TRANSPORT", "grpc-tunnel")
	require.True(t, NewAgentEnvConfig().IsGRPCTunnel())

	t.Setenv("AXON_RELAY_TRANSPORT", "snyk-broker")
	require.False(t, NewAgentEnvConfig().IsGRPCTunnel())
}

func TestEnvConfig_RelayModeLegacyAlias(t *testing.T) {
	t.Setenv("RELAY_MODE", "grpc-tunnel")
	assert.True(t, NewAgentEnvConfig().IsGRPCTunnel())
}

func TestEnvConfig_MaxRequestTimeout(t *testing.T) {
	t.Setenv("AXON_GRPC_TUNNEL_MAX_REQUEST_TIMEOUT", "90s")
	assert.Equal(t, "1m30s", NewAgentEnvConfig().MaxRequestTimeout.String())
}

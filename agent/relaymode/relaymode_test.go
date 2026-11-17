package relaymode

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
)

func TestPinned(t *testing.T) {
	_, ok := Pinned(config.AgentConfig{})
	assert.False(t, ok, "an unset mode is not pinned")

	m, ok := Pinned(config.AgentConfig{RelayMode: "grpc-tunnel"})
	assert.True(t, ok)
	assert.Equal(t, config.RelayModeGrpcTunnel, m)

	m, ok = Pinned(config.AgentConfig{RelayMode: "snyk-broker"})
	assert.True(t, ok)
	assert.Equal(t, config.RelayModeSnykBroker, m)

	// Any configured value pins the mode. A near miss runs snyk-broker, as
	// it always did.
	m, ok = Pinned(config.AgentConfig{RelayMode: "grpc_tunnel"})
	assert.True(t, ok)
	assert.Equal(t, config.RelayModeSnykBroker, m)
}

func TestFromRegistration(t *testing.T) {
	cases := []struct {
		in    string
		mode  config.RelayMode
		known bool
	}{
		{"", Default, true},
		{"snyk-broker", config.RelayModeSnykBroker, true},
		{"grpc-tunnel", config.RelayModeGrpcTunnel, true},
		{"GRPC-TUNNEL", Default, false},
		{"carrier-pigeon", Default, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			mode, known := FromRegistration(c.in)
			assert.Equal(t, c.mode, mode)
			assert.Equal(t, c.known, known)
		})
	}
}

func TestEffective(t *testing.T) {
	assert.Equal(t, Default, Effective(config.AgentConfig{}))
	assert.Equal(t, config.RelayModeGrpcTunnel, Effective(config.AgentConfig{RelayMode: "grpc-tunnel"}))
}

func TestStatus(t *testing.T) {
	s := NewStatus()
	assert.Equal(t, config.RelayMode(""), s.Active())
	s.Set(config.RelayModeGrpcTunnel)
	assert.Equal(t, config.RelayModeGrpcTunnel, s.Active())
}

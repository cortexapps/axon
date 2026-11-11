package grpctunnel

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every agent used to send client_version "0.0.1", the registration protocol
// version, so the tunnel server could not tell one agent build from another.
// The hello now carries the build version, the relay mode and the image
// variant.
func TestHello_CarriesBuildVersionAndRelayMode(t *testing.T) {
	t.Setenv("AXON_BUILD_VERSION", "3.1.4")
	hello := captureHello(t, config.AgentConfig{InstanceId: "agent-v", RelayMode: "grpc-tunnel"}, "broker-token-value")

	require.NotNil(t, hello)
	assert.Equal(t, "3.1.4", hello.ClientVersion)
	assert.Equal(t, "grpc-tunnel", hello.Metadata["relay_mode"])
	assert.Equal(t, "full", hello.Metadata["image_variant"])
}

func TestHello_BuildVersionDefaultsToDev(t *testing.T) {
	t.Setenv("AXON_BUILD_VERSION", "")
	hello := captureHello(t, config.AgentConfig{InstanceId: "agent-d", RelayMode: "grpc-tunnel"}, "broker-token-value")

	require.NotNil(t, hello)
	assert.Equal(t, "dev", hello.ClientVersion)
}

func TestHelloMetadata_GoOnlyImage(t *testing.T) {
	prev := config.ForcedRelayMode
	config.ForcedRelayMode = "grpc-tunnel"
	defer func() { config.ForcedRelayMode = prev }()

	md := helloMetadata(config.AgentConfig{RelayMode: "grpc-tunnel"})
	assert.Equal(t, "grpc", md["image_variant"])
	assert.Equal(t, "grpc-tunnel", md["relay_mode"])
}

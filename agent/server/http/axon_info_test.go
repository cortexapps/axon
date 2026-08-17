package http

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// __axon/info reports which relay transport an agent is running so the fleet's
// transport mix is visible from the backend. The endpoint itself needs a live
// gRPC server to list handlers, so it is exercised end to end by
// relay_scenarios.sh against both transports; what is unit-tested here is the
// value it reports.

func modeFor(t *testing.T, cfg config.AgentConfig) string {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	h := NewAxonHandler(AxonHandlerParams{Logger: logger, Config: cfg}).(*axonHandler)
	return h.relayMode()
}

func TestRelayMode_GrpcTunnel(t *testing.T) {
	assert.Equal(t, "grpc-tunnel", modeFor(t, config.AgentConfig{
		RelayMode: string(config.RelayModeGrpcTunnel),
	}))
}

func TestRelayMode_SnykBroker(t *testing.T) {
	assert.Equal(t, "snyk-broker", modeFor(t, config.AgentConfig{
		RelayMode: string(config.RelayModeSnykBroker),
	}))
}

// An unset transport is the snyk-broker default, not an unknown. Reporting the
// effective value keeps the backend from having to encode that default, and
// stops an empty string reading as "this agent did not answer".
func TestRelayMode_UnsetReportsTheDefault(t *testing.T) {
	assert.Equal(t, "snyk-broker", modeFor(t, config.AgentConfig{}))
}

// Always populated, so a fleet view never has a blank column. Outside a
// container image this is the "dev" fallback rather than a build tag.
func TestBuildVersion_AlwaysPopulated(t *testing.T) {
	assert.NotEmpty(t, getBuildVersion())

	t.Setenv("AXON_BUILD_VERSION", "grpc-tunnel-initial-commit-abc1234")
	assert.Equal(t, "grpc-tunnel-initial-commit-abc1234", getBuildVersion())
}

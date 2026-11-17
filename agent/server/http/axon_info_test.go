package http

import (
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The value reported is the effective transport, not the configured string.
// Only an exact "grpc-tunnel" selects the tunnel — config.IsGRPCTunnel compares
// exactly — so a near miss like "grpc_tunnel" runs snyk-broker, and reporting
// it verbatim would put a transport nobody is running into the fleet's mix.
func TestRelayMode_UnrecognizedReportsTheEffectiveMode(t *testing.T) {
	assert.Equal(t, "snyk-broker", modeFor(t, config.AgentConfig{
		RelayMode: "grpc_tunnel",
	}))
}

// Always populated, so a fleet view never has a blank column. Outside a
// container image this is the "dev" fallback rather than a build tag.
func TestBuildVersion_AlwaysPopulated(t *testing.T) {
	assert.NotEmpty(t, getBuildVersion())

	t.Setenv("AXON_BUILD_VERSION", "grpc-tunnel-initial-commit-abc1234")
	assert.Equal(t, "grpc-tunnel-initial-commit-abc1234", getBuildVersion())
}

// /healthcheck carries the build version too, not just /info. The tunnel
// server reports build_version from its own /healthcheck, and having the two
// halves answer "which build is this" on different paths is exactly the
// divergence this keeps closed. Unlike /info, health needs no live gRPC
// server, so it is exercised directly here.
func TestHealthcheck_ReportsBuildVersion(t *testing.T) {
	t.Setenv("AXON_BUILD_VERSION", "v0.2.12-abc1234")

	logger, _ := zap.NewDevelopment()
	h := NewAxonHandler(AxonHandlerParams{Logger: logger, Config: config.AgentConfig{}}).(*axonHandler)

	rec := httptest.NewRecorder()
	h.healthcheck(rec, httptest.NewRequest(nethttp.MethodGet, "/healthcheck", nil))

	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "health body is not valid JSON: %s", rec.Body.String())
	// The existing liveness field has to survive: callers already parse it.
	assert.Equal(t, true, body["OK"])
	assert.Equal(t, "v0.2.12-abc1234", body["build_version"])
}

// An unstamped agent says "dev" rather than emitting a blank field, matching
// the tunnel server's fallback.
func TestHealthcheck_UnstampedReportsDev(t *testing.T) {
	t.Setenv("AXON_BUILD_VERSION", "")

	logger, _ := zap.NewDevelopment()
	h := NewAxonHandler(AxonHandlerParams{Logger: logger, Config: config.AgentConfig{}}).(*axonHandler)

	rec := httptest.NewRecorder()
	h.healthcheck(rec, httptest.NewRequest(nethttp.MethodGet, "/healthcheck", nil))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "dev", body["build_version"])
}

package relaymode

import (
	"context"
	"errors"
	"testing"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/stretchr/testify/assert"
)

type fakeRegistration struct {
	tunnelMode string
	err        error
	calls      int
}

func (f *fakeRegistration) Register(ctx context.Context, _ common.Integration, _ string) (*snykbroker.RegistrationInfoResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &snykbroker.RegistrationInfoResponse{ServerUri: "https://relay", Token: "t", TunnelMode: f.tunnelMode}, nil
}

func resolve(t *testing.T, cfg config.AgentConfig, reg *fakeRegistration) config.RelayMode {
	t.Helper()
	t.Setenv("BROKER_SERVER_URL", "")
	t.Setenv("BROKER_TOKEN", "")
	mode, reason := Resolve(context.Background(), cfg, common.IntegrationInfo{Integration: common.IntegrationGithub}, reg)
	assert.NotEmpty(t, reason)
	return mode
}

func TestResolve_ConfigurationWins(t *testing.T) {
	reg := &fakeRegistration{tunnelMode: "grpc-tunnel"}
	assert.Equal(t, config.RelayModeSnykBroker, resolve(t, config.AgentConfig{RelayMode: "snyk-broker"}, reg))
	assert.Equal(t, 0, reg.calls, "a configured mode needs no registration")

	assert.Equal(t, config.RelayModeGrpcTunnel, resolve(t, config.AgentConfig{RelayMode: "grpc-tunnel"}, &fakeRegistration{}))

	// A near miss runs snyk-broker, as it always did.
	assert.Equal(t, config.RelayModeSnykBroker, resolve(t, config.AgentConfig{RelayMode: "grpc_tunnel"}, &fakeRegistration{tunnelMode: "grpc-tunnel"}))
}

func TestResolve_FromCortex(t *testing.T) {
	cases := []struct {
		name string
		reg  *fakeRegistration
		want config.RelayMode
	}{
		{"grpc-tunnel", &fakeRegistration{tunnelMode: "grpc-tunnel"}, config.RelayModeGrpcTunnel},
		{"snyk-broker", &fakeRegistration{tunnelMode: "snyk-broker"}, config.RelayModeSnykBroker},
		{"no preference", &fakeRegistration{}, Default},
		{"unknown value", &fakeRegistration{tunnelMode: "carrier-pigeon"}, Default},
		{"registration fails", &fakeRegistration{err: errors.New("cortex is down")}, Default},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolve(t, config.AgentConfig{}, c.reg))
			assert.Equal(t, 1, c.reg.calls)
		})
	}
}

func TestResolve_NoRegistrationWhenTheTunnelsSkipIt(t *testing.T) {
	reg := &fakeRegistration{tunnelMode: "grpc-tunnel"}
	assert.Equal(t, Default, resolve(t, config.AgentConfig{DryRun: true}, reg))
	assert.Equal(t, 0, reg.calls)

	t.Setenv("BROKER_SERVER_URL", "https://relay")
	t.Setenv("BROKER_TOKEN", "t")
	mode, _ := Resolve(context.Background(), config.AgentConfig{}, common.IntegrationInfo{}, reg)
	assert.Equal(t, Default, mode)
	assert.Equal(t, 0, reg.calls)
}

package relaymode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRegistration answers each call with the next response in answers, and
// repeats the last one after that.
type fakeRegistration struct {
	mu      sync.Mutex
	answers []answer
	calls   int
}

type answer struct {
	tunnelMode string
	err        error
}

func answers(a ...answer) *fakeRegistration { return &fakeRegistration{answers: a} }

func mode(m string) answer { return answer{tunnelMode: m} }

func failure() answer { return answer{err: errors.New("cortex is down")} }

func (f *fakeRegistration) Register(ctx context.Context, _ common.Integration, _ string) (*snykbroker.RegistrationInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.answers[min(f.calls, len(f.answers)-1)]
	f.calls++
	if a.err != nil {
		return nil, a.err
	}
	return &snykbroker.RegistrationInfoResponse{ServerUri: "https://relay", Token: "t", TunnelMode: a.tunnelMode}, nil
}

func (f *fakeRegistration) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func resolve(t *testing.T, cfg config.AgentConfig, reg *fakeRegistration) Result {
	t.Helper()
	t.Setenv("BROKER_SERVER_URL", "")
	t.Setenv("BROKER_TOKEN", "")
	res := Resolve(context.Background(), cfg, common.IntegrationInfo{Integration: common.IntegrationGithub}, reg)
	assert.NotEmpty(t, res.Reason)
	return res
}

func TestResolve_ConfigurationWins(t *testing.T) {
	reg := answers(mode("grpc-tunnel"))
	res := resolve(t, config.AgentConfig{RelayMode: "snyk-broker"}, reg)
	assert.Equal(t, config.RelayModeSnykBroker, res.Mode)
	assert.False(t, res.Remote, "Cortex cannot change a configured mode")
	assert.Equal(t, 0, reg.callCount(), "a configured mode needs no registration")

	assert.Equal(t, config.RelayModeGrpcTunnel, resolve(t, config.AgentConfig{RelayMode: "grpc-tunnel"}, answers(mode(""))).Mode)

	// A near miss runs snyk-broker, as it always did.
	assert.Equal(t, config.RelayModeSnykBroker, resolve(t, config.AgentConfig{RelayMode: "grpc_tunnel"}, answers(mode("grpc-tunnel"))).Mode)
}

func TestResolve_FromCortex(t *testing.T) {
	cases := []struct {
		name string
		reg  *fakeRegistration
		want config.RelayMode
	}{
		{"grpc-tunnel", answers(mode("grpc-tunnel")), config.RelayModeGrpcTunnel},
		{"snyk-broker", answers(mode("snyk-broker")), config.RelayModeSnykBroker},
		{"no preference", answers(mode("")), Default},
		{"unknown value", answers(mode("carrier-pigeon")), Default},
		{"registration fails", answers(failure()), Default},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := resolve(t, config.AgentConfig{}, c.reg)
			assert.Equal(t, c.want, res.Mode)
			assert.True(t, res.Remote, "Cortex drives the mode, also after a failed registration")
			assert.Equal(t, 1, c.reg.callCount())
		})
	}
}

func TestResolve_NoRegistrationWhenTheTunnelsSkipIt(t *testing.T) {
	reg := answers(mode("grpc-tunnel"))
	res := resolve(t, config.AgentConfig{DryRun: true}, reg)
	assert.Equal(t, Default, res.Mode)
	assert.False(t, res.Remote)
	assert.Equal(t, 0, reg.callCount())

	t.Setenv("BROKER_SERVER_URL", "https://relay")
	t.Setenv("BROKER_TOKEN", "t")
	res = Resolve(context.Background(), config.AgentConfig{}, common.IntegrationInfo{}, reg)
	assert.Equal(t, Default, res.Mode)
	assert.False(t, res.Remote)
	assert.Equal(t, 0, reg.callCount())
}

// watch runs Watch until it reports a change or the time is up, and returns
// the new mode, or "" for no change.
func watch(t *testing.T, reg *fakeRegistration, running config.RelayMode, wantCalls int) config.RelayMode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var changed config.RelayMode
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, time.Millisecond, common.IntegrationInfo{}, reg, running, func(m config.RelayMode) { changed = m })
	}()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return reg.callCount() >= wantCalls
		}
	}, 2*time.Second, time.Millisecond)
	cancel()
	<-done
	return changed
}

func TestWatch_ConfirmedChangeRestarts(t *testing.T) {
	assert.Equal(t, config.RelayModeGrpcTunnel,
		watch(t, answers(mode("snyk-broker"), mode("grpc-tunnel")), config.RelayModeSnykBroker, 2))

	// Cortex clears its preference, and the default differs from the running mode.
	assert.Equal(t, Default,
		watch(t, answers(mode("")), config.RelayModeGrpcTunnel, 1))
}

func TestWatch_FailuresChangeNothing(t *testing.T) {
	// The registration at start failed, so the default runs. Failures after
	// that are not a change; the first success that differs is.
	reg := answers(failure(), failure(), failure(), mode("grpc-tunnel"))
	assert.Equal(t, config.RelayModeGrpcTunnel, watch(t, reg, Default, 4))

	reg = answers(failure())
	assert.Equal(t, config.RelayMode(""), watch(t, reg, config.RelayModeGrpcTunnel, 20))
}

func TestWatch_SameOrUnknownModeChangesNothing(t *testing.T) {
	assert.Equal(t, config.RelayMode(""), watch(t, answers(mode("grpc-tunnel")), config.RelayModeGrpcTunnel, 20))
	assert.Equal(t, config.RelayMode(""), watch(t, answers(mode("")), Default, 20))
	assert.Equal(t, config.RelayMode(""), watch(t, answers(mode("carrier-pigeon")), config.RelayModeGrpcTunnel, 20))
}

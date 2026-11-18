package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type grpcTunnelRegistration struct{}

func (grpcTunnelRegistration) Register(context.Context, common.Integration, string) (*snykbroker.RegistrationInfoResponse, error) {
	return &snykbroker.RegistrationInfoResponse{TunnelMode: "grpc-tunnel"}, nil
}

// When Cortex selects a different mode, the watcher stops the app and
// requests a restart. startAgent then starts the agent again.
func TestRestartOnRelayModeChange_StopsTheApp(t *testing.T) {
	restartRequested.Store(false)
	t.Cleanup(func() { restartRequested.Store(false) })

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Provide(func() snykbroker.Registration { return grpcTunnelRegistration{} }),
		restartOnRelayModeChange(config.AgentConfig{AutoRegisterFrequency: time.Millisecond},
			common.IntegrationInfo{}, config.RelayModeSnykBroker),
	)
	require.NoError(t, app.Start(context.Background()))

	select {
	case <-app.Wait():
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop the app")
	}
	require.True(t, restartRequested.Load())
	require.NoError(t, app.Stop(context.Background()))
}

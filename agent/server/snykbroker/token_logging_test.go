package snykbroker

import (
	"strings"
	"testing"

	"github.com/cortexapps/axon/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The broker token is the sole credential on the relay path, and the agent
// writes its logs into the customer's log sink. So no log line may carry the
// raw token. It is logged as the token hash on both the relay server and
// BROKER_SERVER log for the same token, which keeps the lines joinable.
func TestRefreshTokenInfo_LogsTokenHashNotRawToken(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const rawToken = "4f49654b-1111-2222-3333-9deef1d9f2f6"

	t.Setenv("BROKER_SERVER_URL", "")
	t.Setenv("BROKER_TOKEN", "")

	mockRegistration := NewMockRegistration(ctrl)
	mockRegistration.EXPECT().Register(gomock.Any(), gomock.Any()).Return(
		&RegistrationInfoResponse{
			ServerUri: "https://relay.example.com",
			Token:     rawToken,
		}, nil,
	)

	core, logs := observer.New(zapcore.DebugLevel)

	mgr := &relayInstanceManager{
		logger:          zap.New(core),
		registration:    mockRegistration,
		integrationInfo: defaultIntegrationInfo,
		operationsCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "broker_operations_test"},
			[]string{"integration", "alias", "operation", "status"},
		),
	}

	info, err := mgr.refreshTokenInfo()
	require.NoError(t, err)
	require.True(t, info.HasChanged, "the first registration is a change")

	entries := logs.All()
	require.NotEmpty(t, entries, "refreshTokenInfo should log the change")

	for _, entry := range entries {
		require.NotContains(t, entry.Message, rawToken)
		for key, value := range entry.ContextMap() {
			s, ok := value.(string)
			if !ok {
				continue
			}
			require.NotContains(t, s, rawToken, "field %q leaks the raw token", key)
			require.False(t, strings.Contains(rawToken, s) && s != "",
				"field %q leaks part of the raw token", key)
		}
	}

	changed := logs.FilterMessage("Registration info has changed").All()
	require.Len(t, changed, 1)
	require.Equal(t, common.TokenHash(rawToken), changed[0].ContextMap()["tokenHash"])
}

func TestTokenHash_EmptyTokenHashesToEmpty(t *testing.T) {
	require.Empty(t, common.TokenHash(""), "an absent token must not look like a real one")
}

package snykbroker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx/fxtest"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
)

func TestManagerSuccess(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	mgr := createTestRelayInstanceManager(t, controller, nil)

	err := mgr.Start()
	require.NoError(t, err)
	err = mgr.Close()
	require.NoError(t, err)

}

func TestManagerUnauthorized(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	mgr := createTestRelayInstanceManager(t, controller, ErrUnauthorized)

	err := mgr.Start()
	require.Error(t, err, ErrUnauthorized)

}

// POST request successfully restarts supervisor and returns 200 OK
func TestRelayRestartServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mgr := createTestRelayInstanceManager(t, ctrl, nil)

	err := mgr.Start()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	httpHandler := (mgr.RelayInstanceManager).(http.Handler)
	httpHandler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, int(mgr.Instance().startCount.Load()))

}

type wrappedRelayInstanceManager struct {
	RelayInstanceManager
	mockRegistration *MockRegistration
}

func (w *wrappedRelayInstanceManager) Instance() *relayInstanceManager {
	return w.RelayInstanceManager.(*relayInstanceManager)
}

func createTestRelayInstanceManager(t *testing.T, controller *gomock.Controller, expectedError error) *wrappedRelayInstanceManager {
	envVars := map[string]string{
		"ACCEPT_FILE_DIR":  "./accept_files",
		"GITHUB_TOKEN":     "the-token",
		"GITHUB_API":       "api.github.com",
		"GITHUB_GRAPHQL":   "api.github.com/graphql",
		"SNYK_BROKER_PATH": "sleep",
		"SNYK_BROKER_ARGS": "60",
	}

	common.ApplyEnv(envVars)

	lifecycle := fxtest.NewLifecycle(t)
	config := config.NewAgentEnvConfig()
	config.FailWaitTime = time.Millisecond * 100
	logger := zap.NewNop()
	ii := common.IntegrationInfo{
		Integration: common.IntegrationGithub,
	}
	mockServer := cortex_http.NewMockServer(controller)
	mockServer.EXPECT().RegisterHandler(gomock.Any()).MinTimes(1)

	mockRegistration := NewMockRegistration(controller)

	response := &RegistrationInfoResponse{
		ServerUri: "http://broker.cortex.io",
		Token:     "abcd1234",
	}

	if expectedError != nil {
		mockRegistration.EXPECT().Register(gomock.Eq(common.IntegrationGithub), gomock.Eq("")).MinTimes(1).Return(nil, expectedError)
	} else {
		mockRegistration.EXPECT().Register(gomock.Eq(common.IntegrationGithub), gomock.Eq("")).MinTimes(1).Return(response, nil)
	}

	return &wrappedRelayInstanceManager{
		RelayInstanceManager: NewRelayInstanceManager(
			lifecycle,
			config,
			logger,
			ii,
			mockServer,
			mockRegistration,
		),
		mockRegistration: mockRegistration,
	}
}

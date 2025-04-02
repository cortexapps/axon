package snykbroker

import (
	"net"
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

func TestApplyValidationConfig(t *testing.T) {

	validationConfig := &common.ValidationConfig{
		URL:    "https://api.github.com/user",
		Method: "POST",
		Auth: common.Auth{
			Type:  "header",
			Value: "bearer the-token",
		},
	}

	envVars := map[string]string{}

	mgr := &relayInstanceManager{}

	mgr.applyClientValidationConfig(validationConfig, envVars)
	assert.Equal(t, "https://api.github.com/user", envVars["BROKER_CLIENT_VALIDATION_URL"])
	assert.Equal(t, "POST", envVars["BROKER_CLIENT_VALIDATION_METHOD"])
	assert.Equal(t, "bearer the-token", envVars["BROKER_CLIENT_VALIDATION_AUTHORIZATION_HEADER"])

}

// POST request successfully restarts supervisor and returns 200 OK
func TestRelayRestartServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mgr := createTestRelayInstanceManager(t, ctrl, nil)

	err := mgr.Start()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__axon/broker/restart", nil)
	httpHandler := (mgr.RelayInstanceManager).(cortex_http.RegisterableHandler)

	mux := http.NewServeMux()
	httpHandler.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, int(mgr.Instance().startCount.Load()))

}

func TestRelayReRegisterServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mgr := createTestRelayInstanceManager(t, ctrl, nil)

	err := mgr.Start()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__axon/broker/reregister", nil)
	httpHandler := (mgr.RelayInstanceManager).(cortex_http.RegisterableHandler)

	mux := http.NewServeMux()
	httpHandler.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, int(mgr.Instance().startCount.Load()))

}

func TestSystemCheck(t *testing.T) {

	jsonPayload := `
			[
			{
				"brokerClientValidationUrl": "https://api.github.com/user",
				"brokerClientValidationMethod": "GET",
				"brokerClientValidationTimeoutMs": 5000,
				"brokerClientValidationUrlStatusCode": 401,
				"ok": false,
				"error": "Failed due to invalid credentials",
				"maskedCredentials": "ghp***sIX"
			},
			{
				"brokerClientValidationUrl": "https://api.github.com/user",
				"brokerClientValidationMethod": "GET",
				"brokerClientValidationTimeoutMs": 5000,
				"brokerClientValidationUrlStatusCode": 200,
				"ok": true,
				"maskedCredentials": "ghp***ICu"
			}
			]
	`

	// Create a mock HTTP server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/systemcheck" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(jsonPayload))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	mgr := &relayInstanceManager{
		config: config.AgentConfig{
			SnykBrokerPort: mockServer.Listener.Addr().(*net.TCPAddr).Port,
		},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/__axon/broker/systemcheck", nil)

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, jsonPayload, w.Body.String())
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
		"GITHUB_API":       "https://api.github.com",
		"GITHUB_GRAPHQL":   "https://api.github.com/graphql",
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

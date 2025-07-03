package snykbroker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/util"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx/fxtest"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
)

func TestManagerSuccess(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	mgr := createTestRelayInstanceManager(t, controller, nil, false)

	err := mgr.Close()
	require.NoError(t, err)

}

func TestManagerSuccessWithReflector(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	mgr := createTestRelayInstanceManager(t, controller, nil, true)

	// call the reflector uri
	uri := mgr.reflector.ProxyURI(mgr.serverUri)

	req, err := http.NewRequest(http.MethodGet, uri+"/foo/bar", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	err = mgr.Close()
	require.NoError(t, err)

	require.Equal(t, 1, len(mgr.requestUrls), "Expected one request to the reflector URI")
	require.Equal(t, "/foo/bar", mgr.requestUrls[0].Path, "Expected request to the reflector URI to have the correct path")

}

func TestManagerUnauthorized(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	mgr := createTestRelayInstanceManager(t, controller, ErrUnauthorized, false)

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
	require.Equal(t, "https://api.github.com/user", envVars["BROKER_CLIENT_VALIDATION_URL"])
	require.Equal(t, "POST", envVars["BROKER_CLIENT_VALIDATION_METHOD"])
	require.Equal(t, "bearer the-token", envVars["BROKER_CLIENT_VALIDATION_AUTHORIZATION_HEADER"])

}

func TestLoadCertsDir(t *testing.T) {
	mgr := &relayInstanceManager{}

	path := mgr.getCertFilePath("../../test/certs")
	require.Equal(t, "../../test/certs/selfsigned-1.pem", path)
}

func TestLoadCertsFile(t *testing.T) {
	mgr := &relayInstanceManager{}

	path := mgr.getCertFilePath("../../test/certs/selfsigned-2.pem")
	require.Equal(t, "../../test/certs/selfsigned-2.pem", path)
}

func TestHttpProxy(t *testing.T) {
	oldEnv := util.SaveEnv(false)
	defer util.RestoreEnv(oldEnv)
	os.Setenv("HTTP_PROXY", "http://proxy.example.com:8080")
	os.Setenv("HTTPS_PROXY", "http://proxy.example.com:8080")
	os.Setenv("NO_PROXY", "localhost")

	cfg := config.AgentConfig{
		HttpCaCertFilePath: "../../test/certs/selfsigned-1.pem",
	}

	mgr := &relayInstanceManager{
		config: cfg,
	}
	env := map[string]string{}
	mgr.setHttpProxyEnvVars(env)

	require.Equal(t, "http://proxy.example.com:8080", env["HTTP_PROXY"])
	require.Equal(t, "http://proxy.example.com:8080", env["HTTPS_PROXY"])
	require.Equal(t, "localhost,127.0.0.1", env["NO_PROXY"])
	require.Equal(t, cfg.HttpCaCertFilePath, env["NODE_EXTRA_CA_CERTS"])

}

// POST request successfully restarts supervisor and returns 200 OK
func TestRelayRestartServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mgr := createTestRelayInstanceManager(t, ctrl, nil, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__axon/broker/restart", nil)
	httpHandler := (mgr.RelayInstanceManager).(cortex_http.RegisterableHandler)

	mux := mux.NewRouter()
	httpHandler.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 2, int(mgr.Instance().startCount.Load()))

}

func TestRelayReRegisterServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mgr := createTestRelayInstanceManager(t, ctrl, nil, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__axon/broker/reregister", nil)
	httpHandler := (mgr.RelayInstanceManager).(cortex_http.RegisterableHandler)

	mux := mux.NewRouter()
	httpHandler.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, int(mgr.Instance().startCount.Load()))

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

	mux := mux.NewRouter()
	mgr.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	// Verify the response
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, jsonPayload, w.Body.String())
}

type wrappedRelayInstanceManager struct {
	RelayInstanceManager
	mockRegistration *MockRegistration
	reflector        *RegistrationReflector
	requestUrls      []url.URL
	serverUri        string
}

func (w *wrappedRelayInstanceManager) Instance() *relayInstanceManager {
	return w.RelayInstanceManager.(*relayInstanceManager)
}

func createTestRelayInstanceManager(t *testing.T, controller *gomock.Controller, expectedError error, useReflector bool) *wrappedRelayInstanceManager {
	envVars := map[string]string{
		"ACCEPTFILE_DIR":   "./accept_files",
		"GITHUB_TOKEN":     "the-token",
		"GITHUB_API":       "https://api.github.com",
		"GITHUB_GRAPHQL":   "https://api.github.com/graphql",
		"SNYK_BROKER_PATH": "sleep",
		"SNYK_BROKER_ARGS": "60",
	}

	common.ApplyEnv(envVars)

	lifecycle := fxtest.NewLifecycle(t)
	cfg := config.NewAgentEnvConfig()
	cfg.FailWaitTime = time.Millisecond * 100
	cfg.HttpRelayReflectorMode = config.RelayReflectorDisabled
	if useReflector {
		cfg.HttpRelayReflectorMode = config.RelayReflectorAllTraffic
	}
	logger := zap.NewNop()
	ii := common.IntegrationInfo{
		Integration: common.IntegrationGithub,
	}
	mockServer := cortex_http.NewMockServer(controller)
	mockServer.EXPECT().RegisterHandler(gomock.Any()).MinTimes(1)

	mockRegistration := NewMockRegistration(controller)

	registry := prometheus.NewRegistry()

	var reflector *RegistrationReflector
	if useReflector {
		params := RegistrationReflectorParams{
			Lifecycle: lifecycle,
			Logger:    logger.Named("reflector"),
			Config:    cfg,
		}
		reflector = NewRegistrationReflector(
			params,
		)
	}

	params := RelayInstanceManagerParams{
		Lifecycle:       lifecycle,
		Config:          cfg,
		Logger:          logger,
		IntegrationInfo: ii,
		HttpServer:      mockServer,
		Registration:    mockRegistration,
		Registry:        registry,
		Reflector:       reflector,
	}

	mgr := &wrappedRelayInstanceManager{
		RelayInstanceManager: NewRelayInstanceManager(
			params,
		),
		mockRegistration: mockRegistration,
		reflector:        reflector,
	}

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mgr.requestUrls = append(mgr.requestUrls, *r.URL)
	}))

	response := &RegistrationInfoResponse{
		ServerUri: testServer.URL,
		Token:     "abcd1234",
	}
	mgr.serverUri = testServer.URL

	if expectedError != nil {
		mockRegistration.EXPECT().Register(gomock.Eq(common.IntegrationGithub), gomock.Eq("")).MinTimes(1).Return(nil, expectedError)
	} else {
		mockRegistration.EXPECT().Register(gomock.Eq(common.IntegrationGithub), gomock.Eq("")).MinTimes(1).Return(response, nil)
	}

	lifecycle.Start(context.Background())

	return mgr
}

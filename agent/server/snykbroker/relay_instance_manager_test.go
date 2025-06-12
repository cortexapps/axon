package snykbroker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/PaesslerAG/jsonpath"
	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/util"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
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
	assert.Equal(t, "/foo/bar", mgr.requestUrls[0].Path, "Expected request to the reflector URI to have the correct path")

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
	assert.Equal(t, "https://api.github.com/user", envVars["BROKER_CLIENT_VALIDATION_URL"])
	assert.Equal(t, "POST", envVars["BROKER_CLIENT_VALIDATION_METHOD"])
	assert.Equal(t, "bearer the-token", envVars["BROKER_CLIENT_VALIDATION_AUTHORIZATION_HEADER"])

}

func TestLoadCertsDir(t *testing.T) {
	mgr := &relayInstanceManager{}

	path := mgr.getCertFilePath("../../test/certs")
	assert.Equal(t, "../../test/certs/selfsigned-1.pem", path)
}

func TestLoadCertsFile(t *testing.T) {
	mgr := &relayInstanceManager{}

	path := mgr.getCertFilePath("../../test/certs/selfsigned-2.pem")
	assert.Equal(t, "../../test/certs/selfsigned-2.pem", path)
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

	assert.Equal(t, "http://proxy.example.com:8080", env["HTTP_PROXY"])
	assert.Equal(t, "http://proxy.example.com:8080", env["HTTPS_PROXY"])
	assert.Equal(t, "localhost", env["NO_PROXY"])
	assert.Equal(t, cfg.HttpCaCertFilePath, env["NODE_EXTRA_CA_CERTS"])

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

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, int(mgr.Instance().startCount.Load()))

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

	mux := mux.NewRouter()
	mgr.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, jsonPayload, w.Body.String())
}

func TestApplyAcceptTransforms(t *testing.T) {

	lifecycle := fxtest.NewLifecycle(t)
	cfg := config.NewAgentEnvConfig()
	logger := zap.NewNop()
	cfg.HttpRelayReflectorMode = config.RelayReflectorAllTraffic

	params := RegistrationReflectorParams{
		Lifecycle: lifecycle,
		Logger:    logger.Named("reflector"),
		Config:    cfg,
	}
	reflector := NewRegistrationReflector(
		params,
	)
	reflector.Start()
	defer reflector.Stop()

	mgr := &relayInstanceManager{
		config:    cfg,
		reflector: reflector,
	}

	cases := []struct {
		acceptFile string
		env        map[string]string
	}{
		{
			acceptFile: "accept_files/accept.github.json",
			env: map[string]string{
				"GITHUB_API":     "api.github.com",
				"GITHUB_GRAPHQL": "api.github.com/graphql",
			},
		},
		{
			acceptFile: "accept_files/accept.bitbucket.basic.json",
			env: map[string]string{
				"BITBUCKET_API": "api.bitbucket.com",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.acceptFile, func(t *testing.T) {
			// Set environment variables
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			// validate it doesn't do it when mode is
			cfgCopy := cfg
			cfgCopy.HttpRelayReflectorMode = config.RelayReflectorRegistrationOnly
			mgr.config = cfgCopy

			newFile := mgr.applyAcceptFileTransforms(c.acceptFile)
			// Check that the new file is the same as the original
			assert.Equal(t, c.acceptFile, newFile, "Expected the accept file to not be transformed when reflector mode is disabled")

			mgr.config = cfg // reset the config to the original

			// Apply the accept file transforms
			newFile = mgr.applyAcceptFileTransforms(c.acceptFile)
			// Check that the new file is not the same as the original
			assert.NotEqual(t, c.acceptFile, newFile, "Expected the accept file to be transformed")

			// gather all of the "origin" values
			newFileContent, err := os.ReadFile(newFile)
			require.NoError(t, err, "Failed to read transformed accept file")

			v := any(nil)

			err = json.Unmarshal(newFileContent, &v)
			require.NoError(t, err, "Failed to unmarshal transformed accept file")
			origins, err := jsonpath.Get("$.private[*].origin", v)
			require.NoError(t, err, "Failed to get origins from transformed accept file")
			for _, origin := range origins.([]any) {
				originStr, ok := origin.(string)
				require.True(t, ok, "Expected origin to be a string")
				// Check that the origin is not empty
				assert.NotEmpty(t, originStr, "Expected origin to be non-empty")
				// Check that the origin is a valid URL
				url, err := url.ParseRequestURI(originStr)
				assert.NoError(t, err, "Expected origin to be a valid URL")
				require.Contains(t, url.Host, "localhost:", "Expected origin to contain localhost")
			}
		})

	}
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
		"ACCEPT_FILE_DIR":  "./accept_files",
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

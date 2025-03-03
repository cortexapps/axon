package snykbroker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegister_Success(t *testing.T) {

	cfg := &config.AgentConfig{
		CortexApiToken: "test_token",
		InstanceId:     uuid.New().String(),
	}

	server := createMockRegistrationServer(
		t, "test_token", "test_integration", "test_alias",
		func(req registerRequest) int {
			require.Equal(t, cfg.InstanceId, req.InstanceId)
			return http.StatusOK
		},
	)
	defer server.Close()
	cfg.CortexApiBaseUrl = server.URL
	reg := NewRegistration(*cfg)

	resp, err := reg.Register(common.Integration("test_integration"), "test_alias")
	require.NoError(t, err)
	require.Equal(t, "the_broker_token", resp.Token)
	require.Equal(t, "http://example.com", resp.ServerUri)
}

func TestRegister_Unauthorized(t *testing.T) {

	cfg := &config.AgentConfig{
		CortexApiToken: "test_token",
		InstanceId:     uuid.New().String(),
	}

	server := createMockRegistrationServer(
		t, "test_token", "test_integration", "test_alias",
		func(req registerRequest) int {
			require.Equal(t, cfg.InstanceId, req.InstanceId)
			return http.StatusUnauthorized
		},
	)
	defer server.Close()
	cfg.CortexApiBaseUrl = server.URL
	reg := NewRegistration(*cfg)

	resp, err := reg.Register(common.Integration("test_integration"), "test_alias")
	require.Nil(t, resp)
	require.Equal(t, err, ErrUnauthorized)
}

func TestRegister_OtherError(t *testing.T) {

	cfg := &config.AgentConfig{
		CortexApiToken: "test_token",
		InstanceId:     uuid.New().String(),
	}

	server := createMockRegistrationServer(
		t, "test_token", "test_integration", "test_alias",
		func(req registerRequest) int {
			require.Equal(t, cfg.InstanceId, req.InstanceId)
			return http.StatusBadGateway
		},
	)
	defer server.Close()
	cfg.CortexApiBaseUrl = server.URL
	reg := NewRegistration(*cfg)

	resp, err := reg.Register(common.Integration("test_integration"), "test_alias")
	require.Nil(t, resp)
	require.IsType(t, err, &RegistrationError{})
	re := err.(*RegistrationError)
	require.Equal(t, re.StatusCode, http.StatusBadGateway)
}

func TestRegister_ConnectError(t *testing.T) {

	cfg := &config.AgentConfig{
		CortexApiToken: "test_token",
		InstanceId:     uuid.New().String(),
	}

	cfg.CortexApiBaseUrl = "http://123xyz-foobar.com"
	reg := NewRegistration(*cfg)

	resp, err := reg.Register(common.Integration("test_integration"), "test_alias")
	require.Nil(t, resp)
	require.IsType(t, err, &RegistrationError{})
	re := err.(*RegistrationError)
	require.Equal(t, re.StatusCode, 0)
	require.IsType(t, re.error, &url.Error{})
}

func createMockRegistrationServer(
	t *testing.T,
	authToken string,
	integration, alias string,
	callback func(req registerRequest) int,
) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+authToken, r.Header.Get("Authorization"))
		require.Equal(t, "/api/v1/relay/register", r.URL.Path)

		var reqBody registerRequest
		err := json.NewDecoder(r.Body).Decode(&reqBody)
		require.NoError(t, err)
		require.Equal(t, integration, string(reqBody.Integration))
		require.Equal(t, alias, reqBody.Alias)
		status := http.StatusOK
		if callback != nil {
			status = callback(reqBody)
		}

		w.WriteHeader(status)

		if status != http.StatusOK {
			return
		}

		expectedResponse := &RegistrationInfoResponse{
			ServerUri: "http://example.com",
			Token:     "the_broker_token",
		}

		json.NewEncoder(w).Encode(expectedResponse)
	}))
}

package snykbroker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

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
	reg := NewRegistration(*cfg, nil)

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
	reg := NewRegistration(*cfg, nil)

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
	reg := NewRegistration(*cfg, nil)

	resp, err := reg.Register(common.Integration("test_integration"), "test_alias")
	require.Nil(t, resp)
	require.IsType(t, err, &RegistrationError{})
	re := err.(*RegistrationError)
	require.Equal(t, http.StatusBadGateway, re.StatusCode)
}

func TestRegister_ConnectError(t *testing.T) {

	cfg := &config.AgentConfig{
		CortexApiToken: "test_token",
		InstanceId:     uuid.New().String(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Fail(t, "This server should not be called")
	}))
	server.Close()

	cfg.CortexApiBaseUrl = server.URL
	reg := NewRegistration(*cfg, nil)

	resp, err := reg.Register(common.Integration("test_integration"), "test_alias")
	require.Nil(t, resp)
	require.IsType(t, err, &RegistrationError{})
	re := err.(*RegistrationError)
	require.Equal(t, 0, re.StatusCode)
	require.IsType(t, &url.Error{}, re.error)
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

func TestRegister_ReleasesConnection(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// Sending the end of the chunked body late is what lets a
			// decoder return before it reaches EOF.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write([]byte(`{"serverUri":"http://example.com","token":"the_broker_token"}` + "\n"))
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}))
			defer server.Close()

			client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
			reg := NewRegistration(config.AgentConfig{
				CortexApiBaseUrl: server.URL,
				CortexApiToken:   "test_token",
			}, client)

			for i := 0; i < 3; i++ {
				done := make(chan struct{})
				go func() {
					defer close(done)
					reg.Register(common.Integration("test_integration"), "test_alias")
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("register %d is waiting for a connection held by an earlier call", i+1)
				}
			}
		})
	}
}

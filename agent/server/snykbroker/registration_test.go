package snykbroker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

	resp, err := reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")
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

	resp, err := reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")
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

	resp, err := reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")
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

	resp, err := reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	eof    bool
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

// A connection goes back to the transport's pool only when its body is read
// to EOF and closed. Trailing bytes after the JSON value stand in for the end
// of a chunked body, which a decoder does not need to read.
func TestRegister_ReleasesConnection(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(
				`{"serverUri":"http://example.com","token":"the_broker_token"}` + "\n\n",
			)}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: body, Header: http.Header{}, Request: r}, nil
			})}
			reg := NewRegistration(config.AgentConfig{
				CortexApiBaseUrl: "http://cortex.example.com",
				CortexApiToken:   "test_token",
			}, client)

			reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")

			require.True(t, body.eof, "body was not read to EOF")
			require.True(t, body.closed, "body was not closed")
		})
	}
}

// The production failure: a chunked response whose terminator arrives after
// the JSON value. With a one-connection pool, a Register that does not drain
// and close the body holds that connection forever and the next call blocks.
func TestRegister_ReleasesPooledConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"serverUri":"http://example.com","token":"the_broker_token"}`))
		w.(http.Flusher).Flush() // data chunk on the wire; terminator only when the handler returns
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	client := &http.Client{
		Transport: &http.Transport{MaxConnsPerHost: 1},
		Timeout:   2 * time.Second,
	}
	reg := NewRegistration(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, client)

	for i := 0; i < 2; i++ {
		resp, err := reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")
		require.NoError(t, err, "call %d", i+1)
		require.Equal(t, "the_broker_token", resp.Token)
	}
}

// newHangingServer answers nothing until release is called. The handler is
// released explicitly rather than through r.Context(): the server only
// notices a client disconnect once the request body has been read, and
// Register's POST body is never read here, so server.Close() would wait on
// the handler forever.
func newHangingServer() (*httptest.Server, func()) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	var once sync.Once
	return server, func() { once.Do(func() { close(release) }) }
}

// A caller's deadline bounds the whole call: a server that never answers
// cannot hold Register past it.
func TestRegister_HonoursCallerDeadline(t *testing.T) {
	server, release := newHangingServer()
	defer server.Close()
	defer release()
	reg := NewRegistration(config.AgentConfig{CortexApiBaseUrl: server.URL, CortexApiToken: "test_token"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := reg.Register(ctx, common.Integration("test_integration"), "test_alias")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

// A caller with no deadline still cannot hang: Register applies its own
// bound, which is what keeps the periodic re-registration poll from
// blocking forever on a hung Cortex API.
func TestRegister_DefaultTimeoutWhenNoDeadline(t *testing.T) {
	previous := registrationTimeout
	registrationTimeout = 50 * time.Millisecond
	t.Cleanup(func() { registrationTimeout = previous })

	server, release := newHangingServer()
	defer server.Close()
	defer release()
	reg := NewRegistration(config.AgentConfig{CortexApiBaseUrl: server.URL, CortexApiToken: "test_token"}, nil)

	start := time.Now()
	_, err := reg.Register(context.Background(), common.Integration("test_integration"), "test_alias")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

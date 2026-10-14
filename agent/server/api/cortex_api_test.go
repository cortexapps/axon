package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/test/httpfake"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCallGet_DryRun(t *testing.T) {
	server, cleanup := mockServer(t, config.AgentConfig{DryRun: true}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("Should not be called")
	})
	defer cleanup()

	req := &pb.CallRequest{
		Method: "GET",
		Path:   "/test",
	}

	resp, err := server.Call(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestCallPost_DryRun(t *testing.T) {
	server, cleanup := mockServer(t, config.AgentConfig{DryRun: true}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("Should not be called")
	})
	defer cleanup()

	req := &pb.CallRequest{
		Method: "POST",
		Path:   "/test",
		Body:   "{\"key\": \"value\"}",
	}

	resp, err := server.Call(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestCall_Success_GET(t *testing.T) {

	server, cleanup := mockServer(t, config.AgentConfig{
		CortexApiToken: "test_token",
	}, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "/test", r.URL.Path)
		requestBody, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "", string(requestBody))
		w.WriteHeader(http.StatusAccepted)
	})
	defer cleanup()

	req := &pb.CallRequest{
		Method: "GET",
		Path:   "/test",
	}
	resp, err := server.Call(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(http.StatusAccepted), resp.StatusCode)
}

func TestCall_Success_POST(t *testing.T) {

	body := "{\"key\": \"value\"}"

	server, cleanup := mockServer(t, config.AgentConfig{
		CortexApiToken: "test_token",
	}, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "/test", r.URL.Path)
		requestBody, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(requestBody))
		w.WriteHeader(http.StatusAccepted)
	})
	defer cleanup()

	req := &pb.CallRequest{
		Method: "POST",
		Path:   "/test",
		Body:   body,
	}
	resp, err := server.Call(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(http.StatusAccepted), resp.StatusCode)
}

func TestCall_Error(t *testing.T) {

	errorBody := "{\"error\": \"error message\"}"

	server, cleanup := mockServer(t, config.AgentConfig{
		CortexApiToken: "test_token",
	}, func(w http.ResponseWriter, r *http.Request) {

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errorBody))
	})
	defer cleanup()

	req := &pb.CallRequest{
		Method: "GET",
		Path:   "/bad",
	}
	resp, err := server.Call(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(http.StatusBadRequest), resp.StatusCode)
	require.Equal(t, errorBody, resp.Body)
}

func TestCallDefaults(t *testing.T) {

	server, cleanup := mockServer(t, config.AgentConfig{
		CortexApiToken: "test_token",
	}, func(w http.ResponseWriter, r *http.Request) {

		require.Equal(t, "GET", r.Method)
		require.Equal(t, "/the/path", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	req := &pb.CallRequest{

		Path: "//the//path",
	}
	resp, err := server.Call(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(http.StatusNoContent), resp.StatusCode)
}

func mockServer(t *testing.T, cfg config.AgentConfig, handler http.HandlerFunc) (pb.CortexApiServer, func()) {
	logger, _ := zap.NewDevelopment()

	mockServer := httptest.NewServer(http.HandlerFunc(handler))

	if !cfg.DryRun {
		cfg.CortexApiBaseUrl = mockServer.URL
	} else {
		cfg.CortexApiBaseUrl = "http://dryrun" // should never be called
	}

	cfg.HttpServerPort = common.GetRandomPort()
	proxy := NewApiProxyHandler(cfg, logger, nil)
	httpServerParams := cortex_http.HttpServerParams{
		Logger: logger,
	}
	proxyServer := cortex_http.NewHttpServer(httpServerParams, cortex_http.WithName("mock"), cortex_http.WithPort(cfg.HttpServerPort))
	proxyServer.RegisterHandler(proxy)
	port, err := proxyServer.Start()
	require.NoError(t, err)
	require.Equal(t, port, cfg.HttpServerPort)

	cfg.HttpServerPort = port

	server := NewCortexApiServer(logger, cfg)

	return server, func() {
		mockServer.Close()
		proxyServer.Close()
	}
}

func TestCall_RateLimited_StopsAtDeadline(t *testing.T) {
	server, cleanup := mockServer(t, config.AgentConfig{
		CortexApiToken: "test_token",
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := server.Call(ctx, &pb.CallRequest{Method: "GET", Path: "/test"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestHelper_RateLimited_RetrySendsBody(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, `{"key": "value"}`, string(body), attempts)
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", ".01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	helper := &httpRequestHelper{
		BaseURL: server.URL,
		logger:  zap.NewNop(),
		client:  &http.Client{},
	}
	resp, err := helper.Do(context.Background(), "POST", "/test", &RequestBody{
		Body:        []byte(`{"key": "value"}`),
		ContentType: "application/json",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, attempts)
}

func TestHelper_RateLimited_StopsAtDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	helper := &httpRequestHelper{
		BaseURL: server.URL,
		logger:  zap.NewNop(),
		client:  &http.Client{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := helper.Do(ctx, "GET", "/test", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
}

func newFakeServer(responses ...*http.Response) (*cortexApiServer, *httpfake.Transport) {
	client, tr := httpfake.Client(responses...)
	return &cortexApiServer{
		logger: zap.NewNop(),
		helper: &httpRequestHelper{BaseURL: "http://cortex.example.com", logger: zap.NewNop(), client: client},
	}, tr
}

// Every fake body fails reads once the request context is done, so this
// also covers the default deadline staying in force until the body is read.
func TestCall_ReadsDrainsAndClosesResponseBody(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, tr := newFakeServer(httpfake.Response(status, `{"ok":true}`+"\n\n"))

			resp, err := server.Call(context.Background(), &pb.CallRequest{Method: "GET", Path: "/test"})
			require.NoError(t, err)
			require.Equal(t, int32(status), resp.StatusCode)
			require.Equal(t, `{"ok":true}`+"\n\n", resp.Body)
			require.True(t, tr.Bodies[0].EOF, "body was not read to EOF")
			require.True(t, tr.Bodies[0].Closed, "body was not closed")
		})
	}
}

func TestCall_RateLimitedBodyIsDrainedAndClosed(t *testing.T) {
	limited := httpfake.Response(http.StatusTooManyRequests, `{"error":"slow down"}`+"\n\n")
	limited.Header.Set("Retry-After", ".01")
	server, tr := newFakeServer(limited, httpfake.Response(http.StatusOK, `{"ok":true}`))

	resp, err := server.Call(context.Background(), &pb.CallRequest{Method: "GET", Path: "/test"})
	require.NoError(t, err)
	require.Equal(t, int32(http.StatusOK), resp.StatusCode)
	require.Len(t, tr.Bodies, 2)
	require.True(t, tr.Bodies[0].EOF, "429 body was not drained")
	require.True(t, tr.Bodies[0].Closed, "429 body was not closed")
	require.True(t, tr.Bodies[1].EOF)
	require.True(t, tr.Bodies[1].Closed)
}

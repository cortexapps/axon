package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
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

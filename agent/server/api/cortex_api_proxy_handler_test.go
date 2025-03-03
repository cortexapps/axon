package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestServeHTTP_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	req, err := http.NewRequest("GET", "/cortex-api/test", nil)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusAccepted, rr.Code)
}

func TestServeHTTP_Host(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	req, err := http.NewRequest("GET", "/test", nil)
	require.NoError(t, err)
	req.Host = cortexApiRoot

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusAccepted, rr.Code)
}

func TestServeHTTP_Success_POST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		require.Equal(t, "xxxyyyzzz", string(body))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("yep"))
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	req, err := http.NewRequest("POST", "/cortex-api/test", bytes.NewBufferString("xxxyyyzzz"))
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "yep", rr.Body.String())
}

func TestServeHTTP_Success_POST_Gzip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		require.Equal(t, "xxxyyyzzz", string(body))
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadRequest)
		buffer := &bytes.Buffer{}
		gzipped, err := gzip.NewWriterLevel(buffer, gzip.BestSpeed)
		require.NoError(t, err)
		gzipped.Write([]byte("nope"))
		gzipped.Close()
		w.Write(buffer.Bytes())
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	req, err := http.NewRequest("POST", "/cortex-api/test", bytes.NewBufferString("xxxyyyzzz"))
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Equal(t, "nope", rr.Body.String())
}

func TestServeHTTP_RateLimited(t *testing.T) {
	retryCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		require.Equal(t, "xxxyyyzzz", string(body), retryCount)
		if retryCount < 3 {
			w.Header().Set("Retry-After", ".01")
			w.WriteHeader(http.StatusTooManyRequests)
			retryCount++
		} else {
			w.WriteHeader(http.StatusNonAuthoritativeInfo)
		}
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	req, err := http.NewRequest("POST", "/cortex-api/test", bytes.NewBufferString("xxxyyyzzz"))
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	start := time.Now()
	proxy.ServeHTTP(rr, req)
	duration := time.Since(start)

	require.Equal(t, http.StatusNonAuthoritativeInfo, rr.Code)
	require.GreaterOrEqual(t, duration, time.Millisecond*10)
	require.Equal(t, 3, retryCount)
}

func TestServeHTTP_RateLimited_Timeout(t *testing.T) {
	retryCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		require.Equal(t, "xxxyyyzzz", string(body), retryCount)
		if retryCount < 5 {
			w.Header().Set("Retry-After", ".1")
			w.WriteHeader(http.StatusTooManyRequests)
			retryCount++
		} else {
			w.WriteHeader(http.StatusNonAuthoritativeInfo)
		}
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*5)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "/cortex-api/test", bytes.NewBufferString("xxxyyyzzz"))
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusRequestTimeout, rr.Code)
}

func TestServeHTTP_DryRun(t *testing.T) {
	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: "http://example.com",
		CortexApiToken:   "test_token",
		DryRun:           true,
	}, zap.NewNop())

	req, err := http.NewRequest("GET", "/cortex-api/test", nil)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}

func TestServeHTTP_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	proxy := NewApiProxyHandler(config.AgentConfig{
		CortexApiBaseUrl: server.URL,
		CortexApiToken:   "test_token",
	}, zap.NewNop())

	req, err := http.NewRequest("GET", "/cortex-api/test", nil)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	require.Equal(t, http.StatusInternalServerError, rr.Code)
}

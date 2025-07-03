package snykbroker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestHeaderApplicationInProxy tests that headers are properly applied to HTTP requests
// going through the reverse proxy created by getProxyWithHeaders
func TestHeaderApplicationInProxy(t *testing.T) {
	// Set up environment variables
	os.Setenv("TEST_API_KEY", "secret-key-123")
	os.Setenv("TEST_TOKEN", "bearer-token-456")
	defer func() {
		os.Unsetenv("TEST_API_KEY")
		os.Unsetenv("TEST_TOKEN")
	}()

	// Create a backend server that captures headers
	var receivedHeaders http.Header
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "success"}`))
	}))
	defer backendServer.Close()

	// Test headers with environment variables resolved
	headers := map[string]string{
		"x-api-key":     "secret-key-123",          // Pre-resolved environment variable
		"authorization": "Bearer bearer-token-456", // Pre-resolved environment variable
		"x-static":      "static-value",            // Static value
	}

	// Create proxy with headers
	proxyEntry, err := newProxyEntry(backendServer.URL, false, 8080, acceptfile.NewResolverMapFromMap(headers), nil)
	proxyEntry.addResponseHeader("x-response", "response-value")
	require.NoError(t, err)
	require.NotNil(t, proxyEntry)

	// Create a test request
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("user-agent", "test-client")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Send request through the proxy
	proxyEntry.handler.ServeHTTP(rr, req)

	// Verify the request was successful
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify that headers were applied to the backend request
	require.Equal(t, "secret-key-123", receivedHeaders.Get("x-api-key"))
	require.Equal(t, "Bearer bearer-token-456", receivedHeaders.Get("authorization"))
	require.Equal(t, "static-value", receivedHeaders.Get("x-static"))
	require.Equal(t, "", receivedHeaders.Get("x-response"))

	// Verify original headers are preserved
	require.Equal(t, "test-client", receivedHeaders.Get("user-agent"))

	require.Equal(t, "response-value", rr.Header().Get("x-response"))

	// Verify the response body
	assert.JSONEq(t, `{"message": "success"}`, rr.Body.String())
}

// TestMultipleProxiesWithDifferentHeaders tests that different proxy instances
// can have different headers applied
func TestMultipleProxiesWithDifferentHeaders(t *testing.T) {
	// Create two backend servers that capture headers
	var server1Headers, server2Headers http.Header

	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server1Headers = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"server": "1"}`))
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server2Headers = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"server": "2"}`))
	}))
	defer server2.Close()

	// Create first proxy with its headers
	headers1 := map[string]string{
		"x-api-key": "key-for-server-1",
		"x-service": "service-1",
	}
	proxy1, err := newProxyEntry(server1.URL, false, 8080, acceptfile.NewResolverMapFromMap(headers1), nil)
	require.NoError(t, err)

	// Create second proxy with different headers
	headers2 := map[string]string{
		"x-api-key": "key-for-server-2",
		"x-service": "service-2",
	}
	proxy2, err := newProxyEntry(server2.URL, false, 8080, acceptfile.NewResolverMapFromMap(headers2), nil)
	require.NoError(t, err)

	// Send requests through both proxies
	req1 := httptest.NewRequest("GET", "/test", nil)
	rr1 := httptest.NewRecorder()
	proxy1.handler.ServeHTTP(rr1, req1)

	req2 := httptest.NewRequest("GET", "/test", nil)
	rr2 := httptest.NewRecorder()
	proxy2.handler.ServeHTTP(rr2, req2)

	// Verify both requests were successful
	require.Equal(t, http.StatusOK, rr1.Code)
	require.Equal(t, http.StatusOK, rr2.Code)

	// Verify correct headers were sent to each server
	require.Equal(t, "key-for-server-1", server1Headers.Get("x-api-key"))
	require.Equal(t, "service-1", server1Headers.Get("x-service"))

	require.Equal(t, "key-for-server-2", server2Headers.Get("x-api-key"))
	require.Equal(t, "service-2", server2Headers.Get("x-service"))
}

// TestProxyWithNoHeaders tests that proxies work correctly without headers
func TestProxyWithNoHeaders(t *testing.T) {
	// Create a backend server
	var receivedHeaders http.Header
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "no headers"}`))
	}))
	defer backendServer.Close()

	// Create reflector
	logger := zap.NewNop()
	reflector := NewRegistrationReflector(RegistrationReflectorParams{
		Logger: logger,
	})
	reflector.Start()
	defer reflector.Stop()

	// Create proxy without headers
	proxyEntry, err := reflector.getProxy(backendServer.URL, false, nil)
	require.NoError(t, err)
	require.NotNil(t, proxyEntry)

	// Create a test request with some headers
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("user-agent", "test-client")
	req.Header.Set("content-type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Send request through the proxy
	proxyEntry.handler.ServeHTTP(rr, req)

	// Verify the request was successful
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify original headers are preserved (no custom headers added)
	require.Equal(t, "test-client", receivedHeaders.Get("user-agent"))
	require.Equal(t, "application/json", receivedHeaders.Get("content-type"))

	// Verify no custom headers were added
	assert.Empty(t, receivedHeaders.Get("x-api-key"))
	assert.Empty(t, receivedHeaders.Get("authorization"))

	// Verify the response body
	assert.JSONEq(t, `{"message": "no headers"}`, rr.Body.String())
}

// TestHeaderOverwriting tests that custom headers can overwrite existing headers
func TestHeaderOverwriting(t *testing.T) {
	// Create a backend server
	var receivedHeaders http.Header
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "overwritten"}`))
	}))
	defer backendServer.Close()

	// Create reflector
	logger := zap.NewNop()
	reflector := NewRegistrationReflector(RegistrationReflectorParams{
		Logger: logger,
	})
	reflector.Start()
	defer reflector.Stop()

	// Headers that will overwrite the request headers
	headers := map[string]string{
		"user-agent":    "proxy-agent", // This should overwrite the original user-agent
		"authorization": "Bearer custom-token",
	}

	// Create proxy with headers
	proxyEntry, err := reflector.getProxy(backendServer.URL, false, acceptfile.NewResolverMapFromMap(headers))
	require.NoError(t, err)

	// Create request with original headers
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("user-agent", "original-agent")
	req.Header.Set("content-type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Send request through the proxy
	proxyEntry.handler.ServeHTTP(rr, req)

	// Verify the request was successful
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify that custom headers overwrote original headers
	require.Equal(t, "proxy-agent", receivedHeaders.Get("user-agent")) // Should be overwritten
	require.Equal(t, "Bearer custom-token", receivedHeaders.Get("authorization"))

	// Verify that non-conflicting headers are preserved
	require.Equal(t, "application/json", receivedHeaders.Get("content-type"))
}

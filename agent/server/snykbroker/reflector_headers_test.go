package snykbroker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Integration tests for accept.json header functionality with live HTTP calls

func TestAcceptFileHeadersAppliedToLiveRequests(t *testing.T) {

	common.IgnoreHosts = []string{}

	// Set up environment variables for testing
	os.Setenv("TEST_API_KEY", "secret-api-key-123")
	os.Setenv("TEST_AUTH_TOKEN", "bearer-token-456")
	defer func() {
		os.Unsetenv("TEST_API_KEY")
		os.Unsetenv("TEST_AUTH_TOKEN")
	}()

	tests := []struct {
		name            string
		acceptContent   map[string]any
		expectedHeaders map[string]string
		testPath        string
		index           int
	}{
		{
			name: "single header with env var",
			acceptContent: map[string]any{
				"private": []any{
					map[string]any{
						"method": "GET",
						"path":   "/api/*",
						"origin": "http://example.com",
						"headers": map[string]any{
							"x-api-key": "${TEST_API_KEY}",
						},
					},
				},
			},
			expectedHeaders: map[string]string{
				"x-api-key": "secret-api-key-123",
			},
			testPath: "/api/test",
		},
		{
			name: "multiple headers with env vars",
			acceptContent: map[string]any{
				"private": []any{
					map[string]any{
						"method": "POST",
						"path":   "/graphql",
						"origin": "http://graphql.example.com",
						"headers": map[string]any{
							"authorization": "Bearer ${TEST_AUTH_TOKEN}",
							"x-api-key":     "${TEST_API_KEY}",
							"x-static":      "static-value",
						},
					},
				},
			},
			expectedHeaders: map[string]string{
				"authorization": "Bearer bearer-token-456",
				"x-api-key":     "secret-api-key-123",
				"x-static":      "static-value",
			},
			testPath: "/graphql",
		},
		{
			name: "headers with wildcard path",
			acceptContent: map[string]any{
				"private": []any{
					map[string]any{
						"method": "any",
						"path":   "/*",
						"origin": "http://api.example.com",
						"headers": map[string]any{
							"x-service-key": "${TEST_API_KEY}",
						},
					},
				},
			},
			expectedHeaders: map[string]string{
				"x-service-key": "secret-api-key-123",
			},
			testPath: "/any/nested/path",
		},
		{
			name: "multiple routes different headers",
			acceptContent: map[string]any{
				"private": []any{
					map[string]any{
						"method": "GET",
						"path":   "/api/*",
						"origin": "http://example.com",
						"headers": map[string]any{
							"x-api-key": "route1",
						},
					},
					map[string]any{
						"method": "GET",
						"path":   "/api-v2/*",
						"origin": "http://example.com",
						"headers": map[string]any{
							"x-api-key": "route2",
						},
					},
				},
			},
			expectedHeaders: map[string]string{
				"x-api-key": "route2",
			},
			index:    1,
			testPath: "/api-v2/test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock backend server that captures headers
			var receivedHeaders http.Header
			backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedHeaders = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status": "ok"}`))
			}))
			defer backendServer.Close()

			// Update the accept content to use the real backend server URL
			updateOriginInAcceptContent(tt.acceptContent, backendServer.URL)

			// Create temporary accept file
			acceptFile := createTempAcceptFile(t, tt.acceptContent)
			defer os.Remove(acceptFile)

			// Create integration info and process the accept file
			integrationInfo := common.IntegrationInfo{
				Integration:    common.IntegrationCustom,
				AcceptFilePath: acceptFile,
			}

			// Create reflector
			logger := zap.NewNop()
			reflector := NewRegistrationReflector(RegistrationReflectorParams{
				Logger: logger,
				Config: config.AgentConfig{
					HttpRelayReflectorMode: config.RelayReflectorAllTraffic,
				},
			})

			// Start the reflector server
			_, err := reflector.Start()
			require.NoError(t, err)
			defer reflector.Stop()

			// Process the accept file with header extraction
			var capturedOrigin string
			var capturedHeaders map[string]string

			info, err := integrationInfo.RewriteOrigins(
				acceptFile,
				func(originalURI string, headers common.ResolverMap) string {
					capturedOrigin = originalURI
					capturedHeaders = headers.ToStringMap()
					return reflector.ProxyURI(originalURI, WithHeaders(headers.ToStringMap()))
				},
			)
			require.NoError(t, err)
			require.NotNil(t, info)

			// Verify headers were captured correctly
			assert.Equal(t, backendServer.URL, capturedOrigin)
			assert.Equal(t, tt.expectedHeaders, capturedHeaders)

			// Make a live HTTP request through the proxy
			proxyURL := fmt.Sprintf("%s%s", info.Routes[tt.index].ProxyOrigin, tt.testPath)
			resp, err := http.Get(proxyURL)
			require.NoError(t, err)
			defer resp.Body.Close()

			// Verify the request was successful
			assert.Equal(t, http.StatusOK, resp.StatusCode)

			// Verify that all expected headers were received by the backend
			for expectedKey, expectedValue := range tt.expectedHeaders {
				actualValue := receivedHeaders.Get(expectedKey)
				assert.Equal(t, expectedValue, actualValue,
					"Header %s: expected %s, got %s", expectedKey, expectedValue, actualValue)
			}

			// Read response to ensure proxy worked correctly
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.JSONEq(t, `{"status": "ok"}`, string(body))
		})
	}
}

func TestAcceptFileHeadersWithMissingEnvVars(t *testing.T) {
	// Ensure env var is not set
	os.Unsetenv("MISSING_ENV_VAR")

	acceptContent := map[string]any{
		"private": []any{
			map[string]any{
				"method": "GET",
				"path":   "/api/*",
				"origin": "http://example.com",
				"headers": map[string]any{
					"x-api-key": "${MISSING_ENV_VAR}",
				},
			},
		},
	}

	acceptFile := createTempAcceptFile(t, acceptContent)
	defer os.Remove(acceptFile)

	integrationInfo := common.IntegrationInfo{
		Integration:    common.IntegrationCustom,
		AcceptFilePath: acceptFile,
	}

	logger := zap.NewNop()
	reflector := NewRegistrationReflector(RegistrationReflectorParams{
		Logger: logger,
		Config: config.AgentConfig{
			HttpRelayReflectorMode: config.RelayReflectorAllTraffic,
		},
	})

	var capturedHeaders map[string]string

	_, err := integrationInfo.RewriteOrigins(
		acceptFile,
		func(originalURI string, headers common.ResolverMap) string {
			capturedHeaders = headers.ToStringMap()
			return reflector.ProxyURI(originalURI, WithHeadersResolver(headers))
		},
	)
	require.NoError(t, err)

	// Verify that missing env vars result in empty string (os.ExpandEnv behavior)
	assert.Equal(t, "", capturedHeaders["x-api-key"])
}

func TestMultipleRoutesWithDifferentHeaders(t *testing.T) {
	os.Setenv("API_KEY_1", "key-one")
	os.Setenv("API_KEY_2", "key-two")
	defer func() {
		os.Unsetenv("API_KEY_1")
		os.Unsetenv("API_KEY_2")
	}()

	// Create two mock backend servers
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

	acceptContent := map[string]any{
		"private": []any{
			map[string]any{
				"method": "GET",
				"path":   "/api1/*",
				"origin": server1.URL,
				"headers": map[string]any{
					"x-api-key": "${API_KEY_1}",
					"x-service": "service-1",
				},
			},
			map[string]any{
				"method": "GET",
				"path":   "/api2/*",
				"origin": server2.URL,
				"headers": map[string]any{
					"x-api-key": "${API_KEY_2}",
					"x-service": "service-2",
				},
			},
		},
	}

	acceptFile := createTempAcceptFile(t, acceptContent)
	defer os.Remove(acceptFile)

	integrationInfo := common.IntegrationInfo{
		Integration:    common.IntegrationCustom,
		AcceptFilePath: acceptFile,
	}

	logger := zap.NewNop()
	reflector := NewRegistrationReflector(RegistrationReflectorParams{
		Logger: logger,
		Config: config.AgentConfig{
			HttpRelayReflectorMode: config.RelayReflectorAllTraffic,
		},
	})

	_, err := reflector.Start()
	require.NoError(t, err)
	defer reflector.Stop()

	// Process the accept file
	headerExtractionCount := 0
	info, err := integrationInfo.RewriteOrigins(
		acceptFile,
		func(originalURI string, headers common.ResolverMap) string {
			headerExtractionCount++
			return reflector.ProxyURI(originalURI, WithHeadersResolver(headers))
		},
	)
	require.NotNil(t, info)
	require.NoError(t, err)

	// Verify that headers were extracted for both routes
	assert.Equal(t, 2, headerExtractionCount)

	// Make requests to both routes
	resp1, err := http.Get(fmt.Sprintf("%s/api1/test", info.Routes[0].ProxyOrigin))
	require.NoError(t, err)
	defer resp1.Body.Close()

	resp2, err := http.Get(fmt.Sprintf("%s/api2/test", info.Routes[1].ProxyOrigin))
	require.NoError(t, err)
	defer resp2.Body.Close()

	// Verify both requests were successful
	assert.Equal(t, http.StatusOK, resp1.StatusCode)
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// Give servers time to process requests
	time.Sleep(100 * time.Millisecond)

	// Verify correct headers were sent to each server
	assert.Equal(t, "key-one", server1Headers.Get("x-api-key"))
	assert.Equal(t, "service-1", server1Headers.Get("x-service"))

	assert.Equal(t, "key-two", server2Headers.Get("x-api-key"))
	assert.Equal(t, "service-2", server2Headers.Get("x-service"))
}

// Helper functions

func createTempAcceptFile(t *testing.T, content map[string]any) string {
	jsonContent, err := json.MarshalIndent(content, "", "  ")
	require.NoError(t, err)

	tmpFile, err := os.CreateTemp("", "accept-*.json")
	require.NoError(t, err)

	_, err = tmpFile.Write(jsonContent)
	require.NoError(t, err)

	err = tmpFile.Close()
	require.NoError(t, err)

	return tmpFile.Name()
}

func updateOriginInAcceptContent(content map[string]any, newOrigin string) {
	if private, ok := content["private"].([]any); ok {
		for _, entry := range private {
			if entryMap, ok := entry.(map[string]any); ok {
				entryMap["origin"] = newOrigin
			}
		}
	}
}

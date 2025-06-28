package common

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteOriginsWithHeaderExtraction(t *testing.T) {
	// Set up environment variables for testing
	os.Setenv("TEST_API_KEY", "secret-key-123")
	os.Setenv("TEST_TOKEN", "bearer-token-456")
	defer func() {
		os.Unsetenv("TEST_API_KEY")
		os.Unsetenv("TEST_TOKEN")
	}()

	tests := []struct {
		name                string
		acceptContent       map[string]interface{}
		expectedHeaderCalls []headerCall
	}{
		{
			name: "single route with headers",
			acceptContent: map[string]interface{}{
				"private": []interface{}{
					map[string]interface{}{
						"method": "GET",
						"path":   "/api/*",
						"origin": "https://api.example.com",
						"headers": map[string]any{
							"x-api-key": "${TEST_API_KEY}",
							"x-static":  "static-value",
						},
					},
				},
			},
			expectedHeaderCalls: []headerCall{
				{
					origin: "https://api.example.com",
					headers: map[string]string{
						"x-api-key": "secret-key-123",
						"x-static":  "static-value",
					},
				},
			},
		},
		{
			name: "multiple routes with different headers",
			acceptContent: map[string]interface{}{
				"private": []interface{}{
					map[string]interface{}{
						"method": "GET",
						"path":   "/api1/*",
						"origin": "https://api1.example.com",
						"headers": map[string]interface{}{
							"authorization": "Bearer ${TEST_TOKEN}",
						},
					},
					map[string]interface{}{
						"method": "POST",
						"path":   "/api2/*",
						"origin": "https://api2.example.com",
						"headers": map[string]interface{}{
							"x-api-key": "${TEST_API_KEY}",
							"x-service": "test-service",
						},
					},
				},
			},
			expectedHeaderCalls: []headerCall{
				{
					origin: "https://api1.example.com",
					headers: map[string]string{
						"authorization": "Bearer bearer-token-456",
					},
				},
				{
					origin: "https://api2.example.com",
					headers: map[string]string{
						"x-api-key": "secret-key-123",
						"x-service": "test-service",
					},
				},
			},
		},
		{
			name: "route without headers",
			acceptContent: map[string]interface{}{
				"private": []interface{}{
					map[string]interface{}{
						"method": "GET",
						"path":   "/api/*",
						"origin": "https://api.example.com",
					},
				},
			},
			expectedHeaderCalls: []headerCall{}, // No headers, so no calls expected
		},
		{
			name: "mixed routes with and without headers",
			acceptContent: map[string]interface{}{
				"private": []interface{}{
					map[string]interface{}{
						"method": "GET",
						"path":   "/api1/*",
						"origin": "https://api1.example.com",
						"headers": map[string]interface{}{
							"x-api-key": "${TEST_API_KEY}",
						},
					},
					map[string]interface{}{
						"method": "GET",
						"path":   "/api2/*",
						"origin": "https://api2.example.com",
						// No headers section
					},
				},
			},
			expectedHeaderCalls: []headerCall{
				{
					origin: "https://api1.example.com",
					headers: map[string]string{
						"x-api-key": "secret-key-123",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary accept file
			acceptFile := createTempAcceptFile(t, tt.acceptContent)
			defer os.Remove(acceptFile)

			// Create integration info
			integrationInfo := IntegrationInfo{
				Integration:    IntegrationCustom,
				AcceptFilePath: acceptFile,
			}

			// Capture header extraction calls
			var headerCalls []headerCall
			headerExtractor := func(origin string, headers ResolverMap) string {
				if len(headers) > 0 {
					headerCalls = append(headerCalls, headerCall{
						origin:  origin,
						headers: headers.ToStringMap(),
					})
				}
				return "proxy-" + origin // Mocking the proxy URI generation
			}

			// Call RewriteOriginsWithHeaderExtraction
			newFile, err := integrationInfo.RewriteOrigins(
				acceptFile,
				headerExtractor,
			)
			require.NoError(t, err)
			require.NotEmpty(t, newFile)

			// Verify header extraction calls
			assert.Equal(t, len(tt.expectedHeaderCalls), len(headerCalls))
			for i, expected := range tt.expectedHeaderCalls {
				if i < len(headerCalls) {
					assert.Equal(t, expected.origin, headerCalls[i].origin)
					assert.Equal(t, expected.headers, headerCalls[i].headers)
				}
			}
		})
	}
}

func TestHeaderEnvironmentVariableResolution(t *testing.T) {
	// Test missing environment variable
	os.Unsetenv("MISSING_VAR")
	os.Setenv("EXISTING_VAR", "existing-value")
	defer os.Unsetenv("EXISTING_VAR")

	acceptContent := map[string]interface{}{
		"private": []interface{}{
			map[string]interface{}{
				"method": "GET",
				"path":   "/api/*",
				"origin": "https://api.example.com",
				"headers": map[string]interface{}{
					"x-existing": "${EXISTING_VAR}",
					"x-missing":  "${MISSING_VAR}",
					"x-static":   "no-vars-here",
				},
			},
		},
	}

	acceptFile := createTempAcceptFile(t, acceptContent)
	defer os.Remove(acceptFile)

	integrationInfo := IntegrationInfo{
		Integration:    IntegrationCustom,
		AcceptFilePath: acceptFile,
	}

	var capturedHeaders map[string]string
	headerExtractor := func(_ string, headers ResolverMap) {
		capturedHeaders = headers.ToStringMap()
	}

	_, err := integrationInfo.RewriteOrigins(
		acceptFile,
		func(originalURI string, headers ResolverMap) string {
			headerExtractor(originalURI, headers)
			return originalURI
		},
	)
	require.NoError(t, err)

	// Verify environment variable resolution
	assert.Equal(t, "existing-value", capturedHeaders["x-existing"])
	assert.Equal(t, "", capturedHeaders["x-missing"]) // os.ExpandEnv returns empty string for missing vars
	assert.Equal(t, "no-vars-here", capturedHeaders["x-static"])
}

func TestComplexEnvironmentVariablePatterns(t *testing.T) {
	os.Setenv("PREFIX", "api")
	os.Setenv("VERSION", "v1")
	os.Setenv("SECRET", "my-secret-123")
	defer func() {
		os.Unsetenv("PREFIX")
		os.Unsetenv("VERSION")
		os.Unsetenv("SECRET")
	}()

	acceptContent := map[string]interface{}{
		"private": []interface{}{
			map[string]interface{}{
				"method": "GET",
				"path":   "/api/*",
				"origin": "https://api.example.com",
				"headers": map[string]interface{}{
					"x-service-name": "${PREFIX}-service-${VERSION}",
					"authorization":  "Bearer ${SECRET}",
					"x-mixed":        "prefix-${VERSION}-suffix",
				},
			},
		},
	}

	acceptFile := createTempAcceptFile(t, acceptContent)
	defer os.Remove(acceptFile)

	integrationInfo := IntegrationInfo{
		Integration:    IntegrationCustom,
		AcceptFilePath: acceptFile,
	}

	var capturedHeaders map[string]string
	headerExtractor := func(origin string, headers ResolverMap) string {
		capturedHeaders = headers.ToStringMap()
		return origin
	}

	_, err := integrationInfo.RewriteOrigins(
		acceptFile,
		headerExtractor,
	)
	require.NoError(t, err)

	// Verify complex environment variable patterns
	assert.Equal(t, "api-service-v1", capturedHeaders["x-service-name"])
	assert.Equal(t, "Bearer my-secret-123", capturedHeaders["authorization"])
	assert.Equal(t, "prefix-v1-suffix", capturedHeaders["x-mixed"])
}

func TestEmptyAndInvalidHeaderValues(t *testing.T) {
	acceptContent := map[string]interface{}{
		"private": []interface{}{
			map[string]interface{}{
				"method": "GET",
				"path":   "/api/*",
				"origin": "https://api.example.com",
				"headers": map[string]interface{}{
					"x-empty":   "",
					"x-number":  123,     // Non-string value
					"x-boolean": true,    // Non-string value
					"x-string":  "valid", // Valid string value
				},
			},
		},
	}

	acceptFile := createTempAcceptFile(t, acceptContent)
	defer os.Remove(acceptFile)

	integrationInfo := IntegrationInfo{
		Integration:    IntegrationCustom,
		AcceptFilePath: acceptFile,
	}

	var capturedHeaders map[string]string
	headerExtractor := func(origin string, headers ResolverMap) string {
		capturedHeaders = headers.ToStringMap()
		return origin
	}

	_, err := integrationInfo.RewriteOrigins(
		acceptFile,
		headerExtractor,
	)
	require.NoError(t, err)

	// Verify that only string values are processed
	assert.Equal(t, "", capturedHeaders["x-empty"])
	assert.Equal(t, "valid", capturedHeaders["x-string"])

	// Non-string values should not be included
	_, hasNumber := capturedHeaders["x-number"]
	_, hasBoolean := capturedHeaders["x-boolean"]
	assert.False(t, hasNumber)
	assert.False(t, hasBoolean)
}

// Helper types and functions

type headerCall struct {
	origin  string
	headers map[string]string
}

func createTempAcceptFile(t *testing.T, content map[string]interface{}) string {
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

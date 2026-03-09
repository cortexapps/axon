package requestexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func makeRules(t *testing.T, rules string, cfg config.AgentConfig) []acceptfile.AcceptFileRuleWrapper {
	t.Helper()
	af, err := acceptfile.NewAcceptFile([]byte(rules), cfg, zap.NewNop())
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rendered, &parsed))

	// Re-parse to get wrappers via a new accept file.
	af2, err := acceptfile.NewAcceptFile(rendered, cfg, zap.NewNop())
	require.NoError(t, err)
	rendered2, err := af2.Render(zap.NewNop())
	require.NoError(t, err)

	af3, err := acceptfile.NewAcceptFile(rendered2, cfg, zap.NewNop())
	require.NoError(t, err)
	_ = af3

	// Get private rules from the wrapper.
	af4, err := acceptfile.NewAcceptFile(rendered, cfg, zap.NewNop())
	require.NoError(t, err)
	wrapper := af4.Wrapper()
	return wrapper.PrivateRules()
}

func TestMatchRule_MethodAndPath(t *testing.T) {
	tests := []struct {
		name        string
		ruleMethod  string
		rulePath    string
		reqMethod   string
		reqPath     string
		shouldMatch bool
	}{
		{"exact GET match", "GET", "/api/v1/repos", "GET", "/api/v1/repos", true},
		{"method mismatch", "POST", "/api/v1/repos", "GET", "/api/v1/repos", false},
		{"any method match", "any", "/api/v1/repos", "DELETE", "/api/v1/repos", true},
		{"wildcard path", "GET", "/api/*", "GET", "/api/repos", true},
		{"wildcard path no match", "GET", "/api/*", "GET", "/other/repos", false},
		{"path mismatch", "GET", "/api/v1/repos", "GET", "/api/v2/repos", false},
		{"case insensitive method", "get", "/api/v1/repos", "GET", "/api/v1/repos", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched := matchesMethod(tt.ruleMethod, tt.reqMethod) && matchesPath(tt.rulePath, tt.reqPath)
			assert.Equal(t, tt.shouldMatch, matched)
		})
	}
}

func TestMatchRule_WildcardSubpath(t *testing.T) {
	assert.True(t, matchesPath("/api/*", "/api/repos"))
	assert.True(t, matchesPath("/api/*", "/api/anything"))
	assert.True(t, matchesPath("/__axon/*", "/__axon/health"))
	assert.False(t, matchesPath("/api/*", "/other/repos"))
}

func TestExecutor_BasicRequest(t *testing.T) {
	// Set up a test HTTP server.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/v1/repos", r.URL.Path)
		w.Header().Set("X-Test", "response-header")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"repos": []}`))
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "GET",
				"path": "/api/v1/repos",
				"origin": "%s"
			}
		]
	}`, server.URL)

	cfg := config.AgentConfig{
		HttpServerPort: 8080,
		PluginDirs:     []string{},
	}

	rules := makeRules(t, rulesJSON, cfg)
	// Filter out the axon route added by render.
	var filteredRules []acceptfile.AcceptFileRuleWrapper
	for _, r := range rules {
		if r.Path() != "/__axon/*" {
			filteredRules = append(filteredRules, r)
		}
	}

	executor := NewRequestExecutor(filteredRules, &http.Client{}, zap.NewNop())

	resp, err := executor.Execute(context.Background(), "GET", "/api/v1/repos", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"repos": []}`, string(resp.Body))
	assert.Equal(t, "response-header", resp.Headers["X-Test"])
}

func TestExecutor_NoMatchingRule(t *testing.T) {
	rulesJSON := `{
		"private": [
			{
				"method": "GET",
				"path": "/api/v1/repos",
				"origin": "https://example.com"
			}
		]
	}`

	cfg := config.AgentConfig{
		HttpServerPort: 8080,
		PluginDirs:     []string{},
	}

	rules := makeRules(t, rulesJSON, cfg)
	executor := NewRequestExecutor(rules, &http.Client{}, zap.NewNop())

	_, err := executor.Execute(context.Background(), "GET", "/unknown/path", nil, nil)
	assert.ErrorIs(t, err, ErrNoMatchingRule)
}

func TestExecutor_BearerAuth(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret-token-123")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret-token-123", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "GET",
				"path": "/api/*",
				"origin": "%s",
				"auth": {
					"scheme": "bearer",
					"token": "${MY_TOKEN}"
				}
			}
		]
	}`, server.URL)

	cfg := config.AgentConfig{
		HttpServerPort: 8080,
		PluginDirs:     []string{},
	}

	rules := makeRules(t, rulesJSON, cfg)
	var filteredRules []acceptfile.AcceptFileRuleWrapper
	for _, r := range rules {
		if r.Path() != "/__axon/*" {
			filteredRules = append(filteredRules, r)
		}
	}

	executor := NewRequestExecutor(filteredRules, &http.Client{}, zap.NewNop())

	resp, err := executor.Execute(context.Background(), "GET", "/api/repos", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestExecutor_BasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "myuser", user)
		assert.Equal(t, "mypass", pass)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "POST",
				"path": "/api/*",
				"origin": "%s",
				"auth": {
					"scheme": "basic",
					"username": "myuser",
					"password": "mypass"
				}
			}
		]
	}`, server.URL)

	cfg := config.AgentConfig{
		HttpServerPort: 8080,
		PluginDirs:     []string{},
	}

	rules := makeRules(t, rulesJSON, cfg)
	var filteredRules []acceptfile.AcceptFileRuleWrapper
	for _, r := range rules {
		if r.Path() != "/__axon/*" {
			filteredRules = append(filteredRules, r)
		}
	}

	executor := NewRequestExecutor(filteredRules, &http.Client{}, zap.NewNop())

	resp, err := executor.Execute(context.Background(), "POST", "/api/data", nil, []byte(`{"key":"value"}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestPool_RoundRobin(t *testing.T) {
	t.Setenv("TEST_API_POOL", "https://api1.example.com,https://api2.example.com,https://api3.example.com")

	pm := NewPoolManager()

	results := make([]string, 6)
	for i := 0; i < 6; i++ {
		results[i] = pm.ResolvePoolVars("${TEST_API}")
	}

	assert.Equal(t, "https://api1.example.com", results[0])
	assert.Equal(t, "https://api2.example.com", results[1])
	assert.Equal(t, "https://api3.example.com", results[2])
	assert.Equal(t, "https://api1.example.com", results[3])
	assert.Equal(t, "https://api2.example.com", results[4])
	assert.Equal(t, "https://api3.example.com", results[5])
}

func TestPool_FallbackToEnvVar(t *testing.T) {
	t.Setenv("SINGLE_API", "https://api.example.com")

	pm := NewPoolManager()
	result := pm.ResolvePoolVars("${SINGLE_API}")
	assert.Equal(t, "https://api.example.com", result)
}

func TestPool_NoMatch(t *testing.T) {
	pm := NewPoolManager()
	result := pm.ResolvePoolVars("https://static.example.com")
	assert.Equal(t, "https://static.example.com", result)
}

func TestBuildTargetURL(t *testing.T) {
	tests := []struct {
		origin  string
		path    string
		want    string
		wantErr bool
	}{
		{"https://api.github.com", "/repos/foo", "https://api.github.com/repos/foo", false},
		{"https://api.github.com/v3", "/repos/foo", "https://api.github.com/v3/repos/foo", false},
		{"https://api.github.com/", "/repos/foo", "https://api.github.com/repos/foo", false},
	}

	for _, tt := range tests {
		t.Run(tt.origin+tt.path, func(t *testing.T) {
			got, err := buildTargetURL(tt.origin, tt.path)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

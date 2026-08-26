package acceptfile

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func makeMatcherRules(t *testing.T, rules string) []AcceptFileRuleWrapper {
	t.Helper()
	cfg := config.AgentConfig{
		HttpServerPort: 8080,
		PluginDirs:     []string{},
	}
	af, err := NewAcceptFile([]byte(rules), cfg, zap.NewNop())
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	af2, err := NewAcceptFile(rendered, cfg, zap.NewNop())
	require.NoError(t, err)

	// Filter out the axon route added by render.
	var filtered []AcceptFileRuleWrapper
	for _, r := range af2.Wrapper().PrivateRules() {
		if r.Path() != "/__axon/*" {
			filtered = append(filtered, r)
		}
	}
	return filtered
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

func TestMatchRule_ValidHeaderRequirement(t *testing.T) {
	tests := []struct {
		name         string
		requirements []ValidHeaderRequirement
		headers      map[string]string
		shouldMatch  bool
	}{
		{
			name:         "no requirements - always matches",
			requirements: nil,
			headers:      nil,
			shouldMatch:  true,
		},
		{
			name: "header present with matching value",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder"}},
			},
			headers:     map[string]string{"x-cortex-service": "scaffolder"},
			shouldMatch: true,
		},
		{
			name: "header present but wrong value",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder"}},
			},
			headers:     map[string]string{"x-cortex-service": "other"},
			shouldMatch: false,
		},
		{
			name: "header missing",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder"}},
			},
			headers:     map[string]string{"x-other": "value"},
			shouldMatch: false,
		},
		{
			name: "header missing - nil headers",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder"}},
			},
			headers:     nil,
			shouldMatch: false,
		},
		{
			name: "case insensitive header name",
			requirements: []ValidHeaderRequirement{
				{Header: "X-Cortex-Service", Values: []string{"scaffolder"}},
			},
			headers:     map[string]string{"x-cortex-service": "scaffolder"},
			shouldMatch: true,
		},
		{
			name: "case insensitive header value",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"Scaffolder"}},
			},
			headers:     map[string]string{"x-cortex-service": "scaffolder"},
			shouldMatch: true,
		},
		{
			name: "multiple allowed values - first matches",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder", "catalog", "other"}},
			},
			headers:     map[string]string{"x-cortex-service": "scaffolder"},
			shouldMatch: true,
		},
		{
			name: "multiple allowed values - second matches",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder", "catalog", "other"}},
			},
			headers:     map[string]string{"x-cortex-service": "catalog"},
			shouldMatch: true,
		},
		{
			name: "multiple allowed values - none match",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder", "catalog"}},
			},
			headers:     map[string]string{"x-cortex-service": "unknown"},
			shouldMatch: false,
		},
		{
			name: "multiple requirements - all must match",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder"}},
				{Header: "x-cortex-tenant", Values: []string{"acme"}},
			},
			headers: map[string]string{
				"x-cortex-service": "scaffolder",
				"x-cortex-tenant":  "acme",
			},
			shouldMatch: true,
		},
		{
			name: "multiple requirements - one missing",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{"scaffolder"}},
				{Header: "x-cortex-tenant", Values: []string{"acme"}},
			},
			headers:     map[string]string{"x-cortex-service": "scaffolder"},
			shouldMatch: false,
		},
		{
			name: "empty values array - just check header exists",
			requirements: []ValidHeaderRequirement{
				{Header: "x-cortex-service", Values: []string{}},
			},
			headers:     map[string]string{"x-cortex-service": "anything"},
			shouldMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchesValid(tt.requirements, tt.headers)
			assert.Equal(t, tt.shouldMatch, result)
		})
	}
}

func TestMatchRule_WithValidHeaders(t *testing.T) {
	// Test that MatchRule correctly uses valid header requirements to select the right rule.
	rulesJSON := `{
		"private": [
			{
				"method": "any",
				"path": "/*",
				"origin": "https://github.com",
				"valid": [
					{
						"header": "x-cortex-service",
						"values": ["scaffolder"]
					}
				]
			},
			{
				"method": "any",
				"path": "/*",
				"origin": "https://api.github.com"
			}
		]
	}`

	rules := makeMatcherRules(t, rulesJSON)

	t.Run("with scaffolder header - matches first rule", func(t *testing.T) {
		headers := map[string]string{"x-cortex-service": "scaffolder"}
		rule := MatchRule(rules, "GET", "/repos/foo", headers)
		require.NotNil(t, rule)
		assert.Equal(t, "https://github.com", rule.Origin())
	})

	t.Run("without scaffolder header - skips first rule, matches second", func(t *testing.T) {
		headers := map[string]string{"x-other": "value"}
		rule := MatchRule(rules, "GET", "/repos/foo", headers)
		require.NotNil(t, rule)
		assert.Equal(t, "https://api.github.com", rule.Origin())
	})

	t.Run("no headers - skips first rule, matches second", func(t *testing.T) {
		rule := MatchRule(rules, "GET", "/repos/foo")
		require.NotNil(t, rule)
		assert.Equal(t, "https://api.github.com", rule.Origin())
	})
}

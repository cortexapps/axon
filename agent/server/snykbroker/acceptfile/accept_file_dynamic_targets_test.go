package acceptfile

import (
	"encoding/json"
	"testing"

	axonConfig "github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func privateRulesOf(t *testing.T, content string) []acceptFileRuleWrapper {
	t.Helper()
	cfg := axonConfig.NewAgentEnvConfig()
	af, err := NewAcceptFile([]byte(content), cfg, nil)
	require.NoError(t, err)
	return newAcceptFileWrapper(af.content, af).PrivateRules()
}

func TestDynamicTargetHostsParsed(t *testing.T) {
	rules := privateRulesOf(t, `{"private": [
		{"method": "any", "origin": "https://a.googleapis.com", "path": "/*",
		 "dynamicTargetHosts": ["*.googleapis.com", "oauth2.googleapis.com"]}
	]}`)
	require.Len(t, rules, 1)
	require.Equal(t, []string{"*.googleapis.com", "oauth2.googleapis.com"}, rules[0].DynamicTargetHosts())
}

func TestDynamicTargetHostsAbsent(t *testing.T) {
	rules := privateRulesOf(t, `{"private": [
		{"method": "any", "origin": "https://api.example.com", "path": "/*"}
	]}`)
	require.Len(t, rules, 1)
	require.Nil(t, rules[0].DynamicTargetHosts())
}

// A typo here would otherwise disable retargeting silently, which surfaces as
// an unexplained 403 on every request rather than as a config error.
func TestDynamicTargetHostsMalformedPanics(t *testing.T) {
	cases := map[string]string{
		"not a list":       `{"private": [{"method": "any", "origin": "https://a.com", "path": "/*", "dynamicTargetHosts": "*.googleapis.com"}]}`,
		"non-string entry": `{"private": [{"method": "any", "origin": "https://a.com", "path": "/*", "dynamicTargetHosts": [42]}]}`,
		"empty entry":      `{"private": [{"method": "any", "origin": "https://a.com", "path": "/*", "dynamicTargetHosts": [""]}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := axonConfig.NewAgentEnvConfig()
			af, err := NewAcceptFile([]byte(content), cfg, zap.NewNop())
			require.NoError(t, err)
			rules := newAcceptFileWrapper(af.content, af).PrivateRules()
			require.Len(t, rules, 1)
			require.Panics(t, func() { rules[0].DynamicTargetHosts() })
		})
	}
}

// The broker has no knowledge of this key, so it must survive rendering
// untouched - the same contract "headers" already relies on.
func TestDynamicTargetHostsRoundTripsToBroker(t *testing.T) {
	cfg := axonConfig.NewAgentEnvConfig()
	af, err := NewAcceptFile([]byte(`{"private": [
		{"method": "any", "origin": "https://a.googleapis.com", "path": "/*",
		 "dynamicTargetHosts": ["*.googleapis.com"]}
	]}`), cfg, nil)
	require.NoError(t, err)

	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)

	var out struct {
		Private []map[string]any `json:"private"`
	}
	require.NoError(t, json.Unmarshal(rendered, &out))

	var found bool
	for _, rule := range out.Private {
		if hosts, ok := rule[RuleKeyDynamicTargetHosts]; ok {
			require.Equal(t, []any{"*.googleapis.com"}, hosts)
			found = true
		}
	}
	require.True(t, found, "dynamicTargetHosts did not survive rendering")
}

// AddRule builds rules from the typed struct rather than a raw dict, so the
// field has to be on the struct too or generated rules can never opt in.
func TestDynamicTargetHostsOnGeneratedRule(t *testing.T) {
	cfg := axonConfig.NewAgentEnvConfig()
	af, err := NewAcceptFile([]byte(`{"private": []}`), cfg, nil)
	require.NoError(t, err)

	wrapper := newAcceptFileWrapper(af.content, af)
	added := wrapper.AddRule(RULES_PRIVATE, acceptFileRule{
		Method:             "any",
		Path:               "/*",
		Origin:             "https://a.googleapis.com",
		DynamicTargetHosts: []string{"*.googleapis.com"},
	})
	require.Equal(t, []string{"*.googleapis.com"}, added.DynamicTargetHosts())
}

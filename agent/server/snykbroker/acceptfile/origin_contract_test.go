package acceptfile

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// AcceptFileRuleWrapper.Origin() is the one accessor this package shares with
// the snyk-broker reflector: relay_instance_manager.go reads it to decide
// whether a rule needs a wildcard policy, to build the reflector proxy URI, and
// to report a bad origin. Nothing else in here is reachable from that path —
// Path() and MatchRule have no callers outside this package and grpctunnel.
//
// It is pinned here, before the routing work that follows refactors it, so a
// change to the shared accessor cannot quietly alter what the reflector sees.
func TestOriginContract(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		env    map[string]string
		want   string
	}{
		{
			name:   "absolute origin is returned verbatim",
			origin: "https://api.github.com",
			want:   "https://api.github.com",
		},
		{
			name:   "origin keeps its base path",
			origin: "https://git.example/api/v3",
			want:   "https://git.example/api/v3",
		},
		{
			name:   "origin keeps its port",
			origin: "http://localhost:9999",
			want:   "http://localhost:9999",
		},
		{
			name:   "plaintext scheme is not upgraded",
			origin: "http://internal.example",
			want:   "http://internal.example",
		},
		{
			name:   "a scheme-less origin defaults to https",
			origin: "github.com",
			want:   "https://github.com",
		},
		{
			name:   "a scheme-less default from ${VAR:default} defaults to https",
			origin: "${GITHUB:github.com}",
			want:   "https://github.com",
		},
		{
			name:   "${VAR} expands from the environment",
			origin: "${GITHUB_API}",
			env:    map[string]string{"GITHUB_API": "https://ghe.example/api/v3"},
			want:   "https://ghe.example/api/v3",
		},
		{
			name:   "${VAR:default} prefers the environment when set",
			origin: "${GITHUB:github.com}",
			env:    map[string]string{"GITHUB": "https://ghe.example"},
			want:   "https://ghe.example",
		},
		{
			// The reflector greps Origin() for "*" to decide a rule needs a
			// wildcard policy, so the star has to survive verbatim.
			name:   "a wildcard family survives untouched",
			origin: "https://*.googleapis.com",
			want:   "https://*.googleapis.com",
		},
		{
			name:   "a wildcard family keeps its port",
			origin: "https://*.api.example.net:8443",
			want:   "https://*.api.example.net:8443",
		},
		{
			name:   "a wildcard family from a ${VAR:default}",
			origin: "${GOOGLE_API:https://*.googleapis.com}",
			want:   "https://*.googleapis.com",
		},
		{
			name:   "credentials in the origin are preserved",
			origin: "http://user:pass@localhost:9000",
			want:   "http://user:pass@localhost:9000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}
			content := `{"private":[{"method":"any","path":"/*","origin":"` + tc.origin + `"}]}`
			af, err := NewAcceptFile([]byte(content), cfg, zap.NewNop())
			require.NoError(t, err)

			rules := af.Wrapper().PrivateRules()
			require.Len(t, rules, 1)
			require.Equal(t, tc.want, rules[0].Origin())
		})
	}
}

// A rule with no origin reads as empty rather than panicking: the reflector
// calls Origin() on every private rule before anything has vetted them.
func TestOriginOfRuleWithoutOneIsEmpty(t *testing.T) {
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}
	af, err := NewAcceptFile([]byte(`{"private":[{"method":"any","path":"/*"}]}`), cfg, zap.NewNop())
	require.NoError(t, err)

	rules := af.Wrapper().PrivateRules()
	require.Len(t, rules, 1)
	require.Equal(t, "", rules[0].Origin())
	require.Equal(t, "", rules[0].RawOrigin())
}

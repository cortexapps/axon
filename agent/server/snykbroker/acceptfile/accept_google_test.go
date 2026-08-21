package acceptfile

import (
	"os"
	"path/filepath"
	"testing"

	axonConfig "github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// googleTemplateOrigin returns the origin the shipped google template resolves to.
//
// Origin() rather than the rendered JSON, because the rendered file keeps
// ${GOOGLE_API} for the broker to expand. Origin() is what the reflector builds
// its proxy target from.
//
// The google-adc stub is required: Headers() locates the plugin and panics if it
// cannot, and a real binary would tie the result to the machine's identity.
func googleTemplateOrigin(t *testing.T) string {
	t.Helper()

	pluginDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(pluginDir, "google-adc"),
		[]byte("#!/bin/sh\nprintf 'Bearer stub'\n"), 0700))

	content, err := os.ReadFile(filepath.Join("..", "accept_files", "accept.google.json"))
	require.NoError(t, err)

	cfg := axonConfig.AgentConfig{HttpServerPort: 80, PluginDirs: []string{pluginDir}}
	af, err := NewAcceptFile(content, cfg, zap.NewNop())
	require.NoError(t, err)

	for _, rule := range af.wrapper.PrivateRules() {
		if len(rule.Headers()) > 0 {
			return rule.Origin()
		}
	}
	require.Fail(t, "the template has no rule with headers")
	return ""
}

// One wildcard rule covers every Google API host Cortex calls: they all resolve to
// a hostname exactly one label under googleapis.com. That this parses as a
// one-label wildcard family is pinned in TestParseOriginAcceptsWildcardFamilies.
func TestGoogleTemplateAuthorizesTheGoogleAPIFamilyByDefault(t *testing.T) {
	require.Equal(t, "https://*.googleapis.com", googleTemplateOrigin(t))
}

// So a deployment reaching a narrower or different host family does not have to
// replace the whole file. Documented in README.relay.md.
func TestGoogleTemplateOriginCanBeOverridden(t *testing.T) {
	t.Setenv("GOOGLE_API", "https://*.mycompany-proxy.example.com")
	require.Equal(t, "https://*.mycompany-proxy.example.com", googleTemplateOrigin(t))
}

func TestGoogleTemplateUsesThePluginForAuthorization(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "accept_files", "accept.google.json"))
	require.NoError(t, err)
	require.Contains(t, string(content), "${plugin:google-adc}")
}

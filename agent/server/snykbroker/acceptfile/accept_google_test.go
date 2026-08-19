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
// Origin() rather than the rendered JSON: the rendered file keeps ${GOOGLE_API} for
// the broker to expand, exactly as every other template does, so the raw output
// says nothing about which host family was authorized. Origin() is the value the
// reflector builds its proxy target from, which makes it the one that decides the
// destination.
//
// A stub stands in for google-adc so the result does not depend on what identity
// the machine running the test has, or on whether the binary has been built.
// Headers() locates the plugin, and panics if it cannot, so the stub has to exist.
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

// One wildcard rule covers every Google API host: all 42 client constructions in
// Cortex resolve to a hostname exactly one label under googleapis.com.
func TestGoogleTemplateAuthorizesTheGoogleAPIFamilyByDefault(t *testing.T) {
	// That this origin parses as a one-label wildcard family is pinned where the
	// parser lives, in TestParseOriginAcceptsWildcardFamilies.
	require.Equal(t, "https://*.googleapis.com", googleTemplateOrigin(t))
}

// The override exists so a deployment that must reach a narrower or different host
// family does not have to replace the whole file. Documented in README.relay.md
// alongside the template.
func TestGoogleTemplateOriginCanBeOverridden(t *testing.T) {
	t.Setenv("GOOGLE_API", "https://*.mycompany-proxy.example.com")
	require.Equal(t, "https://*.mycompany-proxy.example.com", googleTemplateOrigin(t))
}

// The template's authorization header comes from the plugin, so a rendered file
// still carries the placeholder for the agent to expand per request.
func TestGoogleTemplateUsesThePluginForAuthorization(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "accept_files", "accept.google.json"))
	require.NoError(t, err)
	require.Contains(t, string(content), "${plugin:google-adc}")
}

package acceptfile

import (
	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"testing"
)

// newTestRouter builds a Router the way the tunnel client does: parse, render,
// re-parse, take the private rules minus the injected /__axon/* self-route.
func newTestRouter(t *testing.T, rulesJSON string, pluginDirs ...string) *Router {
	t.Helper()
	if pluginDirs == nil {
		pluginDirs = []string{}
	}
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: pluginDirs}

	af, err := NewAcceptFile([]byte(rulesJSON), cfg, zap.NewNop())
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	af2, err := NewAcceptFile(rendered, cfg, zap.NewNop())
	require.NoError(t, err)

	var rules []AcceptFileRuleWrapper
	for _, r := range af2.Wrapper().PrivateRules() {
		if r.Path() != "/__axon/*" {
			rules = append(rules, r)
		}
	}
	return NewRouter(rules, zap.NewNop())
}

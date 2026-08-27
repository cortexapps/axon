package acceptfile

import (
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestRouter builds a Router the way the tunnel client does: parse, render,
// re-parse, take the private rules minus the injected /__axon/* self-route.
func newTestRouter(t *testing.T, rulesJSON string, pluginDirs ...string) *Router {
	t.Helper()
	rt, err := newTestRouterErr(t, rulesJSON, pluginDirs...)
	require.NoError(t, err)
	return rt
}

// newTestRouterErr is the same, for the cases that are about the Router
// refusing to be built at all.
func newTestRouterErr(t *testing.T, rulesJSON string, pluginDirs ...string) (*Router, error) {
	t.Helper()
	if pluginDirs == nil {
		pluginDirs = []string{}
	}
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: pluginDirs}

	af, err := NewAcceptFile([]byte(rulesJSON), cfg, zap.NewNop())
	if err != nil {
		return nil, err
	}
	rendered, err := af.Render(zap.NewNop())
	if err != nil {
		return nil, err
	}
	af2, err := NewAcceptFile(rendered, cfg, zap.NewNop())
	if err != nil {
		return nil, err
	}

	var rules []AcceptFileRuleWrapper
	for _, r := range af2.Wrapper().PrivateRules() {
		if r.Path() != "/__axon/*" {
			rules = append(rules, r)
		}
	}
	return NewRouter(rules, zap.NewNop())
}

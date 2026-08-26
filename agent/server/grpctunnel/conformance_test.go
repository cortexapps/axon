package grpctunnel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestAcceptFileConformance runs the shared accept-file conformance
// fixtures (agent/test/conformance/) through the gRPC-tunnel path:
// accept-file render → Router.Route. The same fixtures are played through
// the snyk-broker E2E harness; together they pin both transports to one
// definition of accept-file semantics (design doc §9).
func TestAcceptFileConformance(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "test", "conformance")
	entries, err := os.ReadDir(fixtureDir)
	require.NoError(t, err)

	ran := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ran++
		t.Run(strings.TrimSuffix(e.Name(), ".json"), func(t *testing.T) {
			runConformanceFixture(t, filepath.Join(fixtureDir, e.Name()))
		})
	}
	require.Greater(t, ran, 0, "no conformance fixtures found in %s", fixtureDir)
}

type conformanceFixture struct {
	Name       string            `json:"name"`
	Env        map[string]string `json:"env"`
	AcceptFile json.RawMessage   `json:"acceptFile"`
	Cases      []conformanceCase `json:"cases"`
}

type conformanceCase struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	} `json:"request"`
	Expect struct {
		Matched bool              `json:"matched"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"expect"`
}

func runConformanceFixture(t *testing.T, path string) {
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var fixture conformanceFixture
	require.NoError(t, json.Unmarshal(raw, &fixture), "fixture %s is not valid JSON", path)
	require.NotEmpty(t, fixture.Cases, "fixture %s has no cases", path)

	for k, v := range fixture.Env {
		t.Setenv(k, v)
	}

	// Build the router exactly the way the tunnel client does: parse,
	// render, re-parse, take private rules.
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}
	af, err := acceptfile.NewAcceptFile(fixture.AcceptFile, cfg, zap.NewNop())
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	af2, err := acceptfile.NewAcceptFile(rendered, cfg, zap.NewNop())
	require.NoError(t, err)

	// The rendered file gains the /__axon/* self-route; exclude it, as the
	// fixtures describe only user rules.
	var rules []acceptfile.AcceptFileRuleWrapper
	for _, r := range af2.Wrapper().PrivateRules() {
		if r.Path() != "/__axon/*" {
			rules = append(rules, r)
		}
	}
	router := NewRouter(rules, zap.NewNop())

	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			start := &pb.CallStart{
				PseudoHeaders: map[string]string{
					":method": c.Request.Method,
					":path":   c.Request.Path,
				},
				Headers: c.Request.Headers,
			}

			breq, err := router.Route(start)

			if !c.Expect.Matched {
				require.Error(t, err, "expected no rule to match")
				var re *RouteError
				require.ErrorAs(t, err, &re)
				assert.Equal(t, int32(404), re.Code)
				return
			}

			require.NoError(t, err, "expected a rule to match")

			if c.Expect.URL != "" {
				assert.Equal(t, c.Expect.URL, breq.URL.String(), "outgoing URL")
			}
			for k, v := range c.Expect.Headers {
				assert.Equal(t, v, breq.Header.Get(k), "outgoing header %q", k)
			}
		})
	}
}

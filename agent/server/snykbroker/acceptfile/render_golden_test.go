package acceptfile

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite golden render files")

// reFileVar matches ${VAR} and ${VAR:default} env references (not typed
// refs like ${plugin:...}, which carry a lowercase type prefix).
var reFileVar = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)(?::[^}]*)?\}`)

// TestRenderGolden pins the rendered output of every built-in accept file.
// Any change to the acceptfile package that alters rendered output for
// existing accept files fails this test — additions for the gRPC tunnel
// must be invisible to the snyk-broker path (design doc §9).
//
// Update intentionally with:
//
//	go test ./server/snykbroker/acceptfile/ -run TestRenderGolden -update-golden
func TestRenderGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "accept_files", "accept.*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no built-in accept files found")

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			content, err := os.ReadFile(file)
			require.NoError(t, err)

			// Set a deterministic value for every env var the file
			// references so rendering is reproducible.
			for _, m := range reFileVar.FindAllStringSubmatch(string(content), -1) {
				t.Setenv(m[1], "https://"+strings.ToLower(strings.ReplaceAll(m[1], "_", "-"))+".golden.test")
			}

			cfg := config.AgentConfig{
				HttpServerPort: 80,
				PluginDirs:     []string{},
			}
			af, err := NewAcceptFile(content, cfg, zap.NewNop())
			require.NoError(t, err)
			rendered, err := af.Render(zap.NewNop())
			require.NoError(t, err)

			// Normalize for stable comparison.
			var pretty bytes.Buffer
			require.NoError(t, json.Indent(&pretty, rendered, "", "  "))
			got := pretty.Bytes()

			goldenPath := filepath.Join("testdata", "render_golden", name+".golden.json")
			if *updateGolden {
				require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0755))
				require.NoError(t, os.WriteFile(goldenPath, got, 0644))
				return
			}

			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err,
				"missing golden file %s — run with -update-golden to create it", goldenPath)
			require.Equal(t, string(want), string(got),
				"rendered output changed for %s; if intentional, re-run with -update-golden and include the diff in review", file)
		})
	}
}

package acceptfile

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite golden render files")

// pendingGolden names accept files whose rendered output is deliberately not
// pinned yet, mapped to why. An accept file arrives here when the integration
// behind it still needs work: generating a golden first would pin whatever the
// renderer happens to do today as though someone had decided it, and the test
// would then defend that decision against the person who comes to make it.
//
// Skipping says the same thing out loud and costs nothing but a line here when
// the work lands.
var pendingGolden = map[string]string{
	"accept.google": "google integration support is a follow-up",
}

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
	files := builtinAcceptFiles(t)
	require.NotEmpty(t, files, "no built-in accept files found")

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			if why, pending := pendingGolden[name]; pending {
				t.Skipf("render not pinned for %s: %s", name, why)
			}

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

// builtinAcceptFiles lists the accept files that ship in the repo.
//
// It asks git rather than globbing the directory: an accept file is "built-in"
// because it is committed, not because it happens to sit on this disk. A
// developer's untracked scratch accept file would otherwise fail this test with
// a missing golden it has no business needing. Outside a git checkout (vendored
// source, some release builds) there is nothing to ask, so fall back to the
// glob — every file present there is tracked by definition.
func builtinAcceptFiles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "accept_files")

	out, err := exec.Command("git", "-C", dir, "ls-files", "--", "accept.*.json").Output()
	if err != nil {
		globbed, gerr := filepath.Glob(filepath.Join(dir, "accept.*.json"))
		require.NoError(t, gerr)
		return globbed
	}

	var files []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == "" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}

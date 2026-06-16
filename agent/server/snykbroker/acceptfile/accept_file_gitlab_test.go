package acceptfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// builtinRule is a minimal view of an accept-file rule, used to assert the
// shape of the built-in accept files without depending on the broker's
// rule-matching internals.
type builtinRule struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Origin string `json:"origin"`
	Auth   struct {
		Scheme   string `json:"scheme"`
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	} `json:"auth"`
}

type builtinAcceptFile struct {
	Private []builtinRule `json:"private"`
}

// TestGitlabAcceptFileHasScaffolderRules guards the GitLab scaffolder parity
// with GitHub (CD-242). A scaffolder git clone over the relay performs
// git-over-HTTP, which authenticates with HTTP Basic (token as password) — not
// the bearer scheme the GitLab REST/GraphQL API uses. Without Basic-auth rules
// for the git smart-HTTP endpoints, GitLab returns 401 then 404 on
// /info/refs?service=git-upload-pack. This mirrors the allowlist proven to fix
// Fiserv's NA agent (CD-219).
func TestGitlabAcceptFileHasScaffolderRules(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "accept_files", "accept.gitlab.json"))
	require.NoError(t, err)

	var af builtinAcceptFile
	require.NoError(t, json.Unmarshal(contents, &af))

	// Every git smart-HTTP endpoint a clone/push touches must have a Basic-auth rule.
	gitPaths := []string{"/*/info/refs", "/*/git-upload-pack", "/*/git-receive-pack"}
	for _, path := range gitPaths {
		var rule *builtinRule
		for i := range af.Private {
			if af.Private[i].Path == path {
				rule = &af.Private[i]
				break
			}
		}
		require.NotNilf(t, rule, "missing accept rule for git path %s", path)
		require.Equalf(t, "basic", rule.Auth.Scheme, "%s must use HTTP Basic for git-over-HTTP", path)
		require.NotEmptyf(t, rule.Auth.Username, "%s Basic auth requires a non-empty username", path)
		require.Equalf(t, "${GITLAB_TOKEN}", rule.Auth.Password, "%s must authenticate with the GitLab token", path)
	}

	// The catch-all API rule (bearer) must still be present for non-git traffic.
	hasBearerAPIRule := false
	for _, rule := range af.Private {
		if rule.Path == "/*" && rule.Auth.Scheme == "bearer" {
			hasBearerAPIRule = true
			break
		}
	}
	require.True(t, hasBearerAPIRule, "expected the catch-all bearer API rule to remain")
}

package acceptfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Parity with the snyk-broker matcher for the constructs Axon accept files
// actually use. The broker compiles paths with path-to-regexp@1.9.0, where "*"
// becomes "(.*)" and crosses "/". path.Match, which this package used first,
// stops at "/" — which silently broke every accept rule with an infix wildcard.

func TestMatchesPath_WildcardCrossesSegments(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Trailing wildcard.
		{"/*", "/", true},
		{"/*", "/a", true},
		{"/*", "/a/b/c", true},
		{"/api/*", "/api/repos", true},
		{"/api/*", "/api/a/b/c", true},
		{"/api/*", "/other/repos", false},
		// The broker requires the separator: "/api/*" is "/api/" plus anything.
		{"/api/*", "/api", false},
		{"/__axon/*", "/__axon/health", true},
		{"/__axon/*", "/__axon/broker/x/y", true},

		// Infix wildcard — the GitLab git-over-HTTP shape. Nested groups are
		// the common case and must match.
		{"/*/info/refs", "/myrepo/info/refs", true},
		{"/*/info/refs", "/mygroup/myrepo/info/refs", true},
		{"/*/info/refs", "/a/b/c/myrepo.git/info/refs", true},
		{"/*/info/refs", "/myrepo/info/other", false},
		{"/*/git-upload-pack", "/group/sub/project.git/git-upload-pack", true},
		{"/*/git-receive-pack", "/group/sub/project.git/git-receive-pack", true},

		// A pattern the broker normalizes by prepending "/".
		{"*/info/refs", "/group/project.git/info/refs", true},

		// Exact paths stay exact.
		{"/graphql", "/graphql", true},
		{"/graphql", "/graphql/x", false},
		{"/api/v1/repos", "/api/v1/repos", true},
		{"/api/v1/repos", "/api/v2/repos", false},

		// An empty pattern matches nothing.
		{"", "/anything", false},
	}

	for _, tt := range cases {
		t.Run(tt.pattern+" <- "+tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, matchesPath(tt.pattern, tt.path))
		})
	}
}

// The regression this exists for: accept.gitlab.json's scaffolder clone rules
// against a nested GitLab group. Over the broker these clone; before the
// wildcard fix they 404'd over the tunnel.
func TestGitlabTemplateMatchesNestedGroups(t *testing.T) {
	for _, v := range []string{"GITLAB_API", "GITLAB_TOKEN"} {
		t.Setenv(v, "https://gitlab.example")
	}
	content, err := os.ReadFile(filepath.Join("..", "accept_files", "accept.gitlab.json"))
	require.NoError(t, err)

	rt := newTestRouter(t, string(content))

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/project.git/info/refs?service=git-upload-pack"},
		{"GET", "/group/project.git/info/refs?service=git-upload-pack"},
		{"GET", "/group/subgroup/project.git/info/refs?service=git-upload-pack"},
		{"POST", "/group/subgroup/project.git/git-upload-pack"},
		{"POST", "/group/subgroup/project.git/git-receive-pack"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			routed, err := rt.Route(c.method, c.path, nil)
			require.NoError(t, err)
			// Git-over-HTTP authenticates with Basic, not the API's bearer.
			require.True(t, hasBasicAuth(routed.Header.Get("Authorization")),
				"git smart-HTTP must use Basic auth, got %q", routed.Header.Get("Authorization"))
		})
	}

	// Non-git traffic still lands on the bearer catch-all.
	routed, err := rt.Route("GET", "/api/v4/projects", nil)
	require.NoError(t, err)
	require.Contains(t, routed.Header.Get("Authorization"), "Bearer ")
}

func hasBasicAuth(v string) bool {
	return len(v) > 6 && v[:6] == "Basic "
}

// ---------------------------------------------------------------------------
// ${VAR} inside path
// ---------------------------------------------------------------------------

// snyk-broker treats ${VAR} in a path as a segment placeholder: the rule
// matches whatever the caller sent in that position, and the configured value
// is substituted into the outgoing URL. It is a rewrite, not a filter.
//
// An earlier revision of this branch pinned the segment to the configured value
// instead, which reads better as an allowlist but is not what the broker does —
// a request the broker rewrote and forwarded would have 404'd on the tunnel.
// Enabling the tunnel switches deployments that are running on snyk-broker
// today, so the quirk has to come with them.
func TestPathEnvVarMatchesAnySegmentAndRewrites(t *testing.T) {
	t.Setenv("GITHUB_ORG", "acme")
	t.Setenv("UPSTREAM", "https://up.example")

	rt := newTestRouter(t, `{"private":[
		{"method":"any","path":"/repos/${GITHUB_ORG}/*","origin":"${UPSTREAM}"}
	]}`)

	// The configured value passes through unchanged.
	routed, err := rt.Route("GET", "/repos/acme/widget", nil)
	require.NoError(t, err)
	require.Equal(t, "https://up.example/repos/acme/widget", routed.URL.String())

	// Any other segment matches too, and is rewritten to the configured value.
	routed, err = rt.Route("GET", "/repos/someone-else/widget", nil)
	require.NoError(t, err)
	require.Equal(t, "https://up.example/repos/acme/widget", routed.URL.String(),
		"the caller's segment is replaced by the configured value")

	// One segment, not many: the placeholder does not swallow separators.
	_, err = rt.Route("GET", "/repos/a/b/widget", nil)
	require.NoError(t, err, "still matched by the trailing wildcard")

	// A path that does not reach the placeholder still misses.
	_, err = rt.Route("GET", "/other/acme/widget", nil)
	require.ErrorIs(t, err, ErrNoRoute)
}

// The rewrite reaches the escaped form too, so what travels on the wire agrees
// with what the rule matched.
func TestPathEnvVarRewritesTheEscapedPath(t *testing.T) {
	t.Setenv("GITHUB_ORG", "acme")
	t.Setenv("UPSTREAM", "https://up.example")

	rt := newTestRouter(t, `{"private":[
		{"method":"any","path":"/repos/${GITHUB_ORG}/*","origin":"${UPSTREAM}"}
	]}`)

	routed, err := rt.Route("GET", "/repos/some%20one/a%2Fb", nil)
	require.NoError(t, err)
	require.Equal(t, "/repos/acme/a%2Fb", routed.URL.EscapedPath(),
		"the placeholder is rewritten and the rest keeps its encoding")
}

// A placeholder whose variable is unset leaves the caller's segment alone
// rather than collapsing it to nothing; snyk-broker likewise substitutes only a
// truthy value. Exercised against matchPath directly: ensureAcceptFileVars
// refuses a file that references an unset variable, so this state is not
// reachable through NewAcceptFile — the branch exists so a Router built from
// rules by some other route cannot produce "//" in a URL.
func TestMatchPathLeavesTheSegmentWhenTheVariableIsUnset(t *testing.T) {
	require.Empty(t, os.Getenv("DEFINITELY_UNSET_ORG"))

	got, ok := matchPath("/repos/${DEFINITELY_UNSET_ORG}/*", "/repos/whoever/widget")
	require.True(t, ok)
	require.Equal(t, "/repos/whoever/widget", got)
}

// ---------------------------------------------------------------------------
// method default
// ---------------------------------------------------------------------------

// snyk-broker defaults a rule with no method to GET. Leaving it unmatched makes
// the rule silently dead.
func TestRuleWithoutMethodDefaultsToGet(t *testing.T) {
	t.Setenv("UPSTREAM", "https://up.example")
	rt := newTestRouter(t, `{"private":[{"path":"/api/*","origin":"${UPSTREAM}"}]}`)

	routed, err := rt.Route("GET", "/api/x", nil)
	require.NoError(t, err)
	require.Equal(t, "https://up.example/api/x", routed.URL.String())

	_, err = rt.Route("POST", "/api/x", nil)
	require.ErrorIs(t, err, ErrNoRoute)
}

// ---------------------------------------------------------------------------
// directory traversal — snyk-broker rejects anything path.normalize() changes
// ---------------------------------------------------------------------------

func TestTraversalIsRejected(t *testing.T) {
	t.Setenv("UPSTREAM", "https://up.example")
	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${UPSTREAM}"}]}`)

	blocked := map[string]string{
		"parent segment":    "/api/../admin",
		"encoded parent":    "/api/%2E%2E/admin",
		"encoded slash dot": "/api/%2e%2e%2Fadmin",
		"inner parent":      "/api/x/../y",
		"current segment":   "/api/./x",
		"double slash":      "/a//b",
		"leading double":    "//api/x",
		"trailing parent":   "/api/..",
	}
	for name, path := range blocked {
		t.Run(name, func(t *testing.T) {
			_, err := rt.Route("GET", path, nil)
			var invalid *InvalidRequestError
			require.ErrorAs(t, err, &invalid, "path=%q must be refused", path)
		})
	}

	allowed := map[string]string{
		"plain":                 "/api/x",
		"trailing slash":        "/api/",
		"root":                  "/",
		"dot in filename":       "/api/.gitignore",
		"dots inside a segment": "/api/a..b",
		"encoded slash":         "/api/v4/projects/group%2Fproject",
	}
	for name, path := range allowed {
		t.Run(name, func(t *testing.T) {
			_, err := rt.Route("GET", path, nil)
			require.NoError(t, err, "path=%q must be allowed", path)
		})
	}
}

// ---------------------------------------------------------------------------
// fragments — discarded before matching, as snyk-broker does
// ---------------------------------------------------------------------------

func TestFragmentIsDiscardedBeforeMatching(t *testing.T) {
	t.Setenv("UPSTREAM", "https://up.example")
	rt := newTestRouter(t, `{"private":[
		{"method":"any","path":"/repos/*/contents/*.json","origin":"${UPSTREAM}"}
	]}`)

	// The broker's case: a sensitive file hidden after a fragment must not
	// smuggle the rule into matching.
	_, err := rt.Route("GET", "/repos/x/contents/id_rsa#/package.json", nil)
	require.ErrorIs(t, err, ErrNoRoute)

	// And a fragment on an otherwise-valid path is dropped, not encoded.
	routed, err := rt.Route("GET", "/repos/x/contents/package.json#L10", nil)
	require.NoError(t, err)
	require.Equal(t, "https://up.example/repos/x/contents/package.json", routed.URL.String())
}

func TestFragmentIsDiscardedBeforeQuery(t *testing.T) {
	t.Setenv("UPSTREAM", "https://up.example")
	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/api/*","origin":"${UPSTREAM}"}]}`)

	routed, err := rt.Route("GET", "/api/search?q=x#frag", nil)
	require.NoError(t, err)
	require.Equal(t, "https://up.example/api/search?q=x", routed.URL.String())
}

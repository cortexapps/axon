package acceptfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cortexapps/axon/config"
)

// Nothing in an accept file stops the agent.
//
// Enabling the tunnel switches deployments running on snyk-broker today, and
// that switch has to be transparent: a file the broker accepts must still
// start. Anything the Router cannot carry is warned about and ignored.
//
// The refusal that used to be here is gone, and the warnings live at Router
// construction rather than at parse: parsing is shared with the snyk-broker
// path, where the Node broker honours these constructs, so warning at parse
// would tell an operator their working rule is being dropped when it is not.
// TestSnykBrokerConstructsStillParse pins that boundary.

// loadWithLogs parses an accept file with a logger the test can inspect.
func loadWithLogs(t *testing.T, content string) (*AcceptFile, []observer.LoggedEntry, error) {
	t.Helper()
	core, logs := observer.New(zapcore.WarnLevel)
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}
	af, err := NewAcceptFile([]byte(content), cfg, zap.New(core))
	return af, logs.All(), err
}

// routerWithLogs builds the Router, which is where a construct it cannot carry
// gets warned about.
func routerWithLogs(t *testing.T, content string) (*Router, []observer.LoggedEntry) {
	t.Helper()
	core, logs := observer.New(zapcore.WarnLevel)
	cfg := config.AgentConfig{HttpServerPort: 8080, PluginDirs: []string{}}

	af, err := NewAcceptFile([]byte(content), cfg, zap.New(core))
	require.NoError(t, err, "parsing must never refuse an accept file")
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	af2, err := NewAcceptFile(rendered, cfg, zap.New(core))
	require.NoError(t, err)

	var rules []AcceptFileRuleWrapper
	for _, r := range af2.Wrapper().PrivateRules() {
		if r.Path() != "/__axon/*" {
			rules = append(rules, r)
		}
	}
	return NewRouter(rules, zap.New(core)), logs.All()
}

// requireWarns asserts the file loads, the Router builds, and a warning names
// the construct being ignored.
func requireWarns(t *testing.T, content, mentions string) []observer.LoggedEntry {
	t.Helper()
	rt, logs := routerWithLogs(t, content)
	require.NotNil(t, rt)
	require.NotEmpty(t, logs, "an ignored construct has to be warned about")

	for _, entry := range logs {
		if containsAny(entry, mentions) {
			return logs
		}
	}
	require.Failf(t, "no warning mentioned the construct",
		"looking for %q in %v", mentions, messages(logs))
	return logs
}

func containsAny(entry observer.LoggedEntry, needle string) bool {
	if strings.Contains(entry.Message, needle) {
		return true
	}
	for _, f := range entry.Context {
		if strings.Contains(f.String, needle) {
			return true
		}
	}
	return false
}

func messages(logs []observer.LoggedEntry) []string {
	out := make([]string, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.Message)
	}
	return out
}

// ---------------------------------------------------------------------------
// valid: only header requirements are applied
// ---------------------------------------------------------------------------

// A body filter narrows the rule in snyk-broker. The Router does not inspect
// bodies, so the rule matches more here than it did there — say so rather than
// refuse to start.
func TestBodyValidFilterWarnsAndIsIgnored(t *testing.T) {
	requireWarns(t, `{"private":[{
		"method":"POST","path":"/*","origin":"https://up.example",
		"valid":[{"path":"proxy.*","value":"please"}]}]}`, "path")
}

func TestBodyRegexValidFilterWarnsAndIsIgnored(t *testing.T) {
	requireWarns(t, `{"private":[{
		"method":"POST","path":"/*","origin":"https://up.example",
		"valid":[{"path":"commits.*.added.*","regex":"package.json"}]}]}`, "path")
}

func TestQueryValidFilterWarnsAndIsIgnored(t *testing.T) {
	requireWarns(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example",
		"valid":[{"queryParam":"proxyMe","values":["please"]}]}]}`, "queryParam")
}

// The header entry is still applied; only the query entry is dropped.
func TestMixedValidArrayWarnsOnlyForTheUnsupportedEntry(t *testing.T) {
	rt, logs := routerWithLogs(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example",
		"valid":[
			{"header":"x-cortex-service","values":["scaffolder"]},
			{"queryParam":"proxyMe","values":["please"]}
		]}]}`)
	require.Len(t, logs, 1)

	// The header requirement still gates the rule.
	_, err := rt.Route("GET", "/thing", nil)
	require.ErrorIs(t, err, ErrNoRoute, "the header requirement must still apply")

	routed, err := rt.Route("GET", "/thing", map[string]string{"x-cortex-service": "scaffolder"})
	require.NoError(t, err)
	require.Equal(t, "https://up.example/thing", routed.URL.String())
}

func TestValidEntryWithNoRecognizedKeyWarns(t *testing.T) {
	requireWarns(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example",
		"valid":[{"values":["scaffolder"]}]}]}`, "no recognized key")
}

func TestHeaderValidFilterIsAppliedSilently(t *testing.T) {
	_, logs := routerWithLogs(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example",
		"valid":[{"header":"x-cortex-service","values":["scaffolder"]}]}]}`)
	require.Empty(t, logs)
}

// The broker's own comment convention appears inside valid entries too.
func TestCommentKeysInsideValidAreIgnoredSilently(t *testing.T) {
	_, logs := routerWithLogs(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example",
		"valid":[{"//":"scaffolder only","header":"x-cortex-service","values":["scaffolder"]}]}]}`)
	require.Empty(t, logs)
}

// ---------------------------------------------------------------------------
// Snyk-specific rule fields
// ---------------------------------------------------------------------------

// The relay negotiates no client capabilities, so the gate cannot be evaluated
// and the rule is allowed through. snyk-broker would have rejected the request.
func TestRequiredCapabilitiesWarnsAndIsIgnored(t *testing.T) {
	requireWarns(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example",
		"requiredCapabilities":["post-streams"]}]}`, "requiredCapabilities")
}

// The tunnel streams every body, so "stream": true asks for what it already
// does — no warning, nothing to ignore.
func TestStreamFieldIsAcceptedSilently(t *testing.T) {
	_, logs := routerWithLogs(t, `{"private":[{
		"method":"GET","path":"/*","origin":"https://up.example","stream":true}]}`)
	require.Empty(t, logs)
}

// ---------------------------------------------------------------------------
// Inbound rules
// ---------------------------------------------------------------------------

func TestPublicRulesAreIgnoredWithAWarning(t *testing.T) {
	af, logs, err := loadWithLogs(t, `{"private":[
		{"method":"any","path":"/*","origin":"https://up.example"}],
		"public":[
		{"method":"POST","path":"/webhook/github","origin":"https://up.example"}]}`)
	require.NoError(t, err, "an inbound section must not stop the agent")
	require.NotNil(t, af)

	require.Len(t, logs, 1, "exactly one warning, not one per rule")
	require.Equal(t, zapcore.WarnLevel, logs[0].Level)
	require.Contains(t, logs[0].Message, "Ignoring inbound rules")
	require.Contains(t, logs[0].Message, "will be removed",
		"the warning has to say the section is going away")

	require.Len(t, af.Wrapper().PrivateRules(), 1)
}

// The section survives a round trip even though nothing reads it, so an
// operator editing the rendered file still sees what they wrote.
func TestPublicRulesArePreservedThroughRender(t *testing.T) {
	af, _, err := loadWithLogs(t, `{"private":[],"public":[
		{"method":"POST","path":"/webhook/github"}]}`)
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	require.Contains(t, string(rendered), `"/webhook/github"`)
}

// Render always emits an empty public array, and re-parsing its own output must
// neither fail nor warn — warning on the shape we ourselves produce would train
// everyone to ignore the warning.
func TestEmptyPublicArrayIsAcceptedSilently(t *testing.T) {
	for _, content := range []string{
		`{}`,
		`{"private":[]}`,
		`{"public":[]}`,
		`{"private":[],"public":[]}`,
	} {
		t.Run(content, func(t *testing.T) {
			_, logs, err := loadWithLogs(t, content)
			require.NoError(t, err)
			require.Empty(t, logs, "an empty inbound section must be silent")
		})
	}
}

// ---------------------------------------------------------------------------
// Nothing refuses an accept file
// ---------------------------------------------------------------------------

// Every construct the Router cannot carry is one snyk-broker implements. A
// deployment switching onto the tunnel has to start on the file it is already
// running, so none of these may stop either the parse or the Router.
func TestNoAcceptFileConstructStopsTheAgent(t *testing.T) {
	for name, content := range map[string]string{
		"body filter": `{"private":[{"method":"POST","path":"/*","origin":"https://up.example",
			"valid":[{"path":"proxy.*","value":"please"}]}]}`,
		"body regex filter": `{"private":[{"method":"POST","path":"/*","origin":"https://up.example",
			"valid":[{"path":"commits.*.added.*","regex":"package.json"}]}]}`,
		"query filter": `{"private":[{"method":"GET","path":"/*","origin":"https://up.example",
			"valid":[{"queryParam":"proxyMe","values":["please"]}]}]}`,
		"required capabilities": `{"private":[{"method":"GET","path":"/*","origin":"https://up.example",
			"requiredCapabilities":["post-streams"]}]}`,
		"inbound rules": `{"private":[{"method":"any","path":"/*","origin":"https://up.example"}],
			"public":[{"method":"POST","path":"/webhook/github"}]}`,
		"stream":             `{"private":[{"method":"GET","path":"/*","origin":"https://up.example","stream":true}]}`,
		"unknown rule field": `{"private":[{"method":"GET","path":"/*","origin":"https://up.example","someFutureField":{"a":1}}]}`,
		"comment key":        `{"private":[{"//":"note","method":"GET","path":"/*","origin":"https://up.example"}]}`,
		"everything at once": `{"private":[{"method":"POST","path":"/*","origin":"https://up.example",
			"requiredCapabilities":["x"],
			"valid":[{"path":"a.b","value":"c"},{"queryParam":"q","values":["v"]}]}],
			"public":[{"method":"POST","path":"/hook"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := loadWithLogs(t, content)
			require.NoError(t, err, "parsing must not refuse this")
			rt, _ := routerWithLogs(t, content)
			require.NotNil(t, rt, "the Router must not refuse this")
		})
	}
}

// The snyk-broker path keeps working. Every construct the Router warns about is
// one the Node broker implements, so parsing has to stay silent about it: an
// operator on snyk-broker must not be told their working rule is being dropped.
func TestSnykBrokerConstructsStillParse(t *testing.T) {
	for name, content := range map[string]string{
		"body filter": `{"private":[{"method":"POST","path":"/*","origin":"https://up.example",
			"valid":[{"path":"proxy.*","value":"please"}]}]}`,
		"query filter": `{"private":[{"method":"GET","path":"/*","origin":"https://up.example",
			"valid":[{"queryParam":"proxyMe","values":["please"]}]}]}`,
		"required capabilities": `{"private":[{"method":"GET","path":"/*","origin":"https://up.example",
			"requiredCapabilities":["post-streams"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, logs, err := loadWithLogs(t, content)
			require.NoError(t, err, "the Node broker honours this; parsing must not refuse it")
			require.Empty(t, logs, "nor tell a snyk-broker operator it is being ignored")
		})
	}
}

func TestUnknownRuleKeysAreAcceptedSilently(t *testing.T) {
	_, logs := routerWithLogs(t, `{"private":[{
		"//":"the catch-all API rule",
		"method":"any","path":"/*","origin":"https://up.example",
		"someFutureField":{"a":1}}]}`)
	require.Empty(t, logs)
}

func TestEveryShippedAcceptFileLoads(t *testing.T) {
	for _, file := range acceptFilesUnderTest(t) {
		t.Run(file, func(t *testing.T) {
			for _, v := range fileEnvVars(t, file) {
				t.Setenv(v, "https://"+v+".example")
			}
			_, _, err := loadWithLogs(t, readFile(t, file))
			require.NoError(t, err)
		})
	}
}

// Accept files the repo hands to the agent — the shipped templates and the
// fixtures the docker E2E suites run with. Without the fixtures here, a
// construct this package starts refusing surfaces only as a container that
// will not start, several CI minutes later.
func acceptFilesUnderTest(t *testing.T) []string {
	t.Helper()
	files := builtinAcceptFiles(t)
	for _, fixture := range []string{
		filepath.Join("..", "..", "..", "test", "relay", "accept-client.json"),
		filepath.Join("..", "..", "..", "test", "load", "accept-load.json"),
	} {
		require.FileExists(t, fixture)
		files = append(files, fixture)
	}
	return files
}

// readFile is a t.Fatal-on-error os.ReadFile.
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

// fileEnvVars lists the environment variables an accept file references, so a
// test can give each a deterministic value before loading it.
func fileEnvVars(t *testing.T, path string) []string {
	t.Helper()
	var names []string
	seen := map[string]bool{}
	for _, m := range reFileVar.FindAllStringSubmatch(readFile(t, path), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	return names
}

package snykbroker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Live end-to-end tests for the Google credential source. They use whatever
// Application Default Credentials the machine has and make real requests to
// Google, so they are opt-in:
//
//	make builtin-plugins
//	AXON_GOOGLE_LIVE_TEST=1 go test ./server/snykbroker/ -run Live -v
//
// They cover the whole agent-side path with nothing faked: the shipped accept
// file, the name resolving to the built binary, the
// render step registering a wildcard origin with the reflector, per-request
// retargeting, the subprocess mint, the file cache, and a real Google API
// answering. Nothing here needs brain-backend, a relay token, the snyk-broker, or
// a tunnel - the reflector is an http.Handler and is driven directly.
//
// They are guarded by an environment variable rather than a build tag so they
// still compile and are type-checked in the normal build. A live test that has
// quietly stopped compiling is a live test nobody runs.
const liveTestEnv = "AXON_GOOGLE_LIVE_TEST"

// liveGoogleHost is one label under googleapis.com, so the shipped wildcard
// origin covers it without any accept-file change.
//
// Its list-projects call needs only an authenticated identity. It is not asserted
// to succeed: what the test needs to know is that Google accepted the credential,
// and a 403 for a disabled API or a missing role still proves that. A 401 does
// not.
const (
	liveGoogleHost = "cloudresourcemanager.googleapis.com"
	liveGooglePath = "/v1/projects"
)

func requireLiveGoogle(t *testing.T) {
	t.Helper()
	if os.Getenv(liveTestEnv) != "1" {
		t.Skipf("set %s=1 to run this against real Google credentials", liveTestEnv)
	}
}

// shippedPluginDir returns the directory `make plugins` installs google-adc into,
// to be used as the accept file's PluginDirs. In the image the same binary sits in
// /agent/plugins alongside the shipped shell plugins.
func shippedPluginDir(t *testing.T) string {
	t.Helper()

	// The test's working directory is the package directory.
	dir, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	require.NoError(t, err)

	if _, err := os.Stat(filepath.Join(dir, "google-adc")); err != nil {
		t.Skipf("run `make plugins` first: %v", err)
	}
	return dir
}

// credentialCacheDirEnv duplicates the plugin's own AXON_CREDENTIAL_CACHE_DIR. The
// plugin is a separate module, so this is a process contract rather than a
// constant that can be imported.
const credentialCacheDirEnv = "AXON_CREDENTIAL_CACHE_DIR"

// useIsolatedCredentialCache keeps the test off any cache a running agent owns,
// and makes the cached token observable so reuse can be asserted.
func useIsolatedCredentialCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(credentialCacheDirEnv, dir)
	return dir
}

// cachedTokens returns the credential values currently on disk.
func cachedTokens(t *testing.T, cacheDir string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(cacheDir, "*.json"))
	require.NoError(t, err)

	values := make([]string, 0, len(matches))
	for _, match := range matches {
		data, err := os.ReadFile(match)
		require.NoError(t, err)

		var entry struct {
			Value string `json:"value"`
		}
		require.NoError(t, json.Unmarshal(data, &entry))
		values = append(values, entry.Value)
	}
	return values
}

// liveGoogleEnv renders the shipped accept file into a real reflector, which is
// where the plugin is located and wired into the proxy entry's headers.
func liveGoogleEnv(t *testing.T) (*renderEnv, func() string, string) {
	t.Helper()

	cacheDir := useIsolatedCredentialCache(t)

	logger, logged := newCapturingLogger(t)
	cfg := config.AgentConfig{
		HttpRelayReflectorMode:    config.RelayReflectorAllTraffic,
		ReflectorWebSocketUpgrade: true,
		PluginDirs:                []string{shippedPluginDir(t)},
	}
	rr := newReflectorWithDrain(t, RegistrationReflectorParams{
		Logger:   logger,
		Registry: prometheus.NewRegistry(),
		Config:   cfg,
	})
	t.Cleanup(func() { rr.Stop() })
	env := &renderEnv{
		mgr:       &relayInstanceManager{config: cfg, logger: logger, reflector: rr},
		reflector: rr,
	}

	content, err := os.ReadFile(filepath.Join("accept_files", "accept.google.json"))
	require.NoError(t, err)

	af, err := acceptfile.NewAcceptFile(content, cfg, logger)
	require.NoError(t, err)

	_, err = af.Render(zap.NewNop(), env.mgr.reflectorRenderStep)
	require.NoError(t, err)

	return env, logged, cacheDir
}

// requestGoogle drives one request through the reflector to the live host.
func requestGoogle(t *testing.T, env *renderEnv) *httptest.ResponseRecorder {
	t.Helper()

	uri, err := env.reflector.getUriForTarget("https://*.googleapis.com")
	require.NoError(t, err, "the shipped accept file did not register its wildcard origin")

	req := httptest.NewRequest("GET", proxyPath(t, uri)+liveGooglePath, nil)
	req.Header.Set(HeaderTargetHost, liveGoogleHost)

	rec := httptest.NewRecorder()
	env.reflector.ServeHTTP(rec, req)
	return rec
}

// The whole path, with a real identity and a real Google API at the end of it.
func TestLiveGoogleAcceptsTheInjectedCredential(t *testing.T) {
	requireLiveGoogle(t)
	env, _, cacheDir := liveGoogleEnv(t)

	rec := requestGoogle(t, env)

	// Surfaced only when an assertion fails. A successful list-projects response
	// carries real project and organisation identifiers, which do not belong in a
	// passing test's output.
	diagnostic := truncate(rec.Body.String(), 400)
	t.Logf("%s answered %d", liveGoogleHost, rec.Code)

	// 401 covers two cases the agent cannot currently tell apart: Google refused
	// the token, or the plugin failed and the resolver forwarded the unexpanded
	// placeholder as the header value. Distinguishing them needs the resolver to
	// fail the request instead of logging and continuing, which is not this change.
	require.NotEqual(t, http.StatusUnauthorized, rec.Code,
		"Google refused the authorization header the agent injected: %s", diagnostic)
	require.NotEqual(t, http.StatusForbidden, rec.Code,
		"Google accepted the identity but refused the call; check the IAM binding or whether "+
			"cloudresourcemanager is enabled: %s", diagnostic)

	// A credential was minted and kept, which is what the next request will read.
	tokens := cachedTokens(t, cacheDir)
	require.Len(t, tokens, 1, "expected exactly one cached credential")
	require.NotEmpty(t, tokens[0])
}

// The point of the file cache: a second request does not mint again. Asserted on
// the cached value rather than on timing, since Google will not tell us how many
// tokens it issued.
func TestLiveGoogleReusesTheCachedToken(t *testing.T) {
	requireLiveGoogle(t)
	env, _, cacheDir := liveGoogleEnv(t)

	first := requestGoogle(t, env)
	require.NotEqual(t, http.StatusUnauthorized, first.Code)

	afterFirst := cachedTokens(t, cacheDir)
	require.Len(t, afterFirst, 1)

	info, err := os.Stat(filepath.Join(cacheDir, cacheFileName(t, cacheDir)))
	require.NoError(t, err)
	writtenAt := info.ModTime()

	for i := 0; i < 3; i++ {
		rec := requestGoogle(t, env)
		require.NotEqual(t, http.StatusUnauthorized, rec.Code, "request %d produced no credential", i+2)
	}

	afterMore := cachedTokens(t, cacheDir)
	require.Equal(t, afterFirst, afterMore, "the token changed, so a later request minted again")

	info, err = os.Stat(filepath.Join(cacheDir, cacheFileName(t, cacheDir)))
	require.NoError(t, err)
	require.Equal(t, writtenAt, info.ModTime(), "the cache was rewritten, so a later request minted again")
}

// With a real token in play, this is the assertion worth making live: the value
// the agent injected is nowhere in the logs, at debug level, across a request that
// succeeded and a destination that was rejected.
func TestLiveGoogleCredentialIsNeverLogged(t *testing.T) {
	requireLiveGoogle(t)
	env, logged, cacheDir := liveGoogleEnv(t)

	require.NotEqual(t, http.StatusUnauthorized, requestGoogle(t, env).Code)

	tokens := cachedTokens(t, cacheDir)
	require.Len(t, tokens, 1)

	// The whole header value, and the bare token without its type: a log that
	// printed only the second half would still have leaked the credential.
	credential := tokens[0]
	bare := credential
	if _, after, found := strings.Cut(credential, " "); found {
		bare = after
	}
	require.NotEmpty(t, bare)

	// Also drive a rejected destination, which is the path that logs the most.
	uri, err := env.reflector.getUriForTarget("https://*.googleapis.com")
	require.NoError(t, err)
	rejected := httptest.NewRequest("GET", proxyPath(t, uri)+liveGooglePath, nil)
	rejected.Header.Set(HeaderTargetHost, "not-google.example.com")
	env.reflector.ServeHTTP(httptest.NewRecorder(), rejected)

	contents := logged()
	require.NotContains(t, contents, credential, "the injected header value reached the log")
	require.NotContains(t, contents, bare, "the access token reached the log")
}

// cacheFileName returns the single cache file's name, failing if there is not
// exactly one.
func cacheFileName(t *testing.T, cacheDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cacheDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	return filepath.Base(matches[0])
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

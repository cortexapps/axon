package gcp

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// useCacheDir points the file cache at a directory this test owns. Without it the
// tests would share the real temporary directory, and a token one test wrote
// would be read by another.
func useCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(CacheDirEnv, dir)
	return dir
}

func TestCachedProviderMintsOnceAcrossInstances(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.install(t, respondWithToken("cached-across-instances", 3600))

	first, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	value, err := first.Execute(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer cached-across-instances", value)

	// A second provider stands in for the next subprocess invocation: same cache
	// directory, no shared memory.
	second, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	again, err := second.Execute(context.Background())
	require.NoError(t, err)

	require.Equal(t, value, again)
	require.Equal(t, 1, fakeMetadataServer.mintCount(),
		"the second provider minted again instead of reading the cache")
}

// The cache-hit path must not consult the metadata server at all. If it did, the
// subprocess would still pay a network round trip per request and the file would
// be buying nothing.
func TestCacheHitDoesNotContactTheMetadataServer(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.install(t, respondWithToken("warm-token", 3600))

	warm, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	_, err = warm.Execute(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, fakeMetadataServer.mintCount())

	// From here the metadata server refuses everything, including the detection
	// probe. A provider that reads the cache never notices.
	fakeMetadataServer.refuseEverything(t)

	cold, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	value, err := cold.Execute(context.Background())
	require.NoError(t, err)

	require.Equal(t, "Bearer warm-token", value)
	require.Equal(t, 0, fakeMetadataServer.mintCount(),
		"the cache hit reached the metadata server")
}

// The expiry the issuer stated is what bounds the cached value, not a fixed age.
// A token the metadata server says lives 200 seconds is inside the refresh margin
// and must not be served.
func TestTheIssuersExpiryBoundsTheCache(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.install(t, respondWithToken("short-lived", 200))

	provider, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	_, err = provider.Execute(context.Background())
	require.NoError(t, err)

	entries, err := filepath.Glob(filepath.Join(CacheDir(), CacheName+".*.json"))
	require.NoError(t, err)
	require.Empty(t, entries,
		"a token inside the refresh margin was written, so the next process would serve it")
}

func TestCredentialFileIsOwnerOnly(t *testing.T) {
	dir := useCacheDir(t)
	fakeMetadataServer.install(t, respondWithToken("private-token", 3600))

	provider, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	_, err = provider.Execute(context.Background())
	require.NoError(t, err)

	matches, err := filepath.Glob(filepath.Join(dir, CacheName+".*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 1)

	info, err := os.Stat(matches[0])
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// The fingerprint keys the cache on the identity, so rotating the credential
// configuration does not keep serving the previous one.
func TestChangingTheCredentialConfigurationChangesTheFingerprint(t *testing.T) {
	dir := t.TempDir()

	first := filepath.Join(dir, "one.json")
	require.NoError(t, os.WriteFile(first, []byte(`{"type":"external_account","audience":"one"}`), 0600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", first)
	before := IdentityFingerprint(ScopeCloudPlatform)

	// Same path, different contents: the rotation that a path-keyed cache would
	// miss entirely.
	require.NoError(t, os.WriteFile(first, []byte(`{"type":"external_account","audience":"two"}`), 0600))
	require.NotEqual(t, before, IdentityFingerprint(ScopeCloudPlatform),
		"rewriting the credential file in place did not invalidate the cache key")

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(dir, "two.json"))
	require.NotEqual(t, before, IdentityFingerprint(ScopeCloudPlatform))
}

func TestFingerprintIsStableForTheSameConfiguration(t *testing.T) {
	require.Equal(t, IdentityFingerprint(ScopeCloudPlatform), IdentityFingerprint(ScopeCloudPlatform))
	require.NotEqual(t, IdentityFingerprint(ScopeCloudPlatform), IdentityFingerprint("https://example.com/other"),
		"two scopes share a cache entry, so the narrower one would be served the broader token")
}

func TestFingerprintIsASafePathElement(t *testing.T) {
	fingerprint := IdentityFingerprint(ScopeCloudPlatform)
	require.Len(t, fingerprint, fingerprintLength)
	require.NotContains(t, fingerprint, string(os.PathSeparator))
	require.Regexp(t, `^[0-9a-f]+$`, fingerprint)
}

// The plugin's contract: stdout is exactly the credential.
func TestRunPluginWritesOnlyTheCredential(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.install(t, respondWithToken("plugin-token", 3600))

	var out bytes.Buffer
	require.NoError(t, RunPlugin(context.Background(), &out, zap.NewNop(), false))

	require.Equal(t, "Bearer plugin-token", out.String())
	require.False(t, strings.HasSuffix(out.String(), "\n"),
		"a trailing newline would be concatenated into the header value")
}

// Probe mode reports on the configuration without minting. A startup check that
// minted would consume a token on every relay restart, and would fail startup for
// a transient network fault.
func TestProbeDoesNotMint(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.install(t, respondWithToken("probe-token", 3600))

	var out bytes.Buffer
	require.NoError(t, RunPlugin(context.Background(), &out, zap.NewNop(), true))

	require.Empty(t, out.String(), "probe mode wrote to stdout")
	require.Equal(t, 0, fakeMetadataServer.mintCount(), "probe mode minted a token")
}

func TestRunPluginFailsClosedWhenTheExchangeFails(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.install(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})

	var out bytes.Buffer
	err := RunPlugin(context.Background(), &out, zap.NewNop(), false)

	require.Error(t, err)
	require.Empty(t, out.String(), "a failed mint still wrote something to stdout")
}

// The cache must not turn a failure into a stale success, and must not persist
// anything on a failed exchange.
func TestAFailedMintWritesNoCache(t *testing.T) {
	dir := useCacheDir(t)
	fakeMetadataServer.install(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})

	provider, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err)
	_, err = provider.Execute(context.Background())
	require.Error(t, err)

	matches, err := filepath.Glob(filepath.Join(dir, CacheName+".*"))
	require.NoError(t, err)
	for _, match := range matches {
		require.True(t, strings.HasSuffix(match, ".lock"),
			"a failed mint left %s behind", match)
	}
}

// NewCachedProvider must not detect credentials, or the cache-hit path would pay
// for detection before it ever reads the file.
func TestConstructionDoesNotDetect(t *testing.T) {
	useCacheDir(t)
	fakeMetadataServer.refuseEverything(t)

	_, err := NewCachedProvider(zap.NewNop())
	require.NoError(t, err, "construction reached the credential configuration")
}

func TestRefreshMarginMatchesTheLibraryDefault(t *testing.T) {
	// Restated from the library's unexported defaultExpiryDelta. If the library
	// changes it, TestRefreshMarginIsAsynchronousAndWideEnough fails and this
	// constant has to move with it.
	require.Equal(t, 225*time.Second, RefreshMargin)
}

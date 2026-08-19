package gcp

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// metadataTokenPath is the suffix the library requests, spelled out here so a
// change to it fails a test rather than silently falling through to a 404 that
// looks like a network problem.
const metadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"

// fakeMetadata stands in for the GCE metadata server.
//
// One server serves the whole test binary because the library memoizes its
// "am I on GCE?" answer in a sync.Once, and the GCE_METADATA_HOST check that
// makes the answer yes sits inside that Once. A test that started its own
// server would be deciding, for every later test, whether the metadata path
// exists at all - and the outcome would depend on file and function ordering.
//
// So the address is fixed in TestMain and each test installs its own handler.
type fakeMetadata struct {
	mu      sync.Mutex
	handler http.HandlerFunc
	mints   int
}

var fakeMetadataServer = &fakeMetadata{}

func (f *fakeMetadata) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	handler := f.handler
	if r.URL.Path == metadataTokenPath {
		f.mints++
	}
	f.mu.Unlock()

	if handler == nil {
		http.Error(w, "no handler installed", http.StatusInternalServerError)
		return
	}
	handler(w, r)
}

// mintCount reports how many token requests the server has seen. Tests assert
// on this rather than on timing, which is the only way "the token was reused"
// is observable from outside the library.
func (f *fakeMetadata) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

// install replaces the handler and resets the counter for the duration of a test.
func (f *fakeMetadata) install(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	f.mu.Lock()
	f.handler = handler
	f.mints = 0
	f.mu.Unlock()
	t.Cleanup(func() {
		f.mu.Lock()
		f.handler = nil
		f.mu.Unlock()
	})
}

// refuseEverything installs a handler that mints nothing, and resets the
// counter so a later assertion of zero is about this test rather than about
// whatever ran before it. Use it where consulting the metadata server would
// mean the agent picked the wrong identity.
func (f *fakeMetadata) refuseEverything(t *testing.T) {
	t.Helper()
	f.install(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the metadata server must not be consulted here", http.StatusInternalServerError)
	})
}

// respondWithToken serves a valid metadata token response. expiresIn drives
// whether the library treats the result as fresh or as due for refresh.
func respondWithToken(value string, expiresIn int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadataTokenPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": value,
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
	}
}

func TestMain(m *testing.M) {
	server := httptest.NewServer(fakeMetadataServer)
	host := server.Listener.Addr().String()

	// Detection prefers a credential file over the metadata server, and one of
	// the places it looks is the gcloud well-known path under $HOME. On a
	// developer machine that file usually exists, so without this the tests
	// authenticate with a real identity and call Google - slow, dependent on who
	// is running them, and no longer testing the metadata path at all.
	//
	// Pointing HOME at an empty directory is blunt, but it is the only lever the
	// library offers: the well-known path is computed, not configurable.
	sandbox, err := os.MkdirTemp("", "axon-gcp-adc-test-home")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", sandbox); err != nil {
		panic(err)
	}
	if err := os.Setenv("APPDATA", sandbox); err != nil {
		panic(err)
	}
	if err := os.Unsetenv("GOOGLE_APPLICATION_CREDENTIALS"); err != nil {
		panic(err)
	}

	// Set before any test runs, and never unset: the library reads it once, on
	// the first OnGCE call in the process.
	if err := os.Setenv("GCE_METADATA_HOST", host); err != nil {
		panic(err)
	}

	code := m.Run()
	server.Close()
	_ = os.RemoveAll(sandbox)
	os.Exit(code)
}

// writeCredentialConfig writes an external-account credential configuration
// pointing at the given token endpoint, and returns its path. This is the
// workload-identity-federation shape a customer outside GCE deploys.
func writeCredentialConfig(t *testing.T, tokenURL string) string {
	t.Helper()
	dir := t.TempDir()

	subjectTokenPath := filepath.Join(dir, "subject-token")
	require.NoError(t, os.WriteFile(subjectTokenPath,
		[]byte("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJheG9uIn0.ZmFrZS1zaWduYXR1cmU"), 0600))

	config := map[string]any{
		"type":               "external_account",
		"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          tokenURL,
		"credential_source": map[string]any{
			"file": subjectTokenPath,
		},
	}
	body, err := json.Marshal(config)
	require.NoError(t, err)

	configPath := filepath.Join(dir, "credential-config.json")
	require.NoError(t, os.WriteFile(configPath, body, 0600))
	return configPath
}

// writeImpersonationConfig writes an external-account configuration that
// impersonates a service account after federating, and returns its path. This is
// the shape produced by a workload identity pool plus a dedicated service
// account: no key material, only endpoints and an audience.
func writeImpersonationConfig(t *testing.T, tokenURL, impersonationURL string) string {
	t.Helper()
	dir := t.TempDir()

	subjectTokenPath := filepath.Join(dir, "subject-token")
	require.NoError(t, os.WriteFile(subjectTokenPath,
		[]byte("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJheG9uIn0.ZmFrZS1zaWduYXR1cmU"), 0600))

	config := map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		"subject_token_type":                "urn:ietf:params:oauth:token-type:jwt",
		"token_url":                         tokenURL,
		"service_account_impersonation_url": impersonationURL,
		"credential_source": map[string]any{
			"file": subjectTokenPath,
		},
	}
	body, err := json.Marshal(config)
	require.NoError(t, err)

	configPath := filepath.Join(dir, "impersonation-config.json")
	require.NoError(t, os.WriteFile(configPath, body, 0600))
	return configPath
}

// useCredentialConfig points detection at a credential configuration file.
// GOOGLE_APPLICATION_CREDENTIALS is checked before the metadata probe, so a
// federation test is unaffected by the metadata host TestMain installed.
func useCredentialConfig(t *testing.T, path string) {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

// newSTSServer starts a fake token-exchange endpoint and returns its URL, for
// the credential configuration to name as its token_url.
func newSTSServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL + "/token"
}

// newSilentEndpoint returns a URL that accepts connections and then says
// nothing, which is how a timeout is provoked without touching a real network.
func newSilentEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var mu sync.Mutex
	var accepted []net.Conn

	// Registered before the goroutine starts: a Cleanup call from a goroutine
	// that outlives the test would panic.
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()
	return fmt.Sprintf("http://%s/token", ln.Addr().String())
}

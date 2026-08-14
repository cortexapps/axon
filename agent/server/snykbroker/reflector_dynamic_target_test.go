package snykbroker

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

type recordingBackend struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   int
	host   string
	header http.Header
}

func newRecordingBackend(t *testing.T, body string) *recordingBackend {
	return newRecordingBackendWith(t, body, httptest.NewServer)
}

func newRecordingTLSBackend(t *testing.T, body string) *recordingBackend {
	return newRecordingBackendWith(t, body, httptest.NewTLSServer)
}

func newRecordingBackendWith(t *testing.T, body string, start func(http.Handler) *httptest.Server) *recordingBackend {
	rb := &recordingBackend{}
	rb.server = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rb.mu.Lock()
		rb.hits++
		rb.host = r.Host
		rb.header = r.Header.Clone()
		rb.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	t.Cleanup(rb.server.Close)
	return rb
}

func (rb *recordingBackend) hostPort(t *testing.T) string {
	u, err := url.Parse(rb.server.URL)
	require.NoError(t, err)
	return u.Host
}

func (rb *recordingBackend) snapshot() (int, string, http.Header) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.hits, rb.host, rb.header
}

// newRoutedReflector builds a reflector whose transport resolves the given
// synthetic hostnames to local backends.
//
// Both origin kinds need this. A wildcard origin must be https and its hosts
// never exist in DNS; a concrete origin needs a real hostname before the
// header can agree with it, which an httptest server's 127.0.0.1 cannot
// provide. DialContext stands in for resolution, and name verification is
// skipped because the backends hold certificates for 127.0.0.1 rather than
// for the synthetic names. What these tests exercise is the origin policy and
// the routing path, not TLS naming.
func newRoutedReflector(t *testing.T, backends map[string]*recordingBackend) *RegistrationReflector {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			backend, ok := backends[host]
			if !ok {
				return nil, fmt.Errorf("no backend registered for %q", host)
			}
			return net.Dial(network, backend.hostPort(t))
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	rr := newReflectorWithDrain(t, RegistrationReflectorParams{
		Logger:    newTestLogger(t),
		Registry:  prometheus.NewRegistry(),
		Transport: transport,
		Config:    config.AgentConfig{ReflectorWebSocketUpgrade: true},
	})
	t.Cleanup(func() { rr.Stop() })
	return rr
}

func TestParseOriginAcceptsWildcardFamilies(t *testing.T) {
	for _, origin := range []string{
		"https://*.googleapis.com",
		"https://*.mtls.googleapis.com",
		"https://*.axon.example.com",
	} {
		_, wildcard, err := parseOrigin(origin)
		require.NoError(t, err, "origin=%q", origin)
		require.NotNil(t, wildcard, "origin=%q", origin)
	}
}

func TestParseOriginRejectsUnusableWildcards(t *testing.T) {
	cases := map[string]string{
		"plaintext":            "http://*.googleapis.com",
		"explicit port":        "https://*.googleapis.com:8443",
		"bare wildcard":        "https://*",
		"partial label":        "https://a*.googleapis.com",
		"non-leftmost":         "https://foo.*.googleapis.com",
		"two wildcards":        "https://*.*.googleapis.com",
		"public suffix":        "https://*.com",
		"multipart public sfx": "https://*.co.uk",
		"empty suffix":         "https://*.",
	}
	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseOrigin(origin)
			require.Error(t, err)
		})
	}
}

func TestParseOriginLeavesConcreteOriginsAlone(t *testing.T) {
	for _, origin := range []string{
		"https://bigquery.googleapis.com",
		"http://127.0.0.1:8080",
	} {
		asURL, wildcard, err := parseOrigin(origin)
		require.NoError(t, err, "origin=%q", origin)
		require.Nil(t, wildcard, "origin=%q", origin)
		require.NotNil(t, asURL)
	}
}

// The wildcard covers one label, so the deliberately-excluded
// "<service>.mtls.googleapis.com" family cannot slip in under "*.googleapis.com".
func TestWildcardMatchesExactlyOneLabel(t *testing.T) {
	_, wildcard, err := parseOrigin("https://*.googleapis.com")
	require.NoError(t, err)

	for _, host := range []string{
		"compute.googleapis.com",
		"us-central1-aiplatform.googleapis.com",
	} {
		require.True(t, wildcard.matches(host), "host=%q", host)
	}

	for _, host := range []string{
		"a.b.googleapis.com",
		"bigquery.mtls.googleapis.com",
		"googleapis.com",
		"evilgoogleapis.com",
		"notgoogleapis.com",
		"compute.googleapis.com.evil.com",
		"evil.com",
	} {
		require.False(t, wildcard.matches(host), "host=%q", host)
	}
}

func TestParseTargetHostNormalizesAndRejects(t *testing.T) {
	host, err := parseTargetHost("COMPUTE.GoogleAPIs.com")
	require.NoError(t, err)
	require.Equal(t, "compute.googleapis.com", host)

	cases := map[string]string{
		"empty":            "",
		"comma joined":     "a.googleapis.com,b.googleapis.com",
		"leading space":    " compute.googleapis.com",
		"inner space":      "compute googleapis.com",
		"tab":              "compute.googleapis.com\t",
		"port":             "compute.googleapis.com:8443",
		"url":              "https://compute.googleapis.com",
		"path":             "compute.googleapis.com/v1",
		"user info":        "user@compute.googleapis.com",
		"wildcard":         "*.googleapis.com",
		"ipv4":             "127.0.0.1",
		"ipv6":             "::1",
		"unicode":          "compute.googleapıs.com",
		"trailing dot":     "compute.googleapis.com.",
		"leading dot":      ".googleapis.com",
		"empty inner":      "compute..googleapis.com",
		"hyphen bounded":   "-compute.googleapis.com",
		"underscore":       "compute_x.googleapis.com",
		"percent encoding": "compute%2egoogleapis.com",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseTargetHost(value)
			require.Error(t, err)
		})
	}
}

// The spec's routing table, as a unit: fail-closed in both directions.
func TestResolveTargetHostFailsClosedBothDirections(t *testing.T) {
	wildcardEntry, err := newProxyEntry("https://*.googleapis.com", false, 1234, nil, nil)
	require.NoError(t, err)
	concreteEntry, err := newProxyEntry("https://bigquery.googleapis.com", false, 1234, nil, nil)
	require.NoError(t, err)

	cases := []struct {
		name    string
		entry   *proxyEntry
		values  []string
		want    string
		wantErr bool
	}{
		{"wildcard, inside policy", wildcardEntry, []string{"compute.googleapis.com"}, "compute.googleapis.com", false},
		{"wildcard, outside policy", wildcardEntry, []string{"evil.example.com"}, "", true},
		{"wildcard, absent", wildcardEntry, nil, "", true},
		{"wildcard, duplicated", wildcardEntry, []string{"a.googleapis.com", "b.googleapis.com"}, "", true},
		{"concrete, absent", concreteEntry, nil, "", false},
		{"concrete, equal", concreteEntry, []string{"bigquery.googleapis.com"}, "", false},
		{"concrete, equal but cased", concreteEntry, []string{"BigQuery.GoogleAPIs.com"}, "", false},
		{"concrete, disagreeing", concreteEntry, []string{"compute.googleapis.com"}, "", true},
		{"concrete, duplicated", concreteEntry, []string{"bigquery.googleapis.com", "bigquery.googleapis.com"}, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.entry.resolveTargetHost(tc.values)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestWildcardRetargetsAcrossHosts(t *testing.T) {
	backendA := newRecordingTLSBackend(t, "a")
	backendB := newRecordingTLSBackend(t, "b")
	rr := newRoutedReflector(t, map[string]*recordingBackend{
		"a.axon.example.com": backendA,
		"b.axon.example.com": backendB,
	})

	headers := acceptfile.NewResolverMapFromMap(map[string]string{"Authorization": "Bearer minted"})
	proxyURI := rr.ProxyURI("https://*.axon.example.com", WithHeadersResolver(headers))
	path := proxyPath(t, proxyURI)

	for _, tc := range []struct{ host, want string }{
		{"a.axon.example.com", "a"},
		{"b.axon.example.com", "b"},
	} {
		req := httptest.NewRequest("GET", path+"/v1/things", nil)
		req.Header.Set(HeaderTargetHost, tc.host)
		rec := httptest.NewRecorder()
		rr.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "host=%s", tc.host)
		require.Equal(t, tc.want, rec.Body.String())
	}

	_, hostA, _ := backendA.snapshot()
	hitsB, hostB, headerB := backendB.snapshot()
	require.Equal(t, "a.axon.example.com", hostA)
	require.Equal(t, "b.axon.example.com", hostB)
	require.Equal(t, 1, hitsB)
	// rule headers are still injected on a retargeted request
	require.Equal(t, "Bearer minted", headerB.Get("Authorization"))
	// the routing header never reaches the upstream
	require.Empty(t, headerB.Get(HeaderTargetHost))
}

// No fallback: a wildcard rule has no destination of its own.
func TestWildcardWithoutTargetHostIsRejected(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://*.axon.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil))

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), ErrClassDestinationRejected)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

func TestWildcardOutsidePolicyIsRejected(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://*.axon.example.com")
	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "evil.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), ErrClassDestinationRejected)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

func TestWildcardRefusesWebSocketUpgrade(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://*.axon.example.com")
	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/socket", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

// A concrete rule is the common case, and it must strip the header even though
// it never routes on it - otherwise the value reaches a third-party upstream.
func TestConcreteOriginStripsTargetHostAndRejectsDisagreement(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://a.axon.example.com")
	path := proxyPath(t, proxyURI)

	// Restating the declared authority is allowed, and is still stripped.
	req := httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "a.axon.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	hits, _, header := backend.snapshot()
	require.Equal(t, 1, hits)
	require.Empty(t, header.Get(HeaderTargetHost))

	// Disagreeing is a defect or a probe, never a legitimate request.
	req = httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "compute.googleapis.com")
	rec = httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), ErrClassDestinationRejected)

	hits, _, _ = backend.snapshot()
	require.Equal(t, 1, hits, "the rejected request must not reach the upstream")
}

// A rule with no header at all is the overwhelmingly common case and must be
// entirely unaffected.
func TestConcreteOriginWithoutTargetHostIsUntouched(t *testing.T) {
	env := newTestReflectorEnv(t)
	backend := newRecordingBackend(t, "a")

	proxyURI := env.Reflector.ProxyURI(backend.server.URL)
	rec := httptest.NewRecorder()
	env.Reflector.ServeHTTP(rec, httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "a", rec.Body.String())
	hits, _, header := backend.snapshot()
	require.Equal(t, 1, hits)
	require.Empty(t, header.Get(HeaderTargetHost))
}

func TestDuplicateTargetHostIsRejected(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://a.axon.example.com")
	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
	req.Header.Add(HeaderTargetHost, "a.axon.example.com")
	req.Header.Add(HeaderTargetHost, "a.axon.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

func TestWildcardConcurrentRetargets(t *testing.T) {
	backendA := newRecordingTLSBackend(t, "a")
	backendB := newRecordingTLSBackend(t, "b")
	rr := newRoutedReflector(t, map[string]*recordingBackend{
		"a.axon.example.com": backendA,
		"b.axon.example.com": backendB,
	})

	proxyURI := rr.ProxyURI("https://*.axon.example.com")
	path := proxyPath(t, proxyURI)

	const workers = 16
	const perWorker = 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				host, want := "a.axon.example.com", "a"
				if (w+i)%2 == 1 {
					host, want = "b.axon.example.com", "b"
				}
				req := httptest.NewRequest("GET", path+"/v1/things", nil)
				req.Header.Set(HeaderTargetHost, host)
				rec := httptest.NewRecorder()
				rr.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || rec.Body.String() != want {
					errs <- fmt.Errorf("worker %d req %d: got code=%d body=%q want %q", w, i, rec.Code, rec.Body.String(), want)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	hitsA, _, _ := backendA.snapshot()
	hitsB, _, _ := backendB.snapshot()
	require.Equal(t, workers*perWorker/2, hitsA)
	require.Equal(t, workers*perWorker/2, hitsB)
}

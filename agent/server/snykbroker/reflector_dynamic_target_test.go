package snykbroker

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// newRoutedReflector resolves synthetic hostnames to local backends. Wildcard
// hosts never exist in DNS, and a concrete origin needs a real hostname before
// a target can agree with it, which httptest's 127.0.0.1 cannot provide.
// Verification is skipped because the backends' certificates do not carry the
// synthetic names.
func newRoutedReflector(t *testing.T, backends map[string]*recordingBackend) *RegistrationReflector {
	t.Helper()
	transport := routedTransport(t, backends)
	rr := newReflectorWithDrain(t, RegistrationReflectorParams{
		Logger:    newTestLogger(t),
		Registry:  prometheus.NewRegistry(),
		Transport: transport,
		Config:    config.AgentConfig{ReflectorWebSocketUpgrade: true},
	})
	t.Cleanup(func() { rr.Stop() })
	return rr
}

// routedTransport is the DNS stand-in on its own, for tests that also need to
// reach a backend without going through the reflector.
func routedTransport(t *testing.T, backends map[string]*recordingBackend) *http.Transport {
	t.Helper()
	return &http.Transport{
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
}

func TestParseOriginAcceptsWildcardFamilies(t *testing.T) {
	for _, origin := range []string{
		"https://*.api.example.net",
		"https://*.internal.api.example.net",
		"https://*.axon.example.com",
		"https://*.api.example.net:8443",
		"https://*.something.com.internal:8443",
	} {
		_, wildcard, err := parseOrigin(origin)
		require.NoError(t, err, "origin=%q", origin)
		require.NotNil(t, wildcard, "origin=%q", origin)
	}
}

func TestParseOriginRejectsUnusableWildcards(t *testing.T) {
	cases := map[string]string{
		"plaintext":            "http://*.api.example.net",
		"bare wildcard":        "https://*",
		"partial label":        "https://a*.api.example.net",
		"non-leftmost":         "https://foo.*.api.example.net",
		"two wildcards":        "https://*.*.api.example.net",
		"public suffix":        "https://*.com",
		"multipart public sfx": "https://*.co.uk",
		"empty suffix":         "https://*.",
		"port zero":            "https://*.api.example.net:0",
		"port out of range":    "https://*.api.example.net:70000",
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
		"https://beta.api.example.net",
		"http://127.0.0.1:8080",
	} {
		asURL, wildcard, err := parseOrigin(origin)
		require.NoError(t, err, "origin=%q", origin)
		require.Nil(t, wildcard, "origin=%q", origin)
		require.NotNil(t, asURL)
	}
}

// A wildcard certificate covers a single label, so a multi-label match would
// authorize names that verification then refuses. If this fails, the fix is
// not to relax it.
func TestWildcardMatchesExactlyOneLabel(t *testing.T) {
	_, wildcard, err := parseOrigin("https://*.api.example.net")
	require.NoError(t, err)

	for _, host := range []string{
		"alpha.api.example.net",
		"eu-west1-compute.api.example.net",
	} {
		require.True(t, wildcard.matches(host), "host=%q", host)
	}

	for _, host := range []string{
		"a.b.api.example.net",
		"svc.internal.api.example.net",
		"api.example.net",
		"evilapi.example.net",
		"notapi.example.net",
		"alpha.api.example.net.evil.com",
		"evil.com",
	} {
		require.False(t, wildcard.matches(host), "host=%q", host)
	}
}

// parseTargetHost is not a hostname validator, so this covers only what it
// still claims: the shapes that would confuse the origin match or dial
// something other than a host. Everything else is the match's job, and
// TestPolicyRefusesValuesOutsideTheFamily covers that.
func TestParseTargetHostNormalizesAndRejects(t *testing.T) {
	host, err := parseTargetHost("ALPHA.API.Example.NET")
	require.NoError(t, err)
	require.Equal(t, "alpha.api.example.net", host)

	cases := map[string]string{
		"empty":         "",
		"comma joined":  "alpha.api.example.net,beta.api.example.net",
		"leading space": " alpha.api.example.net",
		"inner space":   "alpha api.example.net",
		"tab":           "alpha.api.example.net\t",
		"carriage":      "alpha.api.example.net\r\nX-Evil: y",
		"default port":  "alpha.api.example.net:443",
		"other port":    "alpha.api.example.net:8443",
		"ipv4":          "127.0.0.1",
		"ipv6":          "::1",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseTargetHost(value)
			require.Error(t, err)
		})
	}
}

// Ordinary hostnames must reach the dial. The double-hyphen cases are here on
// purpose: they are valid DNS but idna.Lookup rejects a hyphen in label
// positions 3-4, which IDNA reserves for "xn--". An earlier revision of this
// change validated through that profile and would have refused a real
// destination. If a strict validator ever comes back, this is what catches it.
func TestPolicyAdmitsOrdinaryHostnames(t *testing.T) {
	entry, err := newProxyEntry("https://*.example.net", false, 1234, nil, nil)
	require.NoError(t, err)

	for _, value := range []string{
		"alpha.example.net",
		"eu-west1-compute.example.net",
		"my-service-01.example.net",
		"ab--cd.example.net",
		"x1--y.example.net",
		"a--b.example.net",
		"1.example.net",
		"xn--e1afmkfd.example.net",
		strings.Repeat("a", 63) + ".example.net",
	} {
		host, err := entry.resolveTargetHost([]string{value})
		require.NoError(t, err, "value=%q", value)
		require.Equal(t, value, host, "value=%q", value)
	}
}

// The origin match is the destination control, not parseTargetHost, so these
// have to be refused by the policy however well-formed they look.
func TestPolicyRefusesValuesOutsideTheFamily(t *testing.T) {
	entry, err := newProxyEntry("https://*.api.example.net", false, 1234, nil, nil)
	require.NoError(t, err)

	cases := map[string]string{
		"other family":      "alpha.evil.example.net",
		"suffix as prefix":  "alpha.api.example.net.evil.com",
		"parent of family":  "api.example.net",
		"partial label":     "evilapi.example.net",
		"two labels":        "a.b.api.example.net",
		"nested family":     "svc.internal.api.example.net",
		"bare suffix":       ".api.example.net",
		"empty inner label": "alpha..api.example.net",
		"trailing dot":      "alpha.api.example.net.",
		"path appended":     "alpha.api.example.net/v1",
		"url":               "https://alpha.api.example.net",
		"percent encoded":   "alpha%2eapi.example.net",
		"unicode label":     "alpha.api.exampl\u0131.net",
		"over-long label":   strings.Repeat("a", 64) + ".example.net",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := entry.resolveTargetHost([]string{value})
			require.Error(t, err)
		})
	}
}

func TestResolveTargetHostFailsClosedBothDirections(t *testing.T) {
	wildcardEntry, err := newProxyEntry("https://*.api.example.net", false, 1234, nil, nil)
	require.NoError(t, err)
	concreteEntry, err := newProxyEntry("https://beta.api.example.net", false, 1234, nil, nil)
	require.NoError(t, err)

	cases := []struct {
		name    string
		entry   *proxyEntry
		values  []string
		want    string
		wantErr bool
	}{
		{"wildcard, inside policy", wildcardEntry, []string{"alpha.api.example.net"}, "alpha.api.example.net", false},
		{"wildcard, outside policy", wildcardEntry, []string{"evil.example.com"}, "", true},
		{"wildcard, absent", wildcardEntry, nil, "", true},
		{"wildcard, duplicated", wildcardEntry, []string{"alpha.api.example.net", "beta.api.example.net"}, "", true},
		{"concrete, absent", concreteEntry, nil, "", false},
		{"concrete, equal", concreteEntry, []string{"beta.api.example.net"}, "", false},
		{"concrete, equal but cased", concreteEntry, []string{"Beta.API.Example.NET"}, "", false},
		{"concrete, disagreeing", concreteEntry, []string{"alpha.api.example.net"}, "", true},
		{"concrete, duplicated", concreteEntry, []string{"beta.api.example.net", "beta.api.example.net"}, "", true},
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
	require.Equal(t, "Bearer minted", headerB.Get("Authorization"))
	require.Empty(t, headerB.Get(HeaderTargetHost))
}

// The header names a host, so the port has to come from the origin. If it did
// not, a family on a non-default port would silently dial 443.
func TestWildcardOriginPortReachesTheDial(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	var dialed []string
	var mu sync.Mutex
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
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

	proxyURI := rr.ProxyURI("https://*.something.com.internal:8443")
	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "alpha.something.com.internal")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"alpha.something.com.internal:8443"}, dialed)
}

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

// A concrete rule never routes on the header but must still strip it, or the
// value reaches a third-party upstream.
func TestConcreteOriginStripsTargetHostAndRejectsDisagreement(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://a.axon.example.com")
	path := proxyPath(t, proxyURI)

	req := httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "a.axon.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	hits, _, header := backend.snapshot()
	require.Equal(t, 1, hits)
	require.Empty(t, header.Get(HeaderTargetHost))

	req = httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "alpha.api.example.net")
	rec = httptest.NewRecorder()
	rr.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), ErrClassDestinationRejected)

	hits, _, _ = backend.snapshot()
	require.Equal(t, 1, hits, "the rejected request must not reach the upstream")
}

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

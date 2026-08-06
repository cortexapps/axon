package snykbroker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
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
	rb := &recordingBackend{}
	rb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestDynamicTargetRetargetsAcrossHosts(t *testing.T) {
	env := newTestReflectorEnv(t)
	backendA := newRecordingBackend(t, "a")
	backendB := newRecordingBackend(t, "b")

	headers := acceptfile.NewResolverMapFromMap(map[string]string{"Authorization": "Bearer minted"})
	proxyURI := env.Reflector.ProxyURI(backendA.server.URL,
		WithHeadersResolver(headers),
		WithDynamicTargetHosts("127.0.0.1"),
	)
	path := proxyPath(t, proxyURI)

	// no header: goes to the baked-in origin
	rec := httptest.NewRecorder()
	env.Reflector.ServeHTTP(rec, httptest.NewRequest("GET", path+"/v1/things", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "a", rec.Body.String())

	// header: retargets the same entry to backend B
	req := httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderRelayTargetHost, backendB.hostPort(t))
	rec = httptest.NewRecorder()
	env.Reflector.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "b", rec.Body.String())

	hitsA, hostA, _ := backendA.snapshot()
	hitsB, hostB, headerB := backendB.snapshot()
	require.Equal(t, 1, hitsA)
	require.Equal(t, backendA.hostPort(t), hostA)
	require.Equal(t, 1, hitsB)
	require.Equal(t, backendB.hostPort(t), hostB)
	// rule headers still injected on the retargeted request
	require.Equal(t, "Bearer minted", headerB.Get("Authorization"))
	// the routing header never reaches the upstream
	require.Empty(t, headerB.Get(HeaderRelayTargetHost))
}

func TestDynamicTargetFailsClosedOnDisallowedHost(t *testing.T) {
	env := newTestReflectorEnv(t)
	backend := newRecordingBackend(t, "a")

	proxyURI := env.Reflector.ProxyURI(backend.server.URL, WithDynamicTargetHosts("*.googleapis.com"))
	path := proxyPath(t, proxyURI)

	req := httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderRelayTargetHost, "evil.example.com")
	rec := httptest.NewRecorder()
	env.Reflector.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

func TestDynamicTargetHeaderIgnoredWithoutOptIn(t *testing.T) {
	env := newTestReflectorEnv(t)
	backend := newRecordingBackend(t, "a")
	other := newRecordingBackend(t, "b")

	proxyURI := env.Reflector.ProxyURI(backend.server.URL)
	path := proxyPath(t, proxyURI)

	req := httptest.NewRequest("GET", path+"/v1/things", nil)
	req.Header.Set(HeaderRelayTargetHost, other.hostPort(t))
	rec := httptest.NewRecorder()
	env.Reflector.ServeHTTP(rec, req)

	// request goes to the baked-in origin, header passes through untouched
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "a", rec.Body.String())
	hits, _, header := backend.snapshot()
	require.Equal(t, 1, hits)
	require.Equal(t, other.hostPort(t), header.Get(HeaderRelayTargetHost))
	otherHits, _, _ := other.snapshot()
	require.Equal(t, 0, otherHits)
}

func TestDynamicTargetRefusesWebSocketUpgrade(t *testing.T) {
	env := newTestReflectorEnv(t)
	backend := newRecordingBackend(t, "a")

	proxyURI := env.Reflector.ProxyURI(backend.server.URL, WithDynamicTargetHosts("127.0.0.1"))
	path := proxyPath(t, proxyURI)

	req := httptest.NewRequest("GET", path+"/socket", nil)
	req.Header.Set(HeaderRelayTargetHost, backend.hostPort(t))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	env.Reflector.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

func TestResolveDynamicHost(t *testing.T) {
	entry := &proxyEntry{dynamicTargetHosts: []string{"*.googleapis.com", "bigquery.googleapis.com", "127.0.0.1"}}

	cases := []struct {
		requested string
		want      string
		ok        bool
	}{
		{"compute.googleapis.com", "compute.googleapis.com", true},
		{"us-central1-aiplatform.googleapis.com", "us-central1-aiplatform.googleapis.com", true},
		{"COMPUTE.GoogleAPIs.com", "compute.googleapis.com", true},
		{"bigquery.googleapis.com", "bigquery.googleapis.com", true},
		{"127.0.0.1:8443", "127.0.0.1:8443", true},
		{"a.b.googleapis.com", "a.b.googleapis.com", true},
		// dot-boundary and shape violations
		{"googleapis.com", "", false},
		{"notgoogleapis.com", "", false},
		{"evilgoogleapis.com", "", false},
		{"evil.com", "", false},
		{"compute.googleapis.com.evil.com", "", false},
		{"compute.googleapis.com/path", "", false},
		{"user@compute.googleapis.com", "", false},
		{"compute.googleapis.com:notaport", "", false},
		{"compute.googleapis.com:", "", false},
		{"", "", false},
		{"   ", "", false},
	}

	for _, tc := range cases {
		got, ok := entry.resolveDynamicHost(tc.requested)
		require.Equal(t, tc.ok, ok, "requested=%q", tc.requested)
		require.Equal(t, tc.want, got, "requested=%q", tc.requested)
	}
}

func TestDynamicTargetConcurrentRetargets(t *testing.T) {
	env := newTestReflectorEnv(t)
	backendA := newRecordingBackend(t, "a")
	backendB := newRecordingBackend(t, "b")

	proxyURI := env.Reflector.ProxyURI(backendA.server.URL, WithDynamicTargetHosts("127.0.0.1"))
	path := proxyPath(t, proxyURI)
	hostA := backendA.hostPort(t)
	hostB := backendB.hostPort(t)

	const workers = 16
	const perWorker = 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				target, want := hostA, "a"
				if (w+i)%2 == 1 {
					target, want = hostB, "b"
				}
				req := httptest.NewRequest("GET", path+"/v1/things", nil)
				req.Header.Set(HeaderRelayTargetHost, target)
				rec := httptest.NewRecorder()
				env.Reflector.ServeHTTP(rec, req)
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

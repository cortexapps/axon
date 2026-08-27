package snykbroker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/require"
)

// The class travels in a header as well as the body. A body can come from an
// upstream, so a caller that has to tell an agent failure from a Google one
// cannot rely on the body alone.

func TestDestinationRejectionNamesItselfInAHeader(t *testing.T) {
	backend := newRecordingTLSBackend(t, "a")
	rr := newRoutedReflector(t, map[string]*recordingBackend{"a.axon.example.com": backend})

	proxyURI := rr.ProxyURI("https://*.axon.example.com")
	req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
	req.Header.Set(HeaderTargetHost, "evil.example.com")
	rec := httptest.NewRecorder()
	rr.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, ErrClassDestinationRejected, rec.Header().Get(HeaderFailureClass))
	hits, _, _ := backend.snapshot()
	require.Equal(t, 0, hits)
}

func TestCredentialProviderFailureNamesTheAgent(t *testing.T) {
	dialed := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed++
	}))
	defer backend.Close()

	headers := acceptfile.ResolverMap{
		"authorization": {
			Key: "${plugin:broken}",
			Resolve: func() (string, error) {
				return "", fmt.Errorf("provider exited 1")
			},
		},
	}
	entry, err := newProxyEntry(backend.URL, false, 8080, headers, nil)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	entry.serve(rec, httptest.NewRequest("GET", "/test", nil))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, ErrClassCredentialFailure, rec.Header().Get(HeaderFailureClass))
	// A provider failure that reached the upstream would come back as its 401,
	// which is a different class and a different thing to go and fix.
	require.Equal(t, 0, dialed)
}

func TestUnreachableUpstreamNamesTheAgentsNetwork(t *testing.T) {
	// Port 1 is reserved and nothing listens on it, so the dial fails locally.
	entry, err := newProxyEntry("http://127.0.0.1:1", false, 8080, nil, nil)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	entry.serve(rec, httptest.NewRequest("GET", "/test", nil))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, ErrClassNetworkFailure, rec.Header().Get(HeaderFailureClass))
}

func TestUpstreamCannotNameAFailureClass(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderFailureClass, ErrClassDestinationRejected)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	entry, err := newProxyEntry(backend.URL, false, 8080, nil, nil)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	entry.serve(rec, httptest.NewRequest("GET", "/test", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get(HeaderFailureClass))
}

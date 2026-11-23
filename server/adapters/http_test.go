package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The agent must get the path as the caller sent it, as with snyk-broker.
// ServeMux cleans a path with "//", "/./" or "/../" and answers with a 301
// to a relative Location, which the dispatcher cannot follow.
func TestBrokerDispatch_ForwardsPathAsSent(t *testing.T) {
	paths := map[string]string{
		"clean":        "/repos/o/r/contents/Jenkinsfile",
		"double slash": "/repos/o/r/contents//Jenkinsfile",
		"dot":          "/repos/o/r/contents/./Jenkinsfile",
		"dot dot":      "/repos/o/r/../r/contents/Jenkinsfile",
		"query":        "/repos/o/r/contents//Jenkinsfile?ref=main",
	}
	entryPoints := map[string]func(h *HttpAdapter) http.Handler{
		"adapter": func(h *HttpAdapter) http.Handler { return h },
		// The cmd/main.go shape: the adapter in front of the server's ServeMux.
		"wrapped mux": func(h *HttpAdapter) http.Handler {
			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})
			return h.Wrap(mux)
		},
	}
	for entryName, entryPoint := range entryPoints {
		for name, path := range paths {
			t.Run(entryName+"/"+name, func(t *testing.T) {
				fh := newFailureHarness(t, 0)
				handler := entryPoint(fh.h)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					req := httptest.NewRequest(http.MethodGet, "/broker/"+failureToken+path, nil).WithContext(ctx)
					handler.ServeHTTP(httptest.NewRecorder(), req)
				}()

				start := fh.next(t, isStart).GetStart()
				require.Equal(t, path, start.GetPseudoHeaders()[":path"])
				cancel()
				<-done
			})
		}
	}
}

func TestConnectionStatusToken(t *testing.T) {
	for _, tc := range []struct {
		path  string
		token string
		ok    bool
	}{
		{"/connection-status/tok", "tok", true},
		{"/broker/connection-status/tok", "tok", true},
		{"/connection-status/66fa3dbe-46fa-4949-9713-89a2ead1e7c2", "66fa3dbe-46fa-4949-9713-89a2ead1e7c2", true},
		{"/broker/connection-status/0961c6399152f7cc71c525cfedf37c265008ed00183a6b9ae15ce70beb452b5a",
			"0961c6399152f7cc71c525cfedf37c265008ed00183a6b9ae15ce70beb452b5a", true},

		// No token, or more than one segment after the prefix.
		{"/connection-status/", "", false},
		{"/connection-status", "", false},
		{"/broker/connection-status/", "", false},
		{"/connection-status/tok/", "", false},
		{"/connection-status/tok/extra", "", false},
		{"/connection-status//tok", "", false},

		// Other prefixes. /broker/<token>/... is dispatch, not connection status.
		{"/broker/tok/connection-status/x", "", false},
		{"/broker//connection-status/tok", "", false},
		{"/x/connection-status/tok", "", false},
		{"/connection-statusx/tok", "", false},
		{"/broker/connection-statusx/tok", "", false},
		{"connection-status/tok", "", false},
		{"/broker/tok/repos/o/r/contents//Jenkinsfile", "", false},
	} {
		token, ok := connectionStatusToken(tc.path)
		require.Equal(t, tc.ok, ok, tc.path)
		require.Equal(t, tc.token, token, tc.path)
	}
}

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
		// cmd/main.go mounts the adapter on an outer ServeMux.
		"outer mux": func(h *HttpAdapter) http.Handler {
			outer := http.NewServeMux()
			outer.Handle("/broker/", h)
			return h.Wrap(outer)
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

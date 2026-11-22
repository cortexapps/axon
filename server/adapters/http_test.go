package adapters

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The agent must get the path as the caller sent it, as with snyk-broker.
// ServeMux cleans a path with "//", "/./" or "/../" and answers with a 301
// to a relative Location, which the dispatcher cannot follow.
func TestBrokerDispatch_ForwardsPathAsSent(t *testing.T) {
	for name, path := range map[string]string{
		"clean":        "/repos/o/r/contents/Jenkinsfile",
		"double slash": "/repos/o/r/contents//Jenkinsfile",
		"dot":          "/repos/o/r/contents/./Jenkinsfile",
		"dot dot":      "/repos/o/r/../r/contents/Jenkinsfile",
		"query":        "/repos/o/r/contents//Jenkinsfile?ref=main",
	} {
		t.Run(name, func(t *testing.T) {
			fh := newFailureHarness(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- fh.serve(ctx, path) }()

			start := fh.next(t, isStart).GetStart()
			require.Equal(t, path, start.GetPseudoHeaders()[":path"])
			cancel()
			<-done
		})
	}
}

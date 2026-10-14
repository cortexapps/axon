package grpctunnel

import (
	"net/http"
	"strings"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/test/httpfake"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// HttpBackend hands the raw response body up to the call handler, which
// streams it out as frames under a context the handler owns and cancels when
// the call is done. A net/http body stops reading once that context ends, so
// the whole body has to reach EOF, and be closed, before the cancel. This is
// the shape of the bug #165 fixed in the Cortex API path; the tunnel path was
// never covered because every other test stubs the Backend interface above
// the HTTP client.
func TestHttpBackend_StreamsWholeBodyAndClosesItBeforeCallContextEnds(t *testing.T) {
	cfg := config.AgentConfig{MaxInflightRequests: 4}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})

	// Several frames' worth, so the read loop has to keep going after the
	// first chunk rather than getting the whole body in one Read.
	body := strings.Repeat("x", 3*maxChunkSize+7)
	client, tr := httpfake.Client(httpfake.Response(http.StatusOK, body))
	tc.backend = NewHttpBackend(client, zap.NewNop())

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/thing", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	frames := fc.byCall("c1")
	var got []byte
	for _, f := range frames {
		require.Nil(t, f.GetCancel(), "call was cancelled: %s", f.GetCancel().GetReason())
		if d := f.GetData(); d != nil {
			got = append(got, d.Payload...)
		}
	}
	require.Equal(t, len(body), len(got), "body was not streamed in full")
	require.NotNil(t, frames[len(frames)-1].GetEnd(), "call did not end with an End frame")

	require.Len(t, tr.Bodies, 1)
	require.True(t, tr.Bodies[0].EOF, "body was not read to EOF")

	// Close runs in the call goroutine's defers after the End frame goes out,
	// and table.remove is the outermost of those, so an empty table means the
	// close has happened.
	waitFor(t, 5*time.Second, func() bool { return !table.active() })
	require.True(t, tr.Bodies[0].Closed, "body was not closed")
}

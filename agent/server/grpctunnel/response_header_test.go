package grpctunnel

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snyk-broker stamps x-axon-relay-instance through the reflector on the way
// out. The tunnel routes upstream traffic directly and never touches the
// reflector, so it has to add the header itself — and for a while it did not.
// No test caught it because the two transports were running different
// end-to-end suites, and the gRPC one had quietly lost this assertion.
func TestResponse_CarriesRelayInstanceHeader(t *testing.T) {
	cfg := config.AgentConfig{MaxInflightRequests: 4, InstanceId: "agent-7"}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})

	tc.backend = backendFunc(func(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
		return &BackendResponse{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(bytes.NewReader([]byte("ok"))),
			Trailer:    func() http.Header { return nil },
		}, nil
	})

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/thing", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	start := fc.byCall("c1")[0].GetStart()
	require.NotNil(t, start)
	assert.Equal(t, "agent-7", start.Headers["x-axon-relay-instance"],
		"the response must identify which agent in a pool served it")
	assert.Equal(t, "text/plain", start.Headers["content-type"],
		"upstream headers still pass through untouched")
}

// An agent with no instance id configured should not advertise an empty one.
func TestResponse_OmitsRelayInstanceHeaderWhenUnset(t *testing.T) {
	cfg := config.AgentConfig{MaxInflightRequests: 4}
	tc, _ := newTestClient(t, cfg, &fakeRegistration{serverURI: "x", tokens: []string{"x"}})
	tc.config.InstanceId = ""

	tc.backend = backendFunc(func(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
		return &BackendResponse{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Trailer:    func() http.Header { return nil },
		}, nil
	})

	fc := newFrameCollector()
	sc := newTestStreamCtx(fc.sendFn)
	table := newTestCallTable(t)

	tc.startCall(sc, table, "c1", reqStart("GET", "/thing", 0))
	tc.handleCallFrame(sc, table, &pb.CallFrame{CallId: "c1", Body: &pb.CallFrame_End{End: &pb.CallEnd{}}})
	fc.waitDone(t, 1)

	_, present := fc.byCall("c1")[0].GetStart().Headers["x-axon-relay-instance"]
	assert.False(t, present)
}

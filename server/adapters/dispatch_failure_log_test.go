package adapters

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// The request-body failure used to make Dispatch return (nil, nil), which
// panicked the handler on resp.Body. It now fails like any other dispatch.
func TestDispatchFailure_RequestBodyFailureDoesNotPanic(t *testing.T) {
	logger := zap.NewNop()
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{DispatchTimeout: 5 * time.Second}
	m := metrics.New("test")
	h := NewHttpAdapter(cfg, registry, dispatch.NewDispatcher(cfg, registry, m, logger), m, logger)
	mustRegister(t, registry, broker.NewToken("tok"), tunnel.ClientIdentity{InstanceID: "agent-7"},
		&tunnel.StreamHandle{StreamID: "s1", Send: func(*pb.ServerFrame) error { return nil }})

	req := httptest.NewRequest(http.MethodPost, "/broker/tok/upload", io.MultiReader(strings.NewReader("abc"), failingBody{}))
	w := httptest.NewRecorder()
	require.NotPanics(t, func() { h.ServeHTTP(w, req) })
	require.Equal(t, http.StatusBadGateway, w.Code)
}

package adapters

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The dispatcher's liveness probe is the only thing standing between a
// connected agent and being erased from the routing index: any non-200 is
// read as "no clients here", and both verifyClientLiveness and
// bootstrapServerIndex act on that. It asks at the root, so the root has to
// answer.
func TestConnectionStatus_AnsweredAtBothPaths(t *testing.T) {
	logger := zap.NewNop()
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{}
	h := NewHttpAdapter(cfg, registry, nil, metrics.New("test"), logger)

	const rawToken = "66fa3dbe-46fa-4949-9713-89a2ead1e7c2"
	token := broker.NewToken(rawToken)

	// Unknown token: 404 from both, so a genuinely absent client still reads
	// as absent.
	for _, path := range []string{"/connection-status/", "/broker/connection-status/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+rawToken, nil))
		require.Equal(t, http.StatusNotFound, w.Code, "path %s, no client registered", path)
	}

	// Register a stream for the token, the way a connected agent does.
	require.NoError(t, registry.Register(token,
		tunnel.ClientIdentity{InstanceID: "axon-test", TenantID: "1"},
		&tunnel.StreamHandle{StreamID: "stream-1", Send: func(*pb.ServerFrame) error { return nil }},
	))

	for _, path := range []string{"/connection-status/", "/broker/connection-status/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path+rawToken, nil))
		require.Equal(t, http.StatusOK, w.Code, "path %s, client registered", path)
		require.JSONEq(t, `{"ok":true}`, w.Body.String())
	}
}

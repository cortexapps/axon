package adapters

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These are the literal values RequestForwardingService.isBrokerUnknownTokenResponse
// compares against, copied from relay-dispatcher rather than computed, so that
// changing how we generate them cannot quietly change what we emit:
//
//	if (response.code != 404) return false
//	if (response.headers["ETag"] != Broker404ETag) return false
//	return peekedBody.string() == Broker404Body
const (
	dispatcherBroker404ETag = `W/"c-NsyIKrDf6fQ7+609QlCVGoC3ovk"`
	dispatcherBroker404Body = `{"ok":false}`
)

// etagOf reads the ETag without going through Header.Get, which canonicalises
// its lookup key to "Etag" and therefore cannot see the exact spelling we
// deliberately store. Anything reading this header from Go must do the same.
func etagOf(t *testing.T, h http.Header) string {
	t.Helper()
	v, ok := h["ETag"]
	require.True(t, ok, `header must be spelled "ETag" exactly, got keys: %v`, h)
	require.Len(t, v, 1)
	return v[0]
}

// A dispatch that lands on a server holding no stream for the token is the
// case snyk-broker answers with its unknown-token 404, and that 404 is the
// only response relay-dispatcher will retry against another instance. We
// returned 502 instead, so the dispatcher read it as a real upstream failure
// and handed it back to the caller — the pool sweep never ran, and requests
// failed on whichever instance they happened to land on.
func TestNoTunnel_MatchesDispatcherUnknownTokenSignature(t *testing.T) {
	h := NewHttpAdapter(config.Config{}, tunnel.NewClientRegistry(zap.NewNop()), nil,
		metrics.New("test"), zap.NewNop())

	w := httptest.NewRecorder()
	h.writeDispatchError(w, dispatch.ErrNoTunnel, broker.NewToken("66fa3dbe-46fa-4949-9713-89a2ead1e7c2"))

	require.Equal(t, http.StatusNotFound, w.Code,
		"dispatcher requires 404; a 502 is not retried against the next instance")
	require.Equal(t, dispatcherBroker404ETag, etagOf(t, w.Header()),
		"dispatcher compares the ETag verbatim")
	require.Equal(t, dispatcherBroker404Body, w.Body.String(),
		"dispatcher compares the body verbatim — no reformatting, no trailing newline")
	require.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
}

// The raw broker token is a credential. Only the hash may be logged, and the
// response body must never carry either.
func TestNoTunnel_ResponseLeaksNoToken(t *testing.T) {
	h := NewHttpAdapter(config.Config{}, tunnel.NewClientRegistry(zap.NewNop()), nil,
		metrics.New("test"), zap.NewNop())

	const raw = "66fa3dbe-46fa-4949-9713-89a2ead1e7c2"
	w := httptest.NewRecorder()
	h.writeDispatchError(w, dispatch.ErrNoTunnel, broker.NewToken(raw))
	require.NotContains(t, w.Body.String(), raw)
}

// expressWeakETag has to agree with Express's `etag` package, since that is
// what produced the value the dispatcher hardcoded.
func TestExpressWeakETag(t *testing.T) {
	// W/"<byte length in hex>-<first 27 chars of base64(sha1(body))>"
	require.Equal(t, dispatcherBroker404ETag, expressWeakETag(brokerOKFalse))
	require.Equal(t, `W/"b-Ai2R8hgEarLmHKwesT1qcY913ys"`, expressWeakETag(brokerOKTrue))
	require.Len(t, brokerOKFalse, 12, "0xc in the ETag above is this length")
	require.Len(t, brokerOKTrue, 11, "0xb in the ETag above is this length")
}

// The liveness probe shares the same bodies, so it gets the same treatment:
// the dispatcher only checks for non-200 here, but a byte-identical response
// is the point of the exercise.
func TestConnectionStatus_CarriesExpressHeaders(t *testing.T) {
	registry := tunnel.NewClientRegistry(zap.NewNop())
	h := NewHttpAdapter(config.Config{}, registry, nil, metrics.New("test"), zap.NewNop())

	const rawToken = "66fa3dbe-46fa-4949-9713-89a2ead1e7c2"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connection-status/"+rawToken, nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, dispatcherBroker404Body, w.Body.String())
	require.Equal(t, dispatcherBroker404ETag, etagOf(t, w.Header()))
	require.Equal(t, "no-connection", w.Header().Get("x-broker-failure"))

	require.NoError(t, registry.Register(broker.NewToken(rawToken),
		tunnel.ClientIdentity{InstanceID: "axon-test", TenantID: "1"},
		&tunnel.StreamHandle{StreamID: "s1", Send: func(*pb.ServerFrame) error { return nil }},
	))

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connection-status/"+rawToken, nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, `{"ok":true}`, w.Body.String())
	require.Equal(t, `W/"b-Ai2R8hgEarLmHKwesT1qcY913ys"`, etagOf(t, w.Header()))
	require.Empty(t, w.Header().Get("x-broker-failure"), "connected clients report no failure")
}

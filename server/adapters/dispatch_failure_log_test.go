package adapters

import (
	"context"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// newLoggedAdapter wires an adapter and dispatcher over a registry holding
// one agent stream whose frames go to send, and returns the logs.
func newLoggedAdapter(t *testing.T, token broker.Token, send func(*pb.ServerFrame) error) (*HttpAdapter, *dispatch.Dispatcher, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	registry := tunnel.NewClientRegistry(logger)
	cfg := config.Config{DispatchTimeout: 5 * time.Second}
	m := metrics.New("test")
	d := dispatch.NewDispatcher(cfg, registry, m, logger)
	mustRegister(t, registry, token,
		tunnel.ClientIdentity{InstanceID: "agent-7", Alias: "gitlab-relay", Integration: "gitlab"},
		&tunnel.StreamHandle{StreamID: "s1", Send: send},
	)
	return NewHttpAdapter(cfg, registry, d, m, logger), d, logs
}

func onlyFailure(t *testing.T, logs *observer.ObservedLogs) observer.LoggedEntry {
	t.Helper()
	found := logs.FilterMessage("Dispatch failed").All()
	require.Len(t, found, 1, "exactly one failure line per failed request")
	return found[0]
}

// An agent refusing a call is the failure an operator most needs to
// attribute: which agent, which call, which path, and what the agent said.
func TestDispatchFailure_AgentCancelIsLoggedWithAttribution(t *testing.T) {
	const raw = "66fa3dbe-46fa-4949-9713-89a2ead1e7c2"
	token := broker.NewToken(raw)
	var d *dispatch.Dispatcher
	send := func(msg *pb.ServerFrame) error {
		call := msg.GetCall()
		if call.GetStart() != nil {
			// Answer as the agent would, off the dispatcher's goroutine.
			go d.HandleFrame("s1", &pb.CallFrame{CallId: call.CallId, Body: &pb.CallFrame_Cancel{
				Cancel: &pb.CallCancel{Reason: "no matching accept rule", Code: 404},
			}})
		}
		return nil
	}
	h, disp, logs := newLoggedAdapter(t, token, send)
	d = disp

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/broker/"+raw+"/api/v4/projects?private_token=secret", nil))
	require.Equal(t, http.StatusNotFound, w.Code)

	entry := onlyFailure(t, logs)
	assert.Equal(t, zapcore.WarnLevel, entry.Level)
	fields := entry.ContextMap()
	assert.Equal(t, string(dispatch.FailureAgentCancel), fields["kind"])
	assert.EqualValues(t, http.StatusNotFound, fields["status"])
	assert.EqualValues(t, 404, fields["agentCode"])
	assert.Equal(t, "agent-7", fields["instanceId"])
	assert.Equal(t, "gitlab-relay", fields["alias"])
	assert.Equal(t, "s1", fields["streamId"])
	assert.NotEmpty(t, fields["callId"])
	assert.Equal(t, token.Hashed(), fields["tokenHash"])
	assert.Equal(t, http.MethodGet, fields["method"])
	assert.Equal(t, "/api/v4/projects", fields["path"], "the query string can carry credentials")
	assert.Contains(t, fields["error"], "no matching accept rule")
	for _, v := range fields {
		if s, ok := v.(string); ok {
			assert.NotContains(t, s, raw, "the raw token must never be logged")
			assert.NotContains(t, s, "secret")
		}
	}
}

// The request-body failure used to make Dispatch return (nil, nil), which
// panicked the handler on resp.Body. It now fails like any other dispatch.
func TestDispatchFailure_RequestBodyFailureDoesNotPanic(t *testing.T) {
	token := broker.NewToken("tok")
	h, _, logs := newLoggedAdapter(t, token, func(*pb.ServerFrame) error { return nil })

	req := httptest.NewRequest(http.MethodPost, "/broker/tok/upload", io.MultiReader(strings.NewReader("abc"), failingBody{}))
	w := httptest.NewRecorder()
	require.NotPanics(t, func() { h.ServeHTTP(w, req) })
	require.Equal(t, http.StatusBadGateway, w.Code)

	fields := onlyFailure(t, logs).ContextMap()
	assert.Equal(t, string(dispatch.FailureRequestBody), fields["kind"])
	assert.Equal(t, "agent-7", fields["instanceId"])
}

// A request routed to a server without the token is how relay-dispatcher
// finds the right server, so it is logged, but not as a warning.
func TestDispatchFailure_NoTunnelIsInfo(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	registry := tunnel.NewClientRegistry(logger)
	m := metrics.New("test")
	d := dispatch.NewDispatcher(config.Config{}, registry, m, logger)
	h := NewHttpAdapter(config.Config{DispatchTimeout: time.Second}, registry, d, m, logger)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/broker/unknown/x", nil))
	require.Equal(t, http.StatusNotFound, w.Code)

	entry := onlyFailure(t, logs)
	assert.Equal(t, zapcore.InfoLevel, entry.Level)
	assert.Equal(t, string(dispatch.FailureNoTunnel), entry.ContextMap()["kind"])
}

// A deadline the agent never answers within is a timeout, logged with the
// agent and call that held it.
func TestDispatchFailure_ResponseTimeoutIsLogged(t *testing.T) {
	token := broker.NewToken("tok")
	h, _, logs := newLoggedAdapter(t, token, func(*pb.ServerFrame) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/broker/tok/slow", nil).WithContext(ctx))
	require.Equal(t, http.StatusGatewayTimeout, w.Code)

	fields := onlyFailure(t, logs).ContextMap()
	assert.Equal(t, string(dispatch.FailureResponseTimeout), fields["kind"])
	assert.Equal(t, "agent-7", fields["instanceId"])
	assert.NotEmpty(t, fields["callId"])
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

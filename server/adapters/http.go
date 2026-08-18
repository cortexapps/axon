// Package adapters contains the server-side protocol adapters that sit in
// front of the transport-agnostic dispatcher. Each adapter translates its
// caller's protocol (HTTP/1.1 today, gRPC in the future) into a
// dispatch.Request and streams the dispatch.Response back to the caller.
// See docs/design/grpc-tunnel-v2.md §6.
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"go.uber.org/zap"
)

// failedDispatchDetailMax caps the raw upstream error text included in the
// JSON error response body.
const failedDispatchDetailMax = 2048

// HttpAdapter translates HTTP/1.1 callers into dispatched calls.
// It mounts at /broker/* to preserve the snyk-broker dispatcher URL shape:
//   - GET /broker/connection-status/{token} → connection-status JSON
//   - /broker/{token}/{path...}             → tunnel dispatch
type HttpAdapter struct {
	registry        *tunnel.ClientRegistry
	dispatcher      *dispatch.Dispatcher
	metrics         *metrics.Metrics
	logger          *zap.Logger
	dispatchTimeout time.Duration
	mux             *http.ServeMux
}

// NewHttpAdapter creates a new HTTP adapter over the dispatcher.
func NewHttpAdapter(
	cfg config.Config,
	registry *tunnel.ClientRegistry,
	dispatcher *dispatch.Dispatcher,
	m *metrics.Metrics,
	logger *zap.Logger,
) *HttpAdapter {
	h := &HttpAdapter{
		registry:        registry,
		dispatcher:      dispatcher,
		metrics:         m,
		logger:          logger.Named("http-adapter"),
		dispatchTimeout: cfg.DispatchTimeout,
	}
	h.mux = http.NewServeMux()
	// The dispatcher asks at the root: ClientConnectivityService.checkBroker
	// builds http://{hostPort}/connection-status/{rawToken}, and treats any
	// non-200 as "this client is not connected here". Serving it only under
	// /broker/ meant every liveness check 404'd, so verifyClientLiveness and
	// bootstrapServerIndex removed this server from the token's index no
	// matter how many clients it was holding. snyk-broker serves the root
	// path; matching it is what makes the two transports interchangeable.
	h.mux.HandleFunc("GET /connection-status/{token}", h.getConnectionStatus)
	h.mux.HandleFunc("GET /broker/connection-status/{token}", h.getConnectionStatus)
	h.mux.HandleFunc("/broker/", h.handleBrokerDispatch)
	return h
}

// ServeHTTP delegates to the internal mux which routes between
// connection-status lookups and broker dispatch.
func (h *HttpAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// getConnectionStatus reports whether a broker token has a registered tunnel client.
//
// Reference: https://github.com/snyk/broker/blob/master/lib/hybrid-sdk/server/routesHandlers/connectionStatusHandler.ts
//
//	200  {"ok": true}                                       // token has at least one connected client
//	404  {"ok": false}     + x-broker-failure: no-connection
func (h *HttpAdapter) getConnectionStatus(w http.ResponseWriter, r *http.Request) {
	tokenOrHash := r.PathValue("token")

	// Try raw token first, then as already-hashed (mirrors handleBrokerDispatch).
	token := broker.NewToken(tokenOrHash)
	if h.registry.GetIdentity(token) == nil {
		token = broker.TokenFromHash(tokenOrHash)
	}
	connected := h.registry.GetIdentity(token) != nil

	w.Header().Set("Content-Type", "application/json")
	if connected {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
		return
	}
	w.Header().Set("x-broker-failure", "no-connection")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"ok":false}`))
}

// handleBrokerDispatch handles HTTP requests at /broker/<token>/<path>,
// forwarding them through a tunnel stream to a connected agent and
// streaming the response back.
func (h *HttpAdapter) handleBrokerDispatch(w http.ResponseWriter, r *http.Request) {
	// Extract token and path from URL: /broker/<token>/<path>
	// Use the escaped path so percent-encoded characters such as %2F in
	// mid-path segments (e.g. GitLab project IDs) survive this hop.
	trimmed := strings.TrimPrefix(r.URL.EscapedPath(), "/broker/")
	slashIdx := strings.Index(trimmed, "/")
	if slashIdx == -1 {
		http.Error(w, "invalid path: missing token", http.StatusBadRequest)
		return
	}

	tokenOrHash := trimmed[:slashIdx]
	dispatchPath := trimmed[slashIdx:]
	if r.URL.RawQuery != "" {
		dispatchPath += "?" + r.URL.RawQuery
	}

	// Try as raw token first, then as already-hashed.
	token := broker.NewToken(tokenOrHash)
	identity := h.registry.GetIdentity(token)
	if identity == nil {
		token = broker.TokenFromHash(tokenOrHash)
		identity = h.registry.GetIdentity(token)
	}

	// Determine dispatch timeout from request context deadline, falling back
	// to the configured default. Bail early if the deadline has already passed.
	timeout := h.dispatchTimeout
	if deadline, ok := r.Context().Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			h.metrics.DispatchErrors.Inc(1)
			http.Error(w, "request deadline already passed", http.StatusGatewayTimeout)
			return
		}
		timeout = remaining
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Extract request headers.
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}

	req := &dispatch.Request{
		PseudoHeaders: map[string]string{
			":method": r.Method,
			":path":   dispatchPath,
		},
		Headers:   headers,
		Body:      r.Body,
		Kind:      pb.CallStart_UNARY,
		TimeoutMs: int32(timeout.Milliseconds()),
	}

	h.logger.Debug("Dispatching request",
		zap.String("method", r.Method),
		zap.String("path", dispatchPath),
		zap.Duration("timeout", timeout),
	)

	start := time.Now()
	resp, err := h.dispatcher.Dispatch(ctx, token, req)
	if err != nil {
		h.writeDispatchError(w, err)
		return
	}
	defer resp.Body.Close()

	// Record tagged metrics if we have identity info.
	tenantID, integration, alias := "", "", ""
	if identity != nil {
		tenantID = identity.TenantID
		integration = identity.Integration
		alias = identity.Alias
	}

	statusCode, err := strconv.Atoi(resp.PseudoHeaders[":status"])
	if err != nil || statusCode < 100 || statusCode > 599 {
		h.logger.Error("Invalid :status from agent",
			zap.String("status", resp.PseudoHeaders[":status"]))
		h.metrics.DispatchErrors.Inc(1)
		http.Error(w, "invalid response status from agent", http.StatusBadGateway)
		return
	}

	h.metrics.DispatchCount(tenantID, integration, alias, r.Method, statusCode)

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(statusCode)

	// Stream the body, flushing as chunks arrive so streaming responses
	// (SSE, chunked downloads) reach the caller without buffering.
	flusher, _ := w.(http.Flusher)
	_, copyErr := io.Copy(&flushWriter{w: w, f: flusher}, resp.Body)

	h.metrics.DispatchDuration(tenantID, integration, alias, float64(time.Since(start).Milliseconds()))

	if copyErr != nil {
		// Response already committed; nothing to send but log it.
		h.logger.Warn("Response body aborted mid-stream", zap.Error(copyErr))
		h.metrics.DispatchErrors.Inc(1)
		return
	}

	// Drain trailers/late errors. Trailer propagation to HTTP/1.1 callers
	// requires TE:trailers negotiation, which Cortex-cloud callers don't
	// use against SaaS APIs; log-and-drop matches httputil.ReverseProxy.
	select {
	case <-resp.TrailersC:
	case err := <-resp.ErrC:
		if err != nil {
			h.logger.Warn("Late failure after response completed", zap.Error(err))
		}
	default:
	}
}

// writeDispatchError maps Dispatch errors to HTTP responses.
func (h *HttpAdapter) writeDispatchError(w http.ResponseWriter, err error) {
	h.metrics.DispatchErrors.Inc(1)

	var cancelErr *dispatch.CancelError
	switch {
	case errors.As(err, &cancelErr):
		status := int(cancelErr.Code)
		if status < 100 || status > 599 {
			status = http.StatusBadGateway
		}
		writeFailedDispatch(w, h.logger, status, cancelErr.Reason)

	case errors.Is(err, dispatch.ErrNoTunnel):
		h.logger.Warn("No tunnel available for token")
		http.Error(w, "no tunnel available", http.StatusBadGateway)

	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)

	case errors.Is(err, context.Canceled):
		// Caller went away; nothing useful to write.
		http.Error(w, "client closed request", 499)

	default:
		h.logger.Error("Dispatch failed", zap.Error(err))
		http.Error(w, "tunnel dispatch failed", http.StatusBadGateway)
	}
}

// writeFailedDispatch writes a JSON error response for a dispatch the agent
// could not complete, carrying the raw agent error (truncated).
func writeFailedDispatch(w http.ResponseWriter, logger *zap.Logger, status int, detail string) {
	logger.Error("Dispatch failed",
		zap.Int("status", status),
		zap.String("error", detail),
	)

	if len(detail) > failedDispatchDetailMax {
		detail = detail[:failedDispatchDetailMax]
	}
	payload, _ := json.Marshal(struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}{
		Error:  "dispatch failed",
		Detail: detail,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(payload)
}

// flushWriter flushes after every write so streamed responses are not
// buffered by net/http.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil && fw.f != nil {
		fw.f.Flush()
	}
	if err != nil {
		return n, fmt.Errorf("write to caller failed: %w", err)
	}
	return n, nil
}

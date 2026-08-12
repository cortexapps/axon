package dispatch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const maxChunkSize = 1024 * 1024             // 1MB
const maxRequestBodySize = 100 * 1024 * 1024 // 100MB

// dispatchTimeoutBuffer extends the pending-queue timeout beyond the agent-facing
// request timeout, so the entry outlives a late response long enough to deliver
// it (or log a properly-attributed timeout).
const dispatchTimeoutBuffer = 5 * time.Second

// failedDispatchDetailMax caps the raw upstream error text included in the
// JSON error response body.
const failedDispatchDetailMax = 2048

// Handler is the HTTP handler that dispatches requests through tunnel streams.
// It mounts at /broker/* and internally routes:
//   - GET /broker/connection-status/{token} → connection-status JSON
//   - /broker/{token}/{path...}              → tunnel dispatch
type Handler struct {
	registry        *tunnel.ClientRegistry
	pending         *PendingRequests
	metrics         *metrics.Metrics
	logger          *zap.Logger
	dispatchTimeout time.Duration
	mux             *http.ServeMux
}

// NewHandler creates a new dispatch handler.
func NewHandler(
	cfg config.Config,
	registry *tunnel.ClientRegistry,
	m *metrics.Metrics,
	logger *zap.Logger,
) *Handler {
	h := &Handler{
		registry:        registry,
		pending:         NewPendingRequests(cfg.DispatchTimeout),
		metrics:         m,
		logger:          logger.Named("dispatch"),
		dispatchTimeout: cfg.DispatchTimeout,
	}
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /broker/connection-status/{token}", h.getConnectionStatus)
	h.mux.HandleFunc("/broker/", h.handleBrokerDispatch)
	return h
}

// HandleResponse processes an incoming HttpResponse from the tunnel service.
// This is the ResponseHandler callback set on the tunnel service.
func (h *Handler) HandleResponse(response *pb.HttpResponse) {
	if err := h.pending.Deliver(response); err != nil {
		h.logger.Debug("Response delivery failed", zap.String("requestId", response.RequestId), zap.Error(err))
	}
}

// HandleStreamClose fails all pending dispatch requests for a closed stream.
func (h *Handler) HandleStreamClose(streamID string) {
	h.pending.FailStream(streamID)
}

// PendingCount returns the number of inflight dispatch requests.
func (h *Handler) PendingCount() int {
	return h.pending.Count()
}

// ServeHTTP delegates to the internal mux which routes between
// connection-status lookups and broker dispatch.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// getConnectionStatus reports whether a broker token has a registered tunnel client.
//
// Reference: https://github.com/snyk/broker/blob/master/lib/hybrid-sdk/server/routesHandlers/connectionStatusHandler.ts
//
// Current response (first pass):
//
//	200  {"ok": true}                                       // token has at least one connected client
//	404  {"ok": false}     + x-broker-failure: no-connection
//
// Planned (follow-up — not yet populated):
//
//	200  {
//	  "ok": true,
//	  "clients": [
//	    {
//	      "version":     "<agent version>",   // from ClientHello / identity
//	      "streamId":    "<stream uuid>",     // per active stream
//	      "filters":     [ ... ],             // accept-file filters, once wired through
//	      "connectedAt": "<RFC3339 timestamp>"
//	    }
//	  ]
//	}
func (h *Handler) getConnectionStatus(w http.ResponseWriter, r *http.Request) {
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
// forwarding them through a tunnel stream to a connected agent.
func (h *Handler) handleBrokerDispatch(w http.ResponseWriter, r *http.Request) {
	// Extract token and path from URL: /broker/<token>/<path>
	trimmed := strings.TrimPrefix(r.URL.Path, "/broker/")
	slashIdx := strings.Index(trimmed, "/")
	if slashIdx == -1 {
		http.Error(w, "invalid path: missing token", http.StatusBadRequest)
		return
	}

	tokenOrHash := trimmed[:slashIdx]
	dispatchPath := trimmed[slashIdx:]

	// Try as raw token first, then as already-hashed.
	token := broker.NewToken(tokenOrHash)
	identity := h.registry.GetIdentity(token)
	if identity == nil {
		token = broker.TokenFromHash(tokenOrHash)
		identity = h.registry.GetIdentity(token)
	}

	// Pick a stream for dispatch (prefers last-successful, see ClientRegistry.PickStream).
	stream := h.registry.PickStream(token)
	if stream == nil {
		h.logger.Warn("No tunnel available for token",
			zap.String("hashedToken", token.Hashed()),
		)
		h.metrics.DispatchErrors.Inc(1)
		http.Error(w, "no tunnel available", http.StatusBadGateway)
		return
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
	timeoutMs := int32(timeout.Milliseconds())

	// Read request body with size limit to prevent OOM.
	var body []byte
	if r.Body != nil {
		limitedReader := io.LimitReader(r.Body, maxRequestBodySize+1)
		var err error
		body, err = io.ReadAll(limitedReader)
		if err != nil {
			h.logger.Error("Failed to read request body", zap.Error(err))
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
		if len(body) > maxRequestBodySize {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Extract request headers.
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}

	requestID := uuid.New().String()

	h.logger.Debug("Dispatching request",
		zap.String("requestId", requestID),
		zap.String("method", r.Method),
		zap.String("path", dispatchPath),
		zap.String("streamId", stream.StreamID),
		zap.Duration("timeout", timeout),
	)

	start := time.Now()
	h.metrics.DispatchInflight.Update(float64(h.pending.Count() + 1))

	// Register pending response before sending. The pending entry lives slightly
	// longer than the agent-facing timeout to absorb in-flight late responses.
	respCh := h.pending.AddWithTimeout(requestID, stream.StreamID, timeout+dispatchTimeoutBuffer)

	if err := h.sendRequest(stream, requestID, r.Method, dispatchPath, headers, body, timeoutMs); err != nil {
		h.pending.Timeout(requestID)
		h.logger.Error("Failed to send request through tunnel", zap.Error(err))
		h.metrics.DispatchErrors.Inc(1)
		http.Error(w, "tunnel send failed", http.StatusBadGateway)
		return
	}

	h.metrics.DispatchBytesSent.Inc(int64(len(body)))

	// Wait for response.
	resp, ok := <-respCh
	duration := time.Since(start)
	h.metrics.DispatchInflight.Update(float64(h.pending.Count()))

	if !ok || resp == nil {
		h.logger.Warn("Dispatch timeout or stream closed",
			zap.String("requestId", requestID),
			zap.Duration("duration", duration),
		)
		h.metrics.DispatchErrors.Inc(1)
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}

	// Mark the stream as healthy — a response (even a 5xx or failed-dispatch) means
	// the tunnel itself is delivering.
	stream.LastSuccessAt.Store(time.Now().UnixNano())

	// Record tagged metrics if we have identity info.
	tenantID, integration, alias := "", "", ""
	if identity != nil {
		tenantID = identity.TenantID
		integration = identity.Integration
		alias = identity.Alias
	}
	h.metrics.DispatchCount(tenantID, integration, alias, r.Method, resp.StatusCode)
	h.metrics.DispatchDuration(tenantID, integration, alias, float64(duration.Milliseconds()))
	h.metrics.DispatchBytesRecv.Inc(int64(len(resp.Body)))

	if resp.IsFailedDispatch {
		writeFailedDispatch(w, h.logger, requestID, resp.Body)
		return
	}

	// Write response.
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.StatusCode)
	if len(resp.Body) > 0 {
		w.Write(resp.Body)
	}
}

// writeFailedDispatch logs an upstream-failed dispatch and writes a JSON 502
// response containing the raw error (truncated). Used by both the production
// dispatcher and the E2E test mock.
func writeFailedDispatch(w http.ResponseWriter, logger *zap.Logger, requestID string, rawBody []byte) {
	logger.Error("Dispatch failed",
		zap.String("requestId", requestID),
		zap.ByteString("error", rawBody),
	)

	detail := string(rawBody)
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
	w.WriteHeader(http.StatusBadGateway)
	w.Write(payload)
}

// sendRequest sends an HTTP request through a tunnel stream, chunking large bodies.
func (h *Handler) sendRequest(stream *tunnel.StreamHandle, requestID, method, path string, headers map[string]string, body []byte, timeoutMs int32) error {
	if len(body) <= maxChunkSize {
		return stream.Send(&pb.TunnelServerMessage{
			Message: &pb.TunnelServerMessage_HttpRequest{
				HttpRequest: &pb.HttpRequest{
					RequestId:  requestID,
					Method:     method,
					Path:       path,
					Headers:    headers,
					Body:       body,
					ChunkIndex: 0,
					IsFinal:    true,
					TimeoutMs:  timeoutMs,
				},
			},
		})
	}

	// Chunked send for large bodies.
	for i := 0; i < len(body); i += maxChunkSize {
		end := i + maxChunkSize
		if end > len(body) {
			end = len(body)
		}
		chunkIndex := int32(i / maxChunkSize)
		isFinal := end == len(body)

		req := &pb.HttpRequest{
			RequestId:  requestID,
			ChunkIndex: chunkIndex,
			IsFinal:    isFinal,
			Body:       body[i:end],
		}

		// First chunk includes method/path/headers and timeout.
		if chunkIndex == 0 {
			req.Method = method
			req.Path = path
			req.Headers = headers
			req.TimeoutMs = timeoutMs
		}

		if err := stream.Send(&pb.TunnelServerMessage{
			Message: &pb.TunnelServerMessage_HttpRequest{
				HttpRequest: req,
			},
		}); err != nil {
			return fmt.Errorf("send chunk %d: %w", chunkIndex, err)
		}
	}
	return nil
}

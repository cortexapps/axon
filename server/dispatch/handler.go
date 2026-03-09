package dispatch

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const maxChunkSize = 1024 * 1024 // 1MB

// Handler is the HTTP handler that dispatches requests through tunnel streams.
// It mounts at /broker/:token/* and routes HTTP requests to connected agents.
type Handler struct {
	registry       *tunnel.ClientRegistry
	pending        *PendingRequests
	metrics        *metrics.Metrics
	logger         *zap.Logger
	dispatchTimeout time.Duration
}

// NewHandler creates a new dispatch handler.
func NewHandler(
	cfg config.Config,
	registry *tunnel.ClientRegistry,
	m *metrics.Metrics,
	logger *zap.Logger,
) *Handler {
	return &Handler{
		registry:        registry,
		pending:         NewPendingRequests(cfg.DispatchTimeout),
		metrics:         m,
		logger:          logger.Named("dispatch"),
		dispatchTimeout: cfg.DispatchTimeout,
	}
}

// HandleResponse processes an incoming HttpResponse from the tunnel service.
// This is the ResponseHandler callback set on the tunnel service.
func (h *Handler) HandleResponse(response *pb.HttpResponse) {
	if err := h.pending.Deliver(response); err != nil {
		h.logger.Debug("Response delivery failed", zap.String("requestId", response.RequestId), zap.Error(err))
	}
}

// PendingCount returns the number of inflight dispatch requests.
func (h *Handler) PendingCount() int {
	return h.pending.Count()
}

// ServeHTTP handles HTTP requests at /broker/<token>/<path>.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	// Pick a stream for dispatch (round-robin).
	stream := h.registry.PickStream(token)
	if stream == nil {
		h.logger.Warn("No tunnel available for token",
			zap.String("hashedToken", token.Hashed()),
		)
		h.metrics.DispatchErrors.Inc(1)
		http.Error(w, "no tunnel available", http.StatusBadGateway)
		return
	}

	// Read request body.
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			h.logger.Error("Failed to read request body", zap.Error(err))
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
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
	)

	start := time.Now()
	h.metrics.DispatchInflight.Update(float64(h.pending.Count() + 1))

	// Register pending response before sending.
	respCh := h.pending.Add(requestID, stream.StreamID)

	// Send request through the tunnel (chunked if needed).
	timeoutMs := int32(h.dispatchTimeout.Milliseconds())
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

	// Write response.
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.StatusCode)
	if len(resp.Body) > 0 {
		w.Write(resp.Body)
	}
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

// gRPC tunnel server for E2E testing.
// This server mimics the Cortex-side tunnel endpoint that accepts
// gRPC connections from Axon agents and dispatches HTTP requests through them.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// failedDispatchDetailMax caps the raw upstream error text included in the JSON
// error response body. Mirrors the production handler in server/dispatch/handler.go.
const failedDispatchDetailMax = 2048

// TunnelServer implements the gRPC TunnelService for E2E tests. It stands in
// for the Cortex-side tunnel endpoint when running the agent against a local
// fixture. The production equivalent lives in server/dispatch/handler.go;
// they are intentionally separate.
type TunnelServer struct {
	tunnelpb.UnimplementedTunnelServiceServer

	logger  *zap.Logger
	mu      sync.RWMutex
	streams map[string]*tunnelStream // keyed by broker token
}

type tunnelStream struct {
	stream   tunnelpb.TunnelService_TunnelServer
	hello    *tunnelpb.ClientHello
	streamID string
	logger   *zap.Logger

	// Pending requests waiting for responses
	pendingMu sync.Mutex
	pending   map[string]chan *tunnelpb.HttpResponse
}

func NewTunnelServer(logger *zap.Logger) *TunnelServer {
	return &TunnelServer{
		logger:  logger,
		streams: make(map[string]*tunnelStream),
	}
}

func (s *TunnelServer) Tunnel(stream tunnelpb.TunnelService_TunnelServer) error {
	// Wait for ClientHello
	msg, err := stream.Recv()
	if err != nil {
		s.logger.Error("Failed to receive first message", zap.Error(err))
		return err
	}

	hello := msg.GetHello()
	if hello == nil {
		s.logger.Error("First message was not ClientHello")
		return fmt.Errorf("first message must be ClientHello")
	}

	streamID := uuid.New().String()
	ts := &tunnelStream{
		stream:   stream,
		hello:    hello,
		streamID: streamID,
		logger:   s.logger,
		pending:  make(map[string]chan *tunnelpb.HttpResponse),
	}

	// Register stream
	s.mu.Lock()
	s.streams[hello.BrokerToken] = ts
	s.mu.Unlock()

	s.logger.Info("Tunnel stream established",
		zap.String("token", hello.BrokerToken),
		zap.String("alias", hello.Alias),
		zap.String("integration", hello.Integration),
		zap.String("streamId", streamID),
	)

	// Send ServerHello
	serverHello := &tunnelpb.TunnelServerMessage{
		Message: &tunnelpb.TunnelServerMessage_Hello{
			Hello: &tunnelpb.ServerHello{
				ServerId:            getServerID(),
				HeartbeatIntervalMs: 30000,
				StreamId:            streamID,
			},
		},
	}
	if err := stream.Send(serverHello); err != nil {
		s.removeStream(hello.BrokerToken)
		return err
	}

	// Handle incoming messages
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			s.logger.Info("Tunnel stream closed (EOF)", zap.String("token", hello.BrokerToken))
			break
		}
		if err != nil {
			s.logger.Warn("Tunnel stream error",
				zap.String("token", hello.BrokerToken),
				zap.Error(err),
			)
			break
		}

		switch m := msg.Message.(type) {
		case *tunnelpb.TunnelClientMessage_Heartbeat:
			// Respond to heartbeat
			hb := &tunnelpb.TunnelServerMessage{
				Message: &tunnelpb.TunnelServerMessage_Heartbeat{
					Heartbeat: &tunnelpb.Heartbeat{TimestampMs: time.Now().UnixMilli()},
				},
			}
			if err := stream.Send(hb); err != nil {
				s.logger.Warn("Failed to send heartbeat response", zap.Error(err))
			}

		case *tunnelpb.TunnelClientMessage_HttpResponse:
			ts.handleResponse(m.HttpResponse)
		}
	}

	s.removeStream(hello.BrokerToken)
	return nil
}

func (s *TunnelServer) removeStream(token string) {
	s.mu.Lock()
	delete(s.streams, token)
	s.mu.Unlock()
	s.logger.Info("Tunnel stream removed", zap.String("token", token))
}

func (s *TunnelServer) getStream(token string) *tunnelStream {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streams[token]
}

func (s *TunnelServer) streamCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.streams)
}

func (ts *tunnelStream) handleResponse(resp *tunnelpb.HttpResponse) {
	ts.pendingMu.Lock()
	ch, ok := ts.pending[resp.RequestId]
	ts.pendingMu.Unlock()

	if ok {
		ch <- resp
	} else {
		ts.logger.Warn("Received response for unknown request", zap.String("requestId", resp.RequestId))
	}
}

func (ts *tunnelStream) sendRequest(ctx context.Context, req *tunnelpb.HttpRequest) (*tunnelpb.HttpResponse, error) {
	// Create response channel
	respChan := make(chan *tunnelpb.HttpResponse, 1)

	ts.pendingMu.Lock()
	ts.pending[req.RequestId] = respChan
	ts.pendingMu.Unlock()

	defer func() {
		ts.pendingMu.Lock()
		delete(ts.pending, req.RequestId)
		ts.pendingMu.Unlock()
	}()

	// Chunking deliberately omitted: no E2E test currently exercises >1MB
	// request bodies through this mock. Mirror the production chunker in
	// server/dispatch/handler.go:sendRequest if that changes.
	msg := &tunnelpb.TunnelServerMessage{
		Message: &tunnelpb.TunnelServerMessage_HttpRequest{
			HttpRequest: req,
		},
	}
	if err := ts.stream.Send(msg); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	select {
	case resp := <-respChan:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HTTPHandler handles dispatch requests from the test
type HTTPHandler struct {
	server *TunnelServer
	logger *zap.Logger
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Handle healthz endpoint
	if path == "/healthz" {
		h.handleHealthz(w, r)
		return
	}

	// Handle broker dispatch: /broker/{token}/{path...}
	if strings.HasPrefix(path, "/broker/") {
		h.handleDispatch(w, r)
		return
	}

	http.NotFound(w, r)
}

func (h *HTTPHandler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	count := h.server.streamCount()
	resp := map[string]interface{}{
		"status":  "ok",
		"streams": count,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *HTTPHandler) handleDispatch(w http.ResponseWriter, r *http.Request) {
	// Parse /broker/{token}/{path...}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/broker/"), "/", 2)
	if len(parts) < 1 {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	token := parts[0]
	path := "/"
	if len(parts) > 1 {
		path = "/" + parts[1]
	}

	// Find stream for token
	ts := h.server.getStream(token)
	if ts == nil {
		http.Error(w, fmt.Sprintf("no tunnel for token: %s", token), http.StatusBadGateway)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusInternalServerError)
		return
	}

	// Convert headers
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	ctx := r.Context()
	deadline, ok := ctx.Deadline()
	var timeoutMs int32 = 60000
	if ok {
		timeoutMs = int32(time.Until(deadline).Milliseconds())
	} else {
		ctx2, cancel := context.WithDeadline(
			ctx,
			time.Now().Add(time.Millisecond*time.Duration(timeoutMs)),
		)
		ctx = ctx2
		defer cancel()
	}

	// Create HTTP request
	req := &tunnelpb.HttpRequest{
		RequestId:  uuid.New().String(),
		Method:     r.Method,
		Path:       path,
		Headers:    headers,
		Body:       body,
		ChunkIndex: 0,
		IsFinal:    true,
		TimeoutMs:  timeoutMs,
	}

	// Send through tunnel
	resp, err := ts.sendRequest(ctx, req)
	if err != nil {
		http.Error(w, fmt.Sprintf("tunnel request failed: %v", err), http.StatusBadGateway)
		return
	}

	if resp.IsFailedDispatch {
		writeFailedDispatch(w, h.logger, req.RequestId, resp.Body)
		return
	}

	// Write response
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(int(resp.StatusCode))
	w.Write(resp.Body)
}

// writeFailedDispatch logs an upstream-failed dispatch and writes a JSON 502
// response containing the raw error (truncated). Mirrors the production helper
// in server/dispatch/handler.go.
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

func getServerID() string {
	if id := os.Getenv("HOSTNAME"); id != "" {
		return id
	}
	return uuid.New().String()
}

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(fmt.Sprintf("failed to create logger: %v", err))
	}
	defer logger.Sync()

	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = "50051"
	}

	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}

	// Create tunnel server
	tunnelServer := NewTunnelServer(logger)

	// Start gRPC server
	grpcLis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		logger.Fatal("Failed to listen on gRPC port",
			zap.String("port", grpcPort),
			zap.Error(err),
		)
	}

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, tunnelServer)

	go func() {
		logger.Info("gRPC server listening", zap.String("port", grpcPort))
		if err := grpcServer.Serve(grpcLis); err != nil {
			logger.Fatal("gRPC server failed", zap.Error(err))
		}
	}()

	// Start HTTP server
	httpHandler := &HTTPHandler{server: tunnelServer, logger: logger}
	httpServer := &http.Server{
		Addr:    ":" + httpPort,
		Handler: httpHandler,
	}

	logger.Info("HTTP server listening", zap.String("port", httpPort))
	if err := httpServer.ListenAndServe(); err != nil {
		logger.Fatal("HTTP server failed", zap.Error(err))
	}
}

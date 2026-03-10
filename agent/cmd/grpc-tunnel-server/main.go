// gRPC tunnel server for E2E testing.
// This server mimics the Cortex-side tunnel endpoint that accepts
// gRPC connections from Axon agents and dispatches HTTP requests through them.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// TunnelServer implements the gRPC TunnelService.
type TunnelServer struct {
	tunnelpb.UnimplementedTunnelServiceServer

	mu      sync.RWMutex
	streams map[string]*tunnelStream // keyed by broker token
}

type tunnelStream struct {
	stream   tunnelpb.TunnelService_TunnelServer
	hello    *tunnelpb.ClientHello
	streamID string

	// Pending requests waiting for responses
	pendingMu sync.Mutex
	pending   map[string]chan *tunnelpb.HttpResponse
}

func NewTunnelServer() *TunnelServer {
	return &TunnelServer{
		streams: make(map[string]*tunnelStream),
	}
}

func (s *TunnelServer) Tunnel(stream tunnelpb.TunnelService_TunnelServer) error {
	// Wait for ClientHello
	msg, err := stream.Recv()
	if err != nil {
		log.Printf("Failed to receive first message: %v", err)
		return err
	}

	hello := msg.GetHello()
	if hello == nil {
		log.Printf("First message was not ClientHello")
		return fmt.Errorf("first message must be ClientHello")
	}

	streamID := uuid.New().String()
	ts := &tunnelStream{
		stream:   stream,
		hello:    hello,
		streamID: streamID,
		pending:  make(map[string]chan *tunnelpb.HttpResponse),
	}

	// Register stream
	s.mu.Lock()
	s.streams[hello.BrokerToken] = ts
	s.mu.Unlock()

	log.Printf("Tunnel stream established: token=%s alias=%s integration=%s streamID=%s",
		hello.BrokerToken, hello.Alias, hello.Integration, streamID)

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
			log.Printf("Tunnel stream closed (EOF): token=%s", hello.BrokerToken)
			break
		}
		if err != nil {
			log.Printf("Tunnel stream error: token=%s err=%v", hello.BrokerToken, err)
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
				log.Printf("Failed to send heartbeat response: %v", err)
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
	log.Printf("Tunnel stream removed: token=%s", token)
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
		log.Printf("Received response for unknown request: %s", resp.RequestId)
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

	// Send request
	msg := &tunnelpb.TunnelServerMessage{
		Message: &tunnelpb.TunnelServerMessage_HttpRequest{
			HttpRequest: req,
		},
	}
	if err := ts.stream.Send(msg); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Wait for response with timeout
	timeout := 60 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	select {
	case resp := <-respChan:
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("request timeout after %v", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HTTPHandler handles dispatch requests from the test
type HTTPHandler struct {
	server *TunnelServer
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

	// Create HTTP request
	req := &tunnelpb.HttpRequest{
		RequestId:  uuid.New().String(),
		Method:     r.Method,
		Path:       path,
		Headers:    headers,
		Body:       body,
		ChunkIndex: 0,
		IsFinal:    true,
		TimeoutMs:  60000,
	}

	// Send through tunnel
	resp, err := ts.sendRequest(r.Context(), req)
	if err != nil {
		http.Error(w, fmt.Sprintf("tunnel request failed: %v", err), http.StatusBadGateway)
		return
	}

	// Write response
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(int(resp.StatusCode))
	w.Write(resp.Body)
}

func getServerID() string {
	if id := os.Getenv("HOSTNAME"); id != "" {
		return id
	}
	return uuid.New().String()
}

func main() {
	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = "50051"
	}

	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}

	// Create tunnel server
	tunnelServer := NewTunnelServer()

	// Start gRPC server
	grpcLis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port %s: %v", grpcPort, err)
	}

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, tunnelServer)

	go func() {
		log.Printf("gRPC server listening on :%s", grpcPort)
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatalf("gRPC server failed: %v", err)
		}
	}()

	// Start HTTP server
	httpHandler := &HTTPHandler{server: tunnelServer}
	httpServer := &http.Server{
		Addr:    ":" + httpPort,
		Handler: httpHandler,
	}

	log.Printf("HTTP server listening on :%s", httpPort)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}

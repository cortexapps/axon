package grpctunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/requestexecutor"
	"github.com/cortexapps/axon/server/snykbroker"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const maxChunkSize = 1024 * 1024 // 1MB

// tunnelClient implements the snykbroker.RelayInstanceManager interface
// using gRPC bidirectional streaming instead of snyk-broker.
type tunnelClient struct {
	config          config.AgentConfig
	logger          *zap.Logger
	integrationInfo common.IntegrationInfo
	registration    snykbroker.Registration
	executor        requestexecutor.RequestExecutor
	httpClient      *http.Client

	running     atomic.Bool
	mu          sync.Mutex
	conn        *grpc.ClientConn
	streams     []*tunnelStream
	parentCtx   context.Context
	cancelAll   context.CancelFunc

	// Metrics
	connectionsActive *prometheus.GaugeVec
	requestsTotal     *prometheus.CounterVec
	reconnectsTotal   *prometheus.CounterVec
	requestDuration   *prometheus.HistogramVec
}

type tunnelStream struct {
	streamID string
	serverID string
	cancel   context.CancelFunc
	done     chan struct{}
}

// sendFunc is a mutex-protected function for sending messages on a gRPC stream.
// Multiple goroutines (heartbeat responses, HTTP response handlers) call Send()
// concurrently; wrapping it in a mutex prevents data races on the underlying stream.
type sendFunc func(msg *pb.TunnelClientMessage) error

// requestAssembler reassembles chunked HTTP requests from the server.
// The server chunks requests larger than 1MB, sending method/path/headers/timeoutMs
// only on the first chunk (chunk_index=0) and body data on all chunks.
type requestAssembler struct {
	mu      sync.Mutex
	pending map[string]*pendingRequest
}

type pendingRequest struct {
	method    string
	path      string
	headers   map[string]string
	body      []byte
	timeoutMs int32
}

func newRequestAssembler() *requestAssembler {
	return &requestAssembler{
		pending: make(map[string]*pendingRequest),
	}
}

// handleChunk processes an incoming HttpRequest chunk. It returns a fully
// assembled *pb.HttpRequest when the final chunk arrives, or nil if more
// chunks are still expected.
func (ra *requestAssembler) handleChunk(chunk *pb.HttpRequest) *pb.HttpRequest {
	// Fast path: single-chunk request (most common case).
	if chunk.ChunkIndex == 0 && chunk.IsFinal {
		return chunk
	}

	ra.mu.Lock()
	defer ra.mu.Unlock()

	if chunk.ChunkIndex == 0 {
		// First chunk of a multi-chunk request: store metadata + body.
		ra.pending[chunk.RequestId] = &pendingRequest{
			method:    chunk.Method,
			path:      chunk.Path,
			headers:   chunk.Headers,
			body:      append([]byte(nil), chunk.Body...),
			timeoutMs: chunk.TimeoutMs,
		}
		return nil
	}

	// Continuation chunk.
	pr, ok := ra.pending[chunk.RequestId]
	if !ok {
		// Orphan chunk — no first chunk was received; discard.
		return nil
	}

	pr.body = append(pr.body, chunk.Body...)

	if !chunk.IsFinal {
		return nil
	}

	// Final chunk: assemble and remove from pending.
	delete(ra.pending, chunk.RequestId)
	return &pb.HttpRequest{
		RequestId: chunk.RequestId,
		Method:    pr.method,
		Path:      pr.path,
		Headers:   pr.headers,
		Body:      pr.body,
		TimeoutMs: pr.timeoutMs,
		IsFinal:   true,
	}
}

// discardAll removes all incomplete pending requests (called on stream close).
func (ra *requestAssembler) discardAll() {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	ra.pending = make(map[string]*pendingRequest)
}

type TunnelClientParams struct {
	fx.In
	Lifecycle       fx.Lifecycle `optional:"true"`
	Config          config.AgentConfig
	Logger          *zap.Logger
	IntegrationInfo common.IntegrationInfo
	HttpServer      cortexHttp.Server
	Registration    snykbroker.Registration
	HttpClient      *http.Client     `optional:"true"`
	Registry        *prometheus.Registry `optional:"true"`
}

func NewTunnelClient(p TunnelClientParams) snykbroker.RelayInstanceManager {
	httpClient := p.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		}
	}

	tc := &tunnelClient{
		config:          p.Config,
		logger:          p.Logger.Named("grpc-tunnel"),
		integrationInfo: p.IntegrationInfo,
		registration:    p.Registration,
		httpClient:      httpClient,
		connectionsActive: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "grpc_tunnel_connections_active",
				Help: "Number of active gRPC tunnel streams",
			},
			[]string{"server_id"},
		),
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "grpc_tunnel_requests_total",
				Help: "Total requests dispatched through gRPC tunnel",
			},
			[]string{"method", "status"},
		),
		reconnectsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "grpc_tunnel_reconnects_total",
				Help: "Total tunnel reconnection attempts",
			},
			[]string{"server_id"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "grpc_tunnel_request_duration_ms",
				Help:    "Request execution latency in milliseconds",
				Buckets: prometheus.ExponentialBuckets(10, 2, 12),
			},
			[]string{"method"},
		),
	}

	p.HttpServer.RegisterHandler(tc)

	if p.Registry != nil {
		p.Registry.MustRegister(
			tc.connectionsActive,
			tc.requestsTotal,
			tc.reconnectsTotal,
			tc.requestDuration,
		)
	}

	if p.Lifecycle != nil {
		p.Lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return tc.Start()
			},
			OnStop: func(ctx context.Context) error {
				return tc.Close()
			},
		})
	}

	return tc
}

func (tc *tunnelClient) RegisterRoutes(mux *mux.Router) error {
	subRouter := mux.PathPrefix(fmt.Sprintf("%s/broker", cortexHttp.AxonPathRoot)).Subrouter()
	subRouter.HandleFunc("/restart", tc.handleRestart)
	subRouter.HandleFunc("/systemcheck", tc.handleSystemCheck)
	return nil
}

func (tc *tunnelClient) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

func (tc *tunnelClient) handleRestart(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := tc.Restart(); err != nil {
		tc.logger.Error("Restart failed", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (tc *tunnelClient) handleSystemCheck(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	tc.mu.Lock()
	streamCount := len(tc.streams)
	tc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","relay_mode":"grpc-tunnel","streams":%d}`, streamCount)
}

func (tc *tunnelClient) Start() error {
	if !tc.running.CompareAndSwap(false, true) {
		return fmt.Errorf("already started")
	}

	go tc.startAsync()
	return nil
}

func (tc *tunnelClient) startAsync() {
	// Register with Cortex API to get server URI + token.
	var regInfo *snykbroker.RegistrationInfoResponse
	backoff := tc.config.FailWaitTime
	for tc.running.Load() {
		var err error
		regInfo, err = tc.registration.Register(tc.integrationInfo.Integration, tc.integrationInfo.Alias)
		if err != nil {
			tc.logger.Error("Registration failed, retrying", zap.Error(err), zap.Duration("backoff", backoff))
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		break
	}

	if regInfo == nil || !tc.running.Load() {
		return
	}

	tc.logger.Info("Registered with Cortex API",
		zap.String("serverUri", regInfo.ServerUri),
	)

	// Render accept file and create RequestExecutor.
	if err := tc.setupExecutor(); err != nil {
		tc.logger.Error("Failed to set up request executor", zap.Error(err))
		return
	}

	// Determine gRPC target. BROKER_SERVER_URL is reused as the gRPC address.
	serverAddr := os.Getenv("BROKER_SERVER_URL")
	if serverAddr == "" {
		serverAddr = regInfo.ServerUri
	}
	// Strip http(s):// if present — gRPC expects host:port.
	serverAddr = stripScheme(serverAddr)

	// Establish gRPC connection.
	dialOpts, dialAddr := tc.buildDialOptions(serverAddr)
	conn, err := grpc.NewClient(dialAddr, dialOpts...)
	if err != nil {
		tc.logger.Error("Failed to connect to gRPC server", zap.String("addr", serverAddr), zap.Error(err))
		return
	}

	tc.mu.Lock()
	tc.conn = conn
	ctx, cancel := context.WithCancel(context.Background())
	tc.parentCtx = ctx
	tc.cancelAll = cancel
	tc.mu.Unlock()

	// Open N tunnel streams.
	client := pb.NewTunnelServiceClient(conn)
	seenServers := make(map[string]bool)

	for i := 0; i < tc.config.TunnelCount; i++ {
		ts := tc.openStream(ctx, client, regInfo.Token, i, seenServers)
		if ts != nil {
			tc.mu.Lock()
			tc.streams = append(tc.streams, ts)
			tc.mu.Unlock()
		}
	}

	tc.logger.Info("gRPC tunnel started",
		zap.Int("streams", len(tc.streams)),
		zap.String("serverAddr", serverAddr),
	)
}

func (tc *tunnelClient) setupExecutor() error {
	af, err := tc.integrationInfo.ToAcceptFile(tc.config, tc.logger)
	if err != nil {
		return fmt.Errorf("error creating accept file: %w", err)
	}

	rendered, err := af.Render(tc.logger)
	if err != nil {
		return fmt.Errorf("error rendering accept file: %w", err)
	}

	// Parse rendered rules.
	af2, err := acceptfile.NewAcceptFile(rendered, tc.config, tc.logger)
	if err != nil {
		return fmt.Errorf("error parsing rendered accept file: %w", err)
	}

	rules := af2.Wrapper().PrivateRules()
	tc.executor = requestexecutor.NewRequestExecutor(rules, tc.httpClient, tc.logger)
	return nil
}

const handshakeTimeout = 30 * time.Second

func (tc *tunnelClient) openStream(
	ctx context.Context,
	client pb.TunnelServiceClient,
	token string,
	index int,
	seenServers map[string]bool,
) *tunnelStream {
	streamCtx, cancel := context.WithCancel(ctx)

	stream, err := client.Tunnel(streamCtx)
	if err != nil {
		tc.logger.Error("Failed to open tunnel stream", zap.Int("index", index), zap.Error(err))
		cancel()
		return nil
	}

	// Cancel the stream if handshake (Send+Recv) takes too long.
	handshakeTimer := time.AfterFunc(handshakeTimeout, func() {
		tc.logger.Warn("Handshake timeout, cancelling stream", zap.Int("index", index))
		cancel()
	})
	defer handshakeTimer.Stop()

	// Send ClientHello.
	hello := &pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_Hello{
			Hello: &pb.ClientHello{
				BrokerToken:    token,
				ClientVersion:  common.ClientVersion,
				TenantId:       os.Getenv("CORTEX_TENANT_ID"),
				Integration:    tc.integrationInfo.Integration.String(),
				Alias:          tc.integrationInfo.Alias,
				InstanceId:     tc.config.InstanceId,
				CortexApiToken: tc.config.CortexApiToken,
			},
		},
	}

	if err := stream.Send(hello); err != nil {
		tc.logger.Error("Failed to send ClientHello", zap.Error(err))
		cancel()
		return nil
	}

	// Receive ServerHello.
	msg, err := stream.Recv()
	if err != nil {
		tc.logger.Error("Failed to receive ServerHello", zap.Error(err))
		cancel()
		return nil
	}

	serverHello := msg.GetHello()
	if serverHello == nil {
		tc.logger.Error("Expected ServerHello, got something else")
		cancel()
		return nil
	}

	// Dedup: if we're already connected to this server, skip.
	if seenServers[serverHello.ServerId] {
		tc.logger.Info("Already connected to this server, skipping duplicate stream",
			zap.String("serverId", serverHello.ServerId),
			zap.Int("index", index),
		)
		cancel()
		return nil
	}
	seenServers[serverHello.ServerId] = true

	tc.logger.Info("Tunnel stream established",
		zap.String("streamId", serverHello.StreamId),
		zap.String("serverId", serverHello.ServerId),
		zap.Int32("heartbeatIntervalMs", serverHello.HeartbeatIntervalMs),
		zap.Int("index", index),
	)

	tc.connectionsActive.WithLabelValues(serverHello.ServerId).Inc()

	ts := &tunnelStream{
		streamID: serverHello.StreamId,
		serverID: serverHello.ServerId,
		cancel:   cancel,
		done:     make(chan struct{}),
	}

	// Create a mutex-protected send function to prevent concurrent Send() calls.
	// Multiple goroutines (heartbeat responses, HTTP response handlers) may send
	// on this stream concurrently.
	sendMu := &sync.Mutex{}
	sendFn := sendFunc(func(msg *pb.TunnelClientMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	})

	// Start request handler goroutine.
	go tc.streamLoop(streamCtx, stream, sendFn, ts, token)

	return ts
}

func (tc *tunnelClient) streamLoop(ctx context.Context, stream pb.TunnelService_TunnelClient, sendFn sendFunc, ts *tunnelStream, token string) {
	assembler := newRequestAssembler()
	defer func() {
		assembler.discardAll()
		tc.connectionsActive.WithLabelValues(ts.serverID).Dec()
		close(ts.done)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				tc.logger.Warn("Stream recv error, will reconnect",
					zap.String("streamId", ts.streamID),
					zap.Error(err),
				)
				tc.reconnectsTotal.WithLabelValues(ts.serverID).Inc()
				go tc.reconnectStream(tc.parentCtx, ts, token)
			}
			return
		}

		switch m := msg.Message.(type) {
		case *pb.TunnelServerMessage_Heartbeat:
			// Respond with heartbeat.
			if err := sendFn(&pb.TunnelClientMessage{
				Message: &pb.TunnelClientMessage_Heartbeat{
					Heartbeat: &pb.Heartbeat{
						TimestampMs: time.Now().UnixMilli(),
					},
				},
			}); err != nil {
				tc.logger.Warn("Failed to send heartbeat response", zap.Error(err))
			}

		case *pb.TunnelServerMessage_HttpRequest:
			assembled := assembler.handleChunk(m.HttpRequest)
			if assembled != nil {
				go tc.handleRequest(sendFn, assembled)
			}

		case *pb.TunnelServerMessage_Hello:
			tc.logger.Warn("Received unexpected ServerHello after handshake")
		}
	}
}

func (tc *tunnelClient) handleRequest(sendFn sendFunc, req *pb.HttpRequest) {
	if tc.executor == nil {
		tc.sendErrorResponse(sendFn, req.RequestId, 503, "executor not ready")
		return
	}

	// Convert headers.
	headers := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		headers[k] = v
	}

	start := time.Now()

	// Use server-provided timeout as context deadline.
	ctx := context.Background()
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	resp, err := tc.executor.Execute(ctx, req.Method, req.Path, headers, req.Body)
	if err != nil {
		tc.logger.Error("Request execution failed",
			zap.String("requestId", req.RequestId),
			zap.String("method", req.Method),
			zap.String("path", req.Path),
			zap.Error(err),
		)
		statusCode := 502
		if err == requestexecutor.ErrNoMatchingRule {
			statusCode = 404
		}
		tc.requestsTotal.WithLabelValues(req.Method, fmt.Sprintf("%d", statusCode)).Inc()
		tc.sendErrorResponse(sendFn, req.RequestId, int32(statusCode), err.Error())
		return
	}

	duration := time.Since(start)
	tc.requestsTotal.WithLabelValues(req.Method, fmt.Sprintf("%d", resp.StatusCode)).Inc()
	tc.requestDuration.WithLabelValues(req.Method).Observe(float64(duration.Milliseconds()))

	// Send response back through tunnel (chunked if needed).
	tc.sendResponse(sendFn, req.RequestId, resp)
}

func (tc *tunnelClient) sendResponse(sendFn sendFunc, requestID string, resp *requestexecutor.ExecutorResponse) {
	if len(resp.Body) <= maxChunkSize {
		if err := sendFn(&pb.TunnelClientMessage{
			Message: &pb.TunnelClientMessage_HttpResponse{
				HttpResponse: &pb.HttpResponse{
					RequestId:  requestID,
					StatusCode: int32(resp.StatusCode),
					Headers:    resp.Headers,
					Body:       resp.Body,
					ChunkIndex: 0,
					IsFinal:    true,
				},
			},
		}); err != nil {
			tc.logger.Warn("Failed to send response",
				zap.String("requestId", requestID),
				zap.Error(err),
			)
		}
		return
	}

	// Chunked response.
	for i := 0; i < len(resp.Body); i += maxChunkSize {
		end := i + maxChunkSize
		if end > len(resp.Body) {
			end = len(resp.Body)
		}
		chunkIndex := int32(i / maxChunkSize)
		isFinal := end == len(resp.Body)

		httpResp := &pb.HttpResponse{
			RequestId:  requestID,
			Body:       resp.Body[i:end],
			ChunkIndex: chunkIndex,
			IsFinal:    isFinal,
		}

		// First chunk includes status code and headers.
		if chunkIndex == 0 {
			httpResp.StatusCode = int32(resp.StatusCode)
			httpResp.Headers = resp.Headers
		}

		if err := sendFn(&pb.TunnelClientMessage{
			Message: &pb.TunnelClientMessage_HttpResponse{
				HttpResponse: httpResp,
			},
		}); err != nil {
			tc.logger.Warn("Failed to send response chunk, aborting remaining chunks",
				zap.String("requestId", requestID),
				zap.Int32("chunkIndex", chunkIndex),
				zap.Error(err),
			)
			return
		}
	}
}

func (tc *tunnelClient) sendErrorResponse(sendFn sendFunc, requestID string, statusCode int32, message string) {
	if err := sendFn(&pb.TunnelClientMessage{
		Message: &pb.TunnelClientMessage_HttpResponse{
			HttpResponse: &pb.HttpResponse{
				RequestId:  requestID,
				StatusCode: statusCode,
				Headers:    map[string]string{"Content-Type": "text/plain"},
				Body:       []byte(message),
				ChunkIndex: 0,
				IsFinal:    true,
			},
		},
	}); err != nil {
		tc.logger.Warn("Failed to send error response",
			zap.String("requestId", requestID),
			zap.Int32("statusCode", statusCode),
			zap.Error(err),
		)
	}
}

func (tc *tunnelClient) reconnectStream(parentCtx context.Context, ts *tunnelStream, token string) {
	// Add jitter to prevent thundering herd.
	jitter := time.Duration(rand.IntN(5000)) * time.Millisecond
	time.Sleep(jitter)

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for attempt := 0; tc.running.Load(); attempt++ {
		// Stop if the parent context was cancelled (e.g. Close() called).
		if parentCtx.Err() != nil {
			return
		}

		tc.logger.Info("Reconnecting tunnel stream",
			zap.String("streamId", ts.streamID),
			zap.Int("attempt", attempt),
		)

		tc.mu.Lock()
		if tc.conn == nil {
			tc.mu.Unlock()
			return
		}
		client := pb.NewTunnelServiceClient(tc.conn)
		tc.mu.Unlock()

		seenServers := make(map[string]bool)
		newStream := tc.openStream(parentCtx, client, token, 0, seenServers)
		if newStream != nil {
			// Replace the old stream entry.
			tc.mu.Lock()
			for i, s := range tc.streams {
				if s == ts {
					tc.streams[i] = newStream
					break
				}
			}
			tc.mu.Unlock()
			return
		}

		// Wait with backoff, but bail if context is cancelled.
		select {
		case <-time.After(backoff):
		case <-parentCtx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (tc *tunnelClient) Restart() error {
	tc.logger.Info("Restarting gRPC tunnel")
	if err := tc.Close(); err != nil {
		tc.logger.Error("Error closing tunnel on restart", zap.Error(err))
	}
	return tc.Start()
}

func (tc *tunnelClient) Close() error {
	if !tc.running.CompareAndSwap(true, false) {
		return nil
	}

	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Cancel all stream contexts.
	if tc.cancelAll != nil {
		tc.cancelAll()
		tc.cancelAll = nil
	}

	// Wait for all streams to finish.
	for _, s := range tc.streams {
		<-s.done
	}
	tc.streams = nil

	// Close gRPC connection.
	if tc.conn != nil {
		tc.conn.Close()
		tc.conn = nil
	}

	tc.logger.Info("gRPC tunnel closed")
	return nil
}

func (tc *tunnelClient) buildDialOptions(targetAddr string) ([]grpc.DialOption, string) {
	opts := []grpc.DialOption{}
	dialAddr := targetAddr

	// Add transport credentials.
	creds := tc.buildTransportCredentials()
	opts = append(opts, grpc.WithTransportCredentials(creds))

	// Check for HTTP proxy configuration.
	proxyURL := tc.getProxyURL(targetAddr)
	if proxyURL != nil {
		tc.logger.Info("Using HTTP proxy for gRPC connection",
			zap.String("proxy", proxyURL.Host),
			zap.String("target", targetAddr),
		)
		dialer := tc.buildProxyDialer(proxyURL)
		opts = append(opts, grpc.WithContextDialer(dialer))

		// Use passthrough scheme to skip local DNS resolution.
		// This ensures the address is passed directly to our custom dialer,
		// which will connect through the proxy and let the proxy resolve the hostname.
		dialAddr = "passthrough:///" + targetAddr
	}

	return opts, dialAddr
}

// getProxyURL returns the proxy URL to use for the target address, or nil if no proxy.
func (tc *tunnelClient) getProxyURL(targetAddr string) *url.URL {
	// Build a fake request to use http.ProxyFromEnvironment
	// This respects HTTP_PROXY, HTTPS_PROXY, and NO_PROXY.
	scheme := "https"
	if tc.config.GrpcInsecure {
		scheme = "http"
	}
	fakeReq, _ := http.NewRequest("GET", fmt.Sprintf("%s://%s/", scheme, targetAddr), nil)
	proxyURL, err := http.ProxyFromEnvironment(fakeReq)
	if err != nil || proxyURL == nil {
		return nil
	}
	return proxyURL
}

// buildProxyDialer returns a context dialer that connects through an HTTP CONNECT proxy.
func (tc *tunnelClient) buildProxyDialer(proxyURL *url.URL) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		// Connect to the proxy.
		proxyAddr := proxyURL.Host
		if proxyURL.Port() == "" {
			proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "8080")
		}

		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to proxy %s: %w", proxyAddr, err)
		}

		// Send HTTP CONNECT request.
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)

		// Add proxy authentication if present.
		if proxyURL.User != nil {
			username := proxyURL.User.Username()
			password, _ := proxyURL.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
			connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
		}

		connectReq += "\r\n"

		if _, err := conn.Write([]byte(connectReq)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to send CONNECT request: %w", err)
		}

		// Read the response.
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to read CONNECT response: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT failed with status %d", resp.StatusCode)
		}

		tc.logger.Debug("HTTP CONNECT tunnel established",
			zap.String("proxy", proxyURL.Host),
			zap.String("target", addr),
		)

		return conn, nil
	}
}

func (tc *tunnelClient) buildTransportCredentials() credentials.TransportCredentials {
	if tc.config.GrpcInsecure {
		return insecure.NewCredentials()
	}

	tlsConfig := &tls.Config{}

	if tc.config.HttpCaCertFilePath != "" {
		caCert, err := os.ReadFile(tc.config.HttpCaCertFilePath)
		if err != nil {
			tc.logger.Error("Failed to read CA cert", zap.Error(err))
			return insecure.NewCredentials()
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	}

	return credentials.NewTLS(tlsConfig)
}

func stripScheme(addr string) string {
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimPrefix(addr, "http://")
	return addr
}

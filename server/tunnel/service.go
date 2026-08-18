package tunnel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FrameHandler is called for every CallFrame received from a client.
// It's used to deliver response frames to the dispatch layer.
type FrameHandler func(streamID string, frame *pb.CallFrame)

// StreamCloseHandler is called when a tunnel stream is closed.
// It's used to fail in-flight calls for the closed stream.
type StreamCloseHandler func(streamID string)

// Service implements the TunnelService gRPC server.
type Service struct {
	pb.UnimplementedTunnelServiceServer

	config             config.Config
	logger             *zap.Logger
	registry           *ClientRegistry
	brokerClient       *broker.Client
	metrics            *metrics.Metrics
	frameHandler       FrameHandler
	streamCloseHandler StreamCloseHandler

	mu sync.RWMutex
}

// NewService creates a new tunnel service.
func NewService(
	cfg config.Config,
	logger *zap.Logger,
	registry *ClientRegistry,
	brokerClient *broker.Client,
	m *metrics.Metrics,
) *Service {
	registry.SetMaxStreamsPerToken(cfg.MaxStreamsPerToken)
	return &Service{
		config:       cfg,
		logger:       logger,
		registry:     registry,
		brokerClient: brokerClient,
		metrics:      m,
	}
}

// SetFrameHandler sets the callback for delivering client call frames
// to the dispatch layer.
func (s *Service) SetFrameHandler(handler FrameHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frameHandler = handler
}

// SetStreamCloseHandler sets the callback for when a tunnel stream closes.
// This is used to fail in-flight calls for the closed stream.
func (s *Service) SetStreamCloseHandler(handler StreamCloseHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCloseHandler = handler
}

// Tunnel implements the bidirectional streaming RPC.
func (s *Service) Tunnel(stream pb.TunnelService_TunnelServer) error {
	// Read ClientHello as the first message.
	firstMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv ClientHello: %w", err)
	}

	hello := firstMsg.GetHello()
	if hello == nil {
		return fmt.Errorf("first message must be ClientHello")
	}

	// Trust model: possession of the broker token is the credential — the
	// token's meaning (tenant, integration, alias) was fixed server-side by
	// the authenticated Cortex registration flow, and the dispatcher
	// addresses this server by token only. Everything else in ClientHello
	// is client-supplied, informational metadata: it feeds logs, metrics,
	// and the BROKER_SERVER notify payload, and MUST NOT feed
	// authorization, routing, or stream-acceptance decisions.
	//
	// Unauthenticated (vs a plain error) lets the client distinguish auth
	// failures from transient network errors and trigger an immediate
	// re-registration with the Cortex API.
	if hello.BrokerToken == "" {
		return status.Error(codes.Unauthenticated, "broker_token is required")
	}

	streamID := uuid.New().String()
	token := broker.NewToken(hello.BrokerToken)

	identity := ClientIdentity{
		TenantID:    hello.TenantId,
		Integration: hello.Integration,
		Alias:       hello.Alias,
		InstanceID:  hello.InstanceId,
	}

	// Debug, not Info: a stream opens whenever a call needs one, so at Info
	// this is the loudest line in the service and says nothing an operator
	// acts on. The transition worth seeing is the token gaining or losing
	// connectivity here, which ClientRegistry logs at Info.
	s.logger.Debug("Client connecting",
		zap.String("tenantId", identity.TenantID),
		zap.String("integration", identity.Integration),
		zap.String("alias", identity.Alias),
		zap.String("instanceId", identity.InstanceID),
		zap.String("clientVersion", hello.ClientVersion),
		zap.String("streamId", streamID),
	)

	// Create stream handle with a context for cancellation.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	sendMu := &sync.Mutex{}
	handle := &StreamHandle{
		StreamID: streamID,
		Send: func(msg *pb.ServerFrame) error {
			sendMu.Lock()
			defer sendMu.Unlock()
			return stream.Send(msg)
		},
		Cancel: cancel,
	}

	// Send ServerHello before registering so the handshake completes
	// before the stream becomes dispatchable. Use sendMu for consistency.
	sendMu.Lock()
	err = stream.Send(&pb.ServerFrame{
		Msg: &pb.ServerFrame_Hello{
			Hello: &pb.ServerHello{
				ServerId:            s.config.ServerID,
				StreamId:            streamID,
				HeartbeatIntervalMs: int32(s.config.HeartbeatInterval.Milliseconds()),
				MaxFrameBytes:       int32(s.config.MaxFrameBytes),
				MaxStreams:          int32(s.config.MaxStreamsPerToken),
			},
		},
	})
	sendMu.Unlock()
	if err != nil {
		return fmt.Errorf("send ServerHello: %w", err)
	}

	// Register in client registry (now safe — handshake is done).
	if err := s.registry.Register(token, identity, handle); err != nil {
		if errors.Is(err, ErrTokenStreamCap) {
			s.logger.Warn("Rejecting stream: token at stream cap",
				zap.String("tenantId", identity.TenantID),
				zap.Int("cap", s.config.MaxStreamsPerToken),
			)
			return status.Error(codes.ResourceExhausted, err.Error())
		}
		s.logger.Error("Failed to register client", zap.Error(err))
		return err
	}
	s.metrics.ConnectionsActive.Update(float64(s.registry.StreamCount()))
	s.metrics.ConnectionsTotal.Inc(1)

	// Start stream duration tracking.
	stopwatch := s.metrics.StreamDuration(identity.TenantID, identity.Integration, identity.Alias)

	// Notify BROKER_SERVER asynchronously (infinite retry).
	go s.notifyClientConnected(ctx, token, hello.InstanceId, hello.ClientVersion)

	// Start heartbeat sender.
	heartbeatDone := make(chan struct{})
	go s.heartbeatSender(ctx, stream, sendMu, heartbeatDone)

	// Track last heartbeat for timeout detection using atomic for goroutine safety.
	var lastHeartbeat atomic.Int64
	lastHeartbeat.Store(time.Now().UnixNano())

	// Start heartbeat timeout monitor goroutine. Enforcement is relaxed
	// while a call is in flight on this stream: the recv loop may be blocked
	// delivering body bytes to a slow consumer, which starves heartbeat
	// reads without the connection being dead. Transport-level gRPC
	// keepalive covers dead TCP during calls.
	go func() {
		timeout := 2 * s.config.HeartbeatInterval
		ticker := time.NewTicker(timeout)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if handle.Busy() {
					// A call is active; frame traffic proves liveness.
					lastHeartbeat.Store(time.Now().UnixNano())
					continue
				}
				last := time.Unix(0, lastHeartbeat.Load())
				if time.Since(last) > timeout {
					s.logger.Warn("Heartbeat timeout — closing stream",
						zap.String("streamId", streamID),
						zap.String("tenantId", identity.TenantID),
						zap.Duration("elapsed", time.Since(last)),
					)
					s.metrics.HeartbeatMissed.Inc(1)
					cancel()
					return
				}
			}
		}
	}()

	// Read loop for client messages.
	//
	// Recv runs in its own goroutine because cancelling ctx cannot interrupt
	// a Recv already in flight. ctx is derived from stream.Context(), and
	// cancelling a derived context does not end the RPC — only returning
	// from this handler does, and that is also what unblocks the pending
	// Recv. Checking ctx.Done() only between Recv calls, as this loop used
	// to, meant a stream the server had decided to drop — heartbeat timeout,
	// registry eviction, dispatcher teardown — stayed open until the client
	// happened to send something, holding its registration with it.
	type recvResult struct {
		msg *pb.ClientFrame
		err error
	}
	recvC := make(chan recvResult)
	go func() {
		for {
			msg, err := stream.Recv()
			select {
			case recvC <- recvResult{msg: msg, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		var r recvResult
		select {
		case <-ctx.Done():
			s.cleanupStream(token, streamID, stopwatch)
			return nil
		case r = <-recvC:
		}

		if r.err != nil {
			s.logger.Info("Client stream closed",
				zap.String("streamId", streamID),
				zap.String("tenantId", identity.TenantID),
				zap.Error(r.err),
			)
			s.cleanupStream(token, streamID, stopwatch)
			return nil
		}

		switch m := r.msg.Msg.(type) {
		case *pb.ClientFrame_Heartbeat:
			lastHeartbeat.Store(time.Now().UnixNano())
			s.metrics.HeartbeatReceived.Inc(1)

		case *pb.ClientFrame_Call:
			// Any call frame proves liveness.
			lastHeartbeat.Store(time.Now().UnixNano())
			s.mu.RLock()
			handler := s.frameHandler
			s.mu.RUnlock()
			if handler != nil {
				handler(streamID, m.Call)
			}

		case *pb.ClientFrame_Hello:
			s.logger.Warn("Received duplicate ClientHello, ignoring",
				zap.String("streamId", streamID),
			)
		}
	}
}

// cleanupStream removes a stream from the registry and notifies BROKER_SERVER.
func (s *Service) cleanupStream(token broker.Token, streamID string, stopwatch interface{ Stop() }) {
	stopwatch.Stop()

	// Fail any in-flight calls for this stream.
	s.mu.RLock()
	closeHandler := s.streamCloseHandler
	s.mu.RUnlock()
	if closeHandler != nil {
		closeHandler(streamID)
	}

	// Fetch identity before unregistering so we can pass clientID to the disconnect notification.
	var clientID string
	if identity := s.registry.GetIdentity(token); identity != nil {
		clientID = identity.InstanceID
	}

	entryRemoved := s.registry.Unregister(token, streamID)
	s.metrics.ConnectionsActive.Update(float64(s.registry.StreamCount()))

	// Only notify BROKER_SERVER if the entire entry was removed (last stream).
	if entryRemoved {
		go s.notifyClientDisconnected(token, clientID)
	}
}

// notifyClientConnected sends client-connected to BROKER_SERVER. The
// client retries transient failures internally; this outer loop adds
// unbounded persistence — registration must eventually land as long as
// the stream lives — with a longer backoff between client-level rounds.
func (s *Service) notifyClientConnected(ctx context.Context, token broker.Token, clientID, clientVersion string) {
	backoff := 5 * time.Second
	maxBackoff := time.Minute

	for {
		err := s.brokerClient.ClientConnected(ctx, token, clientID, map[string]string{
			"broker_client_version": clientVersion,
		})
		if err == nil {
			s.registry.SetBrokerServerRegistered(token)
			// One per stream open, so Debug for the same reason as "Client
			// connecting" above. A failure to notify is still logged loudly below.
			s.logger.Debug("BROKER_SERVER client-connected succeeded",
				zap.String("clientId", clientID),
			)
			return
		}

		s.logger.Warn("BROKER_SERVER client-connected exhausted retries, will try again",
			zap.Error(err),
			zap.Duration("backoff", backoff),
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}

// notifyClientDisconnected sends client-disconnected to BROKER_SERVER.
// Transient-failure retry lives in the client; this is bounded (the
// stream is already gone, so best-effort within a timeout).
func (s *Service) notifyClientDisconnected(token broker.Token, clientID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.brokerClient.ClientDisconnected(ctx, token, clientID); err != nil {
		s.logger.Warn("BROKER_SERVER client-disconnected failed", zap.Error(err))
	}
}

// heartbeatSender periodically sends heartbeat messages to the client.
func (s *Service) heartbeatSender(ctx context.Context, stream pb.TunnelService_TunnelServer, sendMu *sync.Mutex, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendMu.Lock()
			err := stream.Send(&pb.ServerFrame{
				Msg: &pb.ServerFrame_Heartbeat{
					Heartbeat: &pb.Heartbeat{
						TimestampMs: time.Now().UnixMilli(),
					},
				},
			})
			sendMu.Unlock()
			if err != nil {
				s.logger.Debug("Failed to send heartbeat", zap.Error(err))
				return
			}
			s.metrics.HeartbeatSent.Inc(1)
		}
	}
}

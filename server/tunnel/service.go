package tunnel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/metrics"
	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ResponseHandler is called when an HttpResponse is received from a client.
// It's used to deliver responses to pending dispatch requests.
type ResponseHandler func(response *pb.HttpResponse)

// StreamCloseHandler is called when a tunnel stream is closed.
// It's used to fail pending dispatch requests for the closed stream.
type StreamCloseHandler func(streamID string)

// Service implements the TunnelService gRPC server.
type Service struct {
	pb.UnimplementedTunnelServiceServer

	config             config.Config
	logger             *zap.Logger
	registry           *ClientRegistry
	brokerClient       *broker.Client
	metrics            *metrics.Metrics
	responseHandler    ResponseHandler
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
	return &Service{
		config:       cfg,
		logger:       logger,
		registry:     registry,
		brokerClient: brokerClient,
		metrics:      m,
	}
}

// SetResponseHandler sets the callback for delivering HTTP responses
// to the dispatch layer.
func (s *Service) SetResponseHandler(handler ResponseHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseHandler = handler
}

// SetStreamCloseHandler sets the callback for when a tunnel stream closes.
// This is used to fail pending dispatch requests for the closed stream.
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

	// Validate required fields.
	if hello.BrokerToken == "" {
		return fmt.Errorf("broker_token is required")
	}
	if hello.TenantId == "" {
		return fmt.Errorf("tenant_id is required")
	}

	streamID := uuid.New().String()
	token := broker.NewToken(hello.BrokerToken)

	identity := ClientIdentity{
		TenantID:    hello.TenantId,
		Integration: hello.Integration,
		Alias:       hello.Alias,
		InstanceID:  hello.InstanceId,
	}

	s.logger.Info("Client connecting",
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
		Send: func(msg *pb.TunnelServerMessage) error {
			sendMu.Lock()
			defer sendMu.Unlock()
			return stream.Send(msg)
		},
		Cancel: cancel,
	}

	// Send ServerHello before registering so the handshake completes
	// before the stream becomes dispatchable. Use sendMu for consistency.
	sendMu.Lock()
	err = stream.Send(&pb.TunnelServerMessage{
		Message: &pb.TunnelServerMessage_Hello{
			Hello: &pb.ServerHello{
				ServerId:            s.config.ServerID,
				HeartbeatIntervalMs: int32(s.config.HeartbeatInterval.Milliseconds()),
				StreamId:            streamID,
			},
		},
	})
	sendMu.Unlock()
	if err != nil {
		return fmt.Errorf("send ServerHello: %w", err)
	}

	// Register in client registry (now safe — handshake is done).
	if err := s.registry.Register(token, identity, handle); err != nil {
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

	// Start heartbeat timeout monitor goroutine.
	go func() {
		timeout := 2 * s.config.HeartbeatInterval
		ticker := time.NewTicker(timeout)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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
	for {
		select {
		case <-ctx.Done():
			s.cleanupStream(token, streamID, stopwatch)
			return nil
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			s.logger.Info("Client stream closed",
				zap.String("streamId", streamID),
				zap.String("tenantId", identity.TenantID),
				zap.Error(err),
			)
			s.cleanupStream(token, streamID, stopwatch)
			return nil
		}

		switch m := msg.Message.(type) {
		case *pb.TunnelClientMessage_Heartbeat:
			lastHeartbeat.Store(time.Now().UnixNano())
			s.metrics.HeartbeatReceived.Inc(1)

		case *pb.TunnelClientMessage_HttpResponse:
			s.mu.RLock()
			handler := s.responseHandler
			s.mu.RUnlock()
			if handler != nil {
				handler(m.HttpResponse)
			}

		case *pb.TunnelClientMessage_Hello:
			s.logger.Warn("Received duplicate ClientHello, ignoring",
				zap.String("streamId", streamID),
			)
		}
	}
}

// cleanupStream removes a stream from the registry and notifies BROKER_SERVER.
func (s *Service) cleanupStream(token broker.Token, streamID string, stopwatch interface{ Stop() }) {
	stopwatch.Stop()

	// Fail any pending dispatch requests for this stream.
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

// notifyClientConnected sends client-connected to BROKER_SERVER with infinite retry.
func (s *Service) notifyClientConnected(ctx context.Context, token broker.Token, clientID, clientVersion string) {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		err := s.brokerClient.ClientConnected(token, clientID, map[string]string{
			"broker_client_version": clientVersion,
		})
		if err == nil {
			s.registry.SetBrokerServerRegistered(token)
			s.logger.Info("BROKER_SERVER client-connected succeeded",
				zap.String("clientId", clientID),
			)
			return
		}

		s.logger.Warn("BROKER_SERVER client-connected failed, retrying",
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

// notifyClientDisconnected sends client-disconnected to BROKER_SERVER with limited retry.
func (s *Service) notifyClientDisconnected(token broker.Token, clientID string) {
	backoff := time.Second
	for attempt := range 3 {
		err := s.brokerClient.ClientDisconnected(token, clientID)
		if err == nil {
			return
		}
		s.logger.Warn("BROKER_SERVER client-disconnected failed",
			zap.Error(err),
			zap.Int("attempt", attempt+1),
		)
		time.Sleep(backoff)
		backoff *= 2
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
			err := stream.Send(&pb.TunnelServerMessage{
				Message: &pb.TunnelServerMessage_Heartbeat{
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

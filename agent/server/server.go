package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/proto"
	"github.com/cortexapps/axon/server/api"
	"github.com/cortexapps/axon/server/handler"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AxonAgent is the gRPC server implementation for the Cortex Axon Agent
//
// To use it, call:
// * RegisterHandler to register your handlers
// * Dispatch to start a streaming dispatch session that will stream back invocations
// * ReportInvocation to report the result of an invocation
type AxonAgent struct {
	pb.AxonAgentServer
	config              config.AgentConfig
	logger              *zap.Logger
	Manager             handler.Manager
	cortexApiServer     pb.CortexApiServer
	grpcServer          *grpc.Server
	inflightLock        sync.RWMutex
	outstandingRequests map[string]inflightRequest
	historyManager      handler.HistoryManager
	// closed is closed by Close so expiry timers still pending stop instead
	// of finishing invocations against a torn-down agent.
	closed    chan struct{}
	closeOnce sync.Once
}

type inflightRequest struct {
	invocable handler.Invocable
	sentAt    time.Time
}

type Params struct {
	fx.In
	Lifecycle fx.Lifecycle `optional:"true"`
	Logger    *zap.Logger
	Config    config.AgentConfig
	Manager   handler.Manager `optional:"true"`
}

func NewAxonAgent(
	p Params,
) *AxonAgent {
	logger := p.Logger.Named("axon-server")
	agent := &AxonAgent{
		config:              p.Config,
		logger:              logger,
		Manager:             p.Manager,
		cortexApiServer:     api.NewCortexApiServer(logger, p.Config),
		outstandingRequests: make(map[string]inflightRequest),
		historyManager:      handler.NewHistoryManager(p.Config, logger),
		closed:              make(chan struct{}),
	}

	if p.Lifecycle != nil {

		p.Lifecycle.Append(
			fx.Hook{
				OnStart: func(ctx context.Context) error {
					return agent.Start(ctx)
				},
			},
		)
	}

	return agent
}

func (s *AxonAgent) RegisterHandler(ctx context.Context, req *pb.RegisterHandlerRequest) (*pb.RegisterHandlerResponse, error) {

	if s.Manager == nil {
		return nil, fmt.Errorf("handler manager is not initialized")
	}

	id, err := s.Manager.RegisterHandler(req.DispatchId, req.HandlerName, time.Duration(req.TimeoutMs)*time.Millisecond, req.Options...)
	return &pb.RegisterHandlerResponse{Id: id}, err
}

func (s *AxonAgent) UnregisterHandler(ctx context.Context, req *pb.UnregisterHandlerRequest) (*pb.UnregisterHandlerResponse, error) {
	if s.Manager == nil {
		return nil, fmt.Errorf("handler manager is not initialized")
	}

	s.Manager.UnregisterHandler(req.Id)
	return &pb.UnregisterHandlerResponse{}, nil
}

// Dispatch is the method that begins streaming back invocations to the client.
// To use dispatch, call with a DispatchId, which is a unique session identifier for this
// dispatch set. The client will receive invocations for all handlers registered with this
// agent.  When a DispatchResponse is received, the client should call its method then
// ReportInvocation to report the result of the invocation.
func (s *AxonAgent) Dispatch(stream pb.AxonAgent_DispatchServer) error {

	if s.Manager == nil {
		return fmt.Errorf("handler manager is not initialized")
	}

	connected := false
	defer func() {
		if connected {
			s.liveness().RecordStreamDisconnected()
		}
	}()

	firstRequest := true
	for {
		req, err := stream.Recv()
		status, _ := status.FromError(err)
		if err == io.EOF || err == context.Canceled || status.Code() == codes.Canceled {
			return nil
		}

		if err != nil {
			s.logger.Error("failed to receive message", zap.Error(err))
			return err
		}

		if firstRequest {
			s.logger.Info("received dispatch request", zap.String("dispatch-id", req.DispatchId), zap.String("client-version", req.ClientVersion))
			firstRequest = false
			// One connect per stream, not per DispatchRequest received.
			s.liveness().RecordStreamConnected()
			connected = true
		}

		s.sendInvocations(req.DispatchId, stream)
	}

}

func (s *AxonAgent) sendComplete(stream pb.AxonAgent_DispatchServer) error {
	dispatchMessage := &pb.DispatchMessage{
		Type: pb.DispatchMessageType_DISPATCH_MESSAGE_WORK_COMPLETED,
	}
	if err := stream.Send(dispatchMessage); err != nil {
		s.logger.Error("failed to send dispatch message to client", zap.Error(err))
		return err
	}
	return nil
}

func (s *AxonAgent) sendInvocations(dispatchId string, stream pb.AxonAgent_DispatchServer) error {

	s.logger.Info("starting handler set", zap.String("dispatch-id", dispatchId))
	err := s.Manager.Start(dispatchId)
	if err != nil {
		s.logger.Error("failed to start handler set", zap.String("dispatch-id", dispatchId), zap.Error(err))
		return err
	}

	finished := false
	go func() {
		<-stream.Context().Done()
		s.logger.Info("stream context closed", zap.String("dispatch-id", dispatchId), zap.Error(stream.Context().Err()))
		finished = true
	}()

	go func() {
		defer func() {
			s.logger.Info("closing handler set", zap.String("dispatch-id", dispatchId))
			s.Manager.Stop(dispatchId)
		}()

		for !finished {
			invoke, err := s.Manager.Dequeue(stream.Context(), dispatchId, s.config.DequeueWaitTime)
			if err != nil {
				s.logger.Error("failed to dequeue message", zap.Error(err))
				continue
			}

			if invoke == nil {

				if s.Manager.IsFinished() && len(s.outstandingRequests) == 0 {
					s.sendComplete(stream)
					finished = true
				}
				continue
			}

			now := time.Now()
			msg := invoke.ToDispatchInvoke()

			dispatchMessage := &pb.DispatchMessage{
				Type:    pb.DispatchMessageType_DISPATCH_MESSAGE_INVOKE,
				Message: &pb.DispatchMessage_Invoke{Invoke: msg},
			}

			if err := stream.Send(dispatchMessage); err != nil {
				s.logger.Error("failed to send dispatch message to client", zap.Error(err))
				return
			}

			s.setOutstandingRequest(msg.InvocationId, &inflightRequest{
				invocable: invoke,
				sentAt:    now,
			})
			go s.waitForExpiry(msg)
		}
	}()
	return nil
}

func (s *AxonAgent) setOutstandingRequest(id string, req *inflightRequest) {
	s.inflightLock.Lock()
	defer s.inflightLock.Unlock()

	if req == nil {
		delete(s.outstandingRequests, id)
		return
	}
	s.outstandingRequests[id] = *req
}

// describeError renders a reported error for a log line. The code carries the
// only detail some senders set — an SDK timeout reports code "timeout" with no
// message — so printing the message alone leaves "invocation error: " and no
// way to tell what went wrong.
func describeError(e *pb.Error) string {
	code, message := e.GetCode(), e.GetMessage()
	switch {
	case code != "" && message != "":
		return fmt.Sprintf("%s: %s", code, message)
	case code != "":
		return code
	case message != "":
		return message
	}
	return "unknown"
}

// liveness is the manager's tracker, or nil (every method of which is a
// no-op) when there is no manager.
func (s *AxonAgent) liveness() *handler.Liveness {
	if s.Manager == nil {
		return nil
	}
	return s.Manager.Liveness()
}

// ReportInvocation is called by the client to report the result of an invocation, which will
// log the result of an invocation into the history path.
//
// It is SDK-originated only: the agent's own timeout path is expireInvocation.
// That split is what liveness classification rests on. Any report, result or
// error, on time or late, proves the SDK's dispatch loop is reading and
// reporting, so it is recorded before the outstanding lookup, which a late
// report no longer passes.
func (s *AxonAgent) ReportInvocation(ctx context.Context, req *pb.ReportInvocationRequest) (*pb.ReportInvocationResponse, error) {
	s.liveness().RecordSuccess()

	var requestErr error
	if e := req.GetError(); e != nil {
		requestErr = fmt.Errorf("invocation error: %s", describeError(e))
	}
	s.finishInvocation(ctx, req, requestErr)
	return &pb.ReportInvocationResponse{}, nil
}

// waitForExpiry expires msg once its TimeoutMs has elapsed, unless the agent
// closes first.
func (s *AxonAgent) waitForExpiry(msg *pb.DispatchHandlerInvoke) {
	timer := time.NewTimer(time.Duration(msg.TimeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		s.expireInvocation(msg)
	case <-s.closed:
	}
}

// expireInvocation is the agent's timer path: the SDK was sent the
// invocation and never reported within its timeout. The history record is
// the same as a reported timeout, but the invocation completes with
// handler.ErrInvocationTimeout so the manager counts it as agent-side.
func (s *AxonAgent) expireInvocation(msg *pb.DispatchHandlerInvoke) {
	message := fmt.Sprintf("handler did not report a result within %dms", msg.TimeoutMs)
	req := &pb.ReportInvocationRequest{
		HandlerInvoke: msg,
		Message: &pb.ReportInvocationRequest_Error{Error: &pb.Error{
			Code:    "timeout",
			Message: message,
		}},
	}
	s.finishInvocation(context.Background(), req, fmt.Errorf("%w: %s", handler.ErrInvocationTimeout, message))
}

// finishInvocation completes an outstanding invocation with requestErr (nil
// for a result) and writes its history record. Unknown ids are ignored: the
// invocation already finished by the other path.
func (s *AxonAgent) finishInvocation(ctx context.Context, req *pb.ReportInvocationRequest, requestErr error) {
	s.inflightLock.RLock()
	ifr, ok := s.outstandingRequests[req.HandlerInvoke.InvocationId]
	s.inflightLock.RUnlock()

	if !ok {
		return
	}
	defer s.setOutstandingRequest(req.HandlerInvoke.InvocationId, nil)

	requestResult := ""

	if rr := req.GetResult(); rr != nil {
		requestResult = rr.GetValue()
	}
	ifr.invocable.Complete(requestResult, requestErr)

	execution := proto.ReportToExecution(req, ifr.sentAt)
	err := s.historyManager.Write(ctx, execution)
	if err != nil {
		s.logger.Error("failed to write history file", zap.Error(err))
	}
}

// ListHandlers returns a list of all registered handlers
func (s *AxonAgent) ListHandlers(ctx context.Context, req *pb.ListHandlersRequest) (*pb.ListHandlersResponse, error) {

	if s.Manager == nil {
		return &pb.ListHandlersResponse{}, nil
	}

	handlers := s.Manager.ListHandlers()
	resp := &pb.ListHandlersResponse{
		Handlers: make([]*pb.HandlerInfo, 0),
	}
	for _, handler := range handlers {

		var lastInvoked *timestamppb.Timestamp = nil
		if handler.LastInvoked() != nil {
			lastInvoked = timestamppb.New(*handler.LastInvoked())
		}

		resp.Handlers = append(resp.Handlers, &pb.HandlerInfo{
			Id:                         handler.Id(),
			Name:                       handler.Name(),
			DispatchId:                 handler.DispatchId(),
			Options:                    handler.Options(),
			IsActive:                   handler.IsActive(),
			LastInvokedClientTimestamp: lastInvoked,
		})
	}
	return resp, nil
}

// GetHandlerHistory returns the history of a handler
func (s *AxonAgent) GetHandlerHistory(ctx context.Context, req *pb.GetHandlerHistoryRequest) (*pb.GetHandlerHistoryResponse, error) {
	history, err := s.historyManager.GetHistory(ctx, req.HandlerName, req.IncludeLogs, req.Tail)
	if err != nil {
		return nil, err
	}
	resp := &pb.GetHandlerHistoryResponse{
		History: history,
	}
	return resp, err
}

// Start starts the gRPC server and listens for incoming requests
func (s *AxonAgent) Start(ctx context.Context) error {

	if s.grpcServer != nil {
		return fmt.Errorf("server already started")
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.config.GrpcPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	err = s.historyManager.Start()
	if err != nil {
		log.Fatal("failed to start history manager", zap.Error(err))
	}

	s.grpcServer = grpc.NewServer()
	pb.RegisterAxonAgentServer(s.grpcServer, s)

	pb.RegisterCortexApiServer(s.grpcServer, s.cortexApiServer)
	// Register reflection service on gRPC server.
	reflection.Register(s.grpcServer)

	// Set up a channel to listen for interrupt signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Run the server in a goroutine
	go func() {
		log.Printf("server listening at %v", lis.Addr())
		if err := s.grpcServer.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	go func() {
		// Block until we receive an interrupt signal
		<-sigChan
		log.Println("Received interrupt signal. Shutting down server...")

		s.grpcServer.GracefulStop()
	}()

	return nil
}

// Close stops the gRPC server
func (s *AxonAgent) Close() {
	if s.grpcServer != nil {
		s.grpcServer.Stop()
		s.grpcServer = nil
	}
	s.closeOnce.Do(func() { close(s.closed) })
	// Closed, not nilled: a report racing Close still has a manager to call,
	// and a closed manager fails a write rather than panicking.
	s.historyManager.Close()
	if s.Manager != nil {
		s.Manager.Close()
	}
}

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
}

// reportGrace is how long past an invocation's timeout the agent waits for
// the client's report before deciding the client never answered.
var reportGrace = 5 * time.Second

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

	firstRequest := true
	for {
		req, err := stream.Recv()
		status, _ := status.FromError(err)
		if err == io.EOF || err == context.Canceled || status.Code() == codes.Canceled {
			return nil
		}

		if firstRequest {
			s.logger.Info("received dispatch request", zap.String("dispatch-id", req.DispatchId), zap.String("client-version", req.ClientVersion))
			firstRequest = false
		}

		if err != nil {
			s.logger.Error("failed to receive message", zap.Error(err))
			return err
		}

		// Subscribe before any handler starts, so a failure can't happen
		// before the client's DispatchEvents stream connects and be lost.
		if req.FailureEvents {
			s.Manager.SubscribeFailures(req.DispatchId)
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

			if !invoke.ClaimDispatch() {
				// It expired while queued and its failure was already
				// reported; sending it now would run stale work.
				continue
			}

			now := time.Now()
			msg := invoke.ToDispatchInvoke()
			// The client may not read this for a while if every handler is
			// busy, so it gets an absolute deadline rather than only a timeout.
			deadline := now.Add(time.Duration(msg.TimeoutMs)*time.Millisecond + reportGrace)
			msg.Deadline = timestamppb.New(deadline)

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
			go func(requestId string) {
				// The grace lets a client that enforces the timeout itself
				// report it first, so the failure is described once, by the
				// side that saw it.
				<-time.After(time.Until(deadline))
				message := fmt.Sprintf("handler did not report a result within %dms", msg.TimeoutMs)
				completed := s.completeInvocation(context.Background(), &pb.ReportInvocationRequest{
					HandlerInvoke: msg,
					Message: &pb.ReportInvocationRequest_Error{Error: &pb.Error{
						Code:    "timeout",
						Message: message,
					}},
				})
				if completed {
					s.Manager.EmitFailure(msg, &pb.InvocationFailure{
						Reason:  pb.FailureReason_TIMEOUT,
						Source:  pb.FailureSource_RUNTIME,
						Message: message,
					})
				}
			}(msg.InvocationId)
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

// ReportInvocation is called by the client to report the result of an invocation, which will
// log the result of an invocation into the history path.
func (s *AxonAgent) ReportInvocation(ctx context.Context, req *pb.ReportInvocationRequest) (*pb.ReportInvocationResponse, error) {
	s.completeInvocation(ctx, req)
	return &pb.ReportInvocationResponse{}, nil
}

// completeInvocation records the result of an outstanding invocation. It
// returns false when the invocation was already completed by an earlier report.
func (s *AxonAgent) completeInvocation(ctx context.Context, req *pb.ReportInvocationRequest) bool {
	s.inflightLock.Lock()
	ifr, ok := s.outstandingRequests[req.HandlerInvoke.InvocationId]
	delete(s.outstandingRequests, req.HandlerInvoke.InvocationId)
	s.inflightLock.Unlock()

	if !ok {
		return false
	}

	var requestErr error

	if e := req.GetError(); e != nil {
		requestErr = fmt.Errorf("invocation error: %s", describeError(e))
	}
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
	return true
}

// DispatchEvents streams the agent's failure events for a dispatch id. An
// event whose send fails goes back on the queue for the next stream.
func (s *AxonAgent) DispatchEvents(req *pb.DispatchRequest, stream pb.AxonAgent_DispatchEventsServer) error {
	if s.Manager == nil {
		return fmt.Errorf("handler manager is not initialized")
	}

	s.logger.Info("client subscribed to failure events", zap.String("dispatch-id", req.DispatchId))
	s.Manager.SubscribeFailures(req.DispatchId)
	for {
		ev, err := s.Manager.NextFailure(stream.Context(), req.DispatchId)
		if err != nil {
			return nil
		}
		if err := stream.Send(ev); err != nil {
			s.Manager.RequeueFailure(req.DispatchId, ev)
			s.logger.Warn("failed to send failure event, requeued",
				zap.String("dispatch-id", req.DispatchId), zap.Error(err))
			return err
		}
	}
}

// RetryInvocation queues a failed invocation again as its next attempt.
func (s *AxonAgent) RetryInvocation(ctx context.Context, req *pb.RetryInvocationRequest) (*pb.RetryInvocationResponse, error) {
	if s.Manager == nil {
		return nil, fmt.Errorf("handler manager is not initialized")
	}
	if req.Invoke == nil {
		return &pb.RetryInvocationResponse{Error: &pb.Error{Code: "invalid", Message: "invoke is required"}}, nil
	}

	id, err := s.Manager.Retrigger(req.Invoke)
	if err != nil {
		s.logger.Warn("retry rejected",
			zap.String("handler", req.Invoke.HandlerName),
			zap.String("invocation-id", req.Invoke.InvocationId),
			zap.Error(err))
		return &pb.RetryInvocationResponse{Error: &pb.Error{Code: "rejected", Message: err.Error()}}, nil
	}
	return &pb.RetryInvocationResponse{InvocationId: id}, nil
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
	if s.historyManager != nil {
		s.historyManager.Close()
		s.historyManager = nil
	}
	s.Manager.Close()
}

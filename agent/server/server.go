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
	Failures            handler.FailureManager
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
	Manager   handler.Manager        `optional:"true"`
	Failures  handler.FailureManager `optional:"true"`
}

func NewAxonAgent(
	p Params,
) *AxonAgent {
	logger := p.Logger.Named("axon-server")
	agent := &AxonAgent{
		config:              p.Config,
		logger:              logger,
		Manager:             p.Manager,
		Failures:            p.Failures,
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
		if req.FailureEvents && s.Failures != nil {
			s.Failures.Subscribe(req.DispatchId)
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

// drain sends whatever src hands out for a dispatch id until ctx ends or a
// send fails. idle runs whenever src has nothing; returning true ends it.
func drain[T comparable](ctx context.Context, src handler.InvocationSource[T], dispatchId string, wait time.Duration, send func(T) error, idle func() bool) error {
	var none T
	for ctx.Err() == nil {
		item, err := src.Next(ctx, dispatchId, wait)
		if err != nil {
			return err
		}
		if item == none {
			if idle != nil && idle() {
				return nil
			}
			continue
		}
		if err := send(item); err != nil {
			return err
		}
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

	go func() {
		defer func() {
			s.logger.Info("closing handler set", zap.String("dispatch-id", dispatchId))
			s.Manager.Stop(dispatchId)
		}()

		send := func(invoke handler.Invocable) error {
			return stream.Send(&pb.DispatchMessage{
				Type:    pb.DispatchMessageType_DISPATCH_MESSAGE_INVOKE,
				Message: &pb.DispatchMessage_Invoke{Invoke: s.track(invoke)},
			})
		}
		idle := func() bool {
			if s.Manager.IsFinished() && s.outstandingCount() == 0 {
				s.sendComplete(stream)
				return true
			}
			return false
		}
		err := drain(stream.Context(), s.Manager, dispatchId, s.config.DequeueWaitTime, send, idle)
		if err != nil {
			s.logger.Error("failed to send dispatch message to client", zap.Error(err))
		}
	}()
	return nil
}

// track records an invocation as sent and gives it a deadline. If no report
// comes back by then, the agent completes it as a timeout, which it reports
// like any other failure. The grace lets a client that enforces the timeout
// itself report it first, so it is described by the side that saw it.
func (s *AxonAgent) track(invoke handler.Invocable) *pb.DispatchHandlerInvoke {
	now := time.Now()
	msg := invoke.ToDispatchInvoke()
	// The client may not read this for a while if every handler is busy, so
	// it gets an absolute deadline rather than only a timeout.
	deadline := now.Add(time.Duration(msg.TimeoutMs)*time.Millisecond + reportGrace)
	msg.Deadline = timestamppb.New(deadline)

	s.setOutstandingRequest(msg.InvocationId, &inflightRequest{invocable: invoke, sentAt: now})
	time.AfterFunc(time.Until(deadline), func() {
		message := fmt.Sprintf("handler did not report a result within %dms", msg.TimeoutMs)
		s.completeInvocation(context.Background(), &pb.ReportInvocationRequest{
			HandlerInvoke: msg,
			Message:       &pb.ReportInvocationRequest_Error{Error: &pb.Error{Code: "timeout", Message: message}},
			Failure: &pb.InvocationFailure{
				Reason:  pb.FailureReason_TIMEOUT,
				Source:  pb.FailureSource_RUNTIME,
				Message: message,
			},
		})
	})
	return msg
}

func (s *AxonAgent) outstandingCount() int {
	s.inflightLock.RLock()
	defer s.inflightLock.RUnlock()
	return len(s.outstandingRequests)
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

// completeInvocation records the result of an outstanding invocation, once:
// a later report for the same invocation is ignored. A report that carries a
// failure completes the invocation with a FailureError, which is how both
// client and agent failures reach the failure manager.
func (s *AxonAgent) completeInvocation(ctx context.Context, req *pb.ReportInvocationRequest) {
	s.inflightLock.Lock()
	ifr, ok := s.outstandingRequests[req.HandlerInvoke.InvocationId]
	delete(s.outstandingRequests, req.HandlerInvoke.InvocationId)
	s.inflightLock.Unlock()

	if !ok {
		return
	}

	var requestErr error

	if e := req.GetError(); e != nil {
		requestErr = fmt.Errorf("invocation error: %s", describeError(e))
	}
	if f := req.GetFailure(); f != nil {
		requestErr = &handler.FailureError{Failure: f}
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
}

// DispatchEvents sends a client its failed invocations and applies its answer
// to each. One the client hasn't answered when the stream ends is sent again
// on its next stream.
func (s *AxonAgent) DispatchEvents(stream pb.AxonAgent_DispatchEventsServer) error {
	if s.Failures == nil {
		return status.Error(codes.Unimplemented, "failure events are not enabled")
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	dispatchId := first.GetDispatchId()
	logger := s.logger.With(zap.String("dispatch-id", dispatchId))
	logger.Info("client subscribed to failure events", zap.String("client-version", first.GetClientVersion()))
	s.Failures.Subscribe(dispatchId)

	var mu sync.Mutex
	unanswered := map[string]bool{}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for id := range unanswered {
			s.Failures.Requeue(dispatchId, id)
		}
	}()

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			res := req.GetResolution()
			if res == nil {
				continue
			}
			mu.Lock()
			delete(unanswered, res.GetInvocationId())
			mu.Unlock()
			if err := s.Failures.Resolve(dispatchId, res); err != nil {
				logger.Warn("failed to apply failure resolution", zap.Error(err))
			}
		}
	}()

	send := func(ev *pb.DispatchEvent) error {
		mu.Lock()
		unanswered[ev.Invoke.InvocationId] = true
		mu.Unlock()
		return stream.Send(ev)
	}
	if err := drain(ctx, s.Failures, dispatchId, s.config.DequeueWaitTime, send, nil); err != nil {
		logger.Warn("failure event stream ended", zap.Error(err))
		return err
	}
	return nil
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

package axon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Agent is the Go implementation of the Cortex Axon client.  To use it
// you create the agent, register your handlers, then call Run()
type Agent struct {
	DispatchId string

	client grpcClient

	handlers           []*handlerInfo
	registeredHandlers map[string]*handlerInfo

	logger       *zap.Logger
	sleepOnError time.Duration
	done         chan struct{}
}

// NewAxonAgent creates a new AxonAgent with the specified options.  You
// must register your handlers then call Run()
func NewAxonAgent(options ...Option) *Agent {

	ao := defaultAgentOptions()

	for _, opt := range options {
		opt(ao)
	}

	logger, err := ao.loggerConfig.Build()
	if err != nil {
		panic(err)
	}

	a := &Agent{
		DispatchId:   uuid.New().String(),
		logger:       logger,
		sleepOnError: ao.sleepOnError,
		done:         make(chan struct{}),
	}

	a.logger = logger
	a.client = newGrpcClient(ao.host, ao.port, logger)
	a.registeredHandlers = make(map[string]*handlerInfo)
	return a
}

func (a *Agent) getHandlerName(handler Handler) string {
	name := runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name()
	parts := strings.Split(name, ".")
	return parts[len(parts)-1]
}

type registerHandlerOptions struct {
	timeout        time.Duration
	handlerOptions []*pb.HandlerOption
}

type RegisterHandlerOption func(*registerHandlerOptions)

func WithTimeout(timeout time.Duration) RegisterHandlerOption {
	return func(o *registerHandlerOptions) {
		o.timeout = timeout
	}
}

func WithInvokeOption(invokeType pb.HandlerInvokeType, value string) RegisterHandlerOption {
	return func(o *registerHandlerOptions) {
		o.handlerOptions = append(o.handlerOptions,
			&pb.HandlerOption{
				Option: &pb.HandlerOption_Invoke{
					Invoke: &pb.HandlerInvokeOption{
						Type:  invokeType,
						Value: value,
					},
				},
			},
		)
	}
}

type Handler func(HandlerContext) interface{}

type handlerInfo struct {
	dispatchId string
	name       string
	options    []*pb.HandlerOption
	handler    Handler
	timeout    time.Duration
}

// RegisterHandler registeres a handler to be invoked with the specified options.  It
// returns the id of the handler which can be used to unregister it
func (a *Agent) RegisterHandler(handler Handler, invokeOptions ...RegisterHandlerOption) (string, error) {

	name := a.getHandlerName(handler)

	for _, h := range a.handlers {
		if h.name == name {
			return "", fmt.Errorf("handler %s already registered", name)
		}
	}

	opts := &registerHandlerOptions{}

	for _, opt := range invokeOptions {
		opt(opts)
	}

	info := &handlerInfo{
		dispatchId: a.DispatchId,
		name:       name,
		options:    opts.handlerOptions,
		handler:    handler,
		timeout:    opts.timeout,
	}
	a.handlers = append(a.handlers, info)
	return a.registerHandler(info)
}

func (a *Agent) registerHandler(info *handlerInfo) (string, error) {

	stub := a.client.agent()
	if stub == nil {
		return "", fmt.Errorf("failed to create agent connection")
	}

	res, err := stub.RegisterHandler(context.Background(), &pb.RegisterHandlerRequest{
		DispatchId:  a.DispatchId,
		HandlerName: info.name,
		TimeoutMs:   int32(info.timeout.Milliseconds()),
		Options:     info.options,
	})

	if err != nil {
		return "", err
	}
	a.registeredHandlers[res.Id] = info
	return res.Id, nil
}

// UnregisterHandler unregisters a handler by id
func (a *Agent) UnregisterHandler(id string) error {

	stub := a.client.agent()
	if stub == nil {
		return fmt.Errorf("failed to create agent connection")
	}

	_, err := stub.UnregisterHandler(context.Background(), &pb.UnregisterHandlerRequest{
		Id: id,
	})
	if err != nil {
		a.logger.Warn("failed to unregister handler", zap.Error(err))
	}
	// even if we error we want to drop this handler
	delete(a.registeredHandlers, id)
	return err
}

func (a Agent) reregisterHandlers() error {
	a.logger.Warn("reregistering handlers")
	for _, handler := range a.handlers {

		for id, h := range a.registeredHandlers {
			if h.name == handler.name {
				a.UnregisterHandler(id)
			}
		}

		_, err := a.registerHandler(handler)
		if err != nil {
			a.logger.Error("failed to reregister handler", zap.Error(err))
			return err
		}
	}
	return nil
}

const sleepErrorWait = 5 * time.Second

// Run starts the agent and begins dispatching invocations to the registered handlers.
func (a *Agent) Run(ctx context.Context) error {
	exit := false
	reregister := false
	sleepOnError := func(err error) {

		if a.sleepOnError == 0 {
			exit = true
			return
		}

		reregister = true
		a.logger.Error("error in agent, sleeping for "+sleepErrorWait.String(), zap.Error(err))
		time.Sleep(sleepErrorWait)
	}

	runningHandlers := sync.WaitGroup{}

	go func() {
		select {
		case <-ctx.Done():
			exit = true
		case <-a.done:
			exit = true
		}
	}()

	for !exit {

		// aquire an agent and register handlers if needed
		// this allows agent crash/restart to recover
		stub := a.client.agent()
		if stub == nil {
			sleepOnError(fmt.Errorf("failed to create agent connection"))
			continue
		}

		if reregister {
			err := a.reregisterHandlers()

			if err != nil {
				sleepOnError(err)
				continue
			}
		}

		reregister = false

		stream, err := stub.Dispatch(context.Background())
		if err != nil {
			sleepOnError(err)
			continue
		}

		// initiate our dispatch session
		err = stream.Send(&pb.DispatchRequest{
			DispatchId: a.DispatchId,
		})
		if err != nil {
			a.logger.Error("failed to send registration id", zap.Error(err))
			stream.CloseSend()
			sleepOnError(err)
			continue
		}

		err = a.processDispatchStream(ctx, stream, &runningHandlers)

		if err == errWorkCompleted {
			close(a.done)
			break
		}

		if err != nil {
			a.logger.Error("dispatch stream exited", zap.Error(err))
		}

	}
	runningHandlers.Wait()
	return ctx.Err()
}

func (a *Agent) setReportError(report *pb.ReportInvocationRequest, code string, err error) {
	if err == nil && code == "" {
		return
	}
	var message string = ""
	if err != nil {
		message = err.Error()
	}
	report.Message = &pb.ReportInvocationRequest_Error{
		Error: &pb.Error{
			Code:    code,
			Message: message,
		},
	}
}

func (a *Agent) setReportResult(report *pb.ReportInvocationRequest, result interface{}) {
	if result == nil {
		return
	}
	report.Message = &pb.ReportInvocationRequest_Result{
		Result: &pb.InvokeResult{
			Value: fmt.Sprintf("%v", result),
		},
	}
}

const dispatchSleep = 100 * time.Millisecond

var errWorkCompleted = errors.New("work completed")

func (a *Agent) processDispatchStream(ctx context.Context, stream grpc.BidiStreamingClient[pb.DispatchRequest, pb.DispatchMessage], runningHandlers *sync.WaitGroup) error {
	defer stream.CloseSend()
	for {

		req, err := stream.Recv()
		if err != nil {
			status, _ := status.FromError(err)
			if err == io.EOF || status.Code() == codes.Unavailable {
				return nil
			}

			a.logger.Error("failed to receive request", zap.Error(err))
			return err
		}

		if req == nil {
			time.Sleep(dispatchSleep)
			continue
		}

		switch req.Type {
		case pb.DispatchMessageType_DISPATCH_MESSAGE_INVOKE:
			invoke := req.GetInvoke()
			if invoke == nil {
				a.logger.Error("invoke message is nil")
				continue
			}

			timeoutCtx := ctx
			cancel := func() {}
			if invoke.TimeoutMs != 0 {
				timeoutCtx, cancel = context.WithTimeout(context.Background(), time.Millisecond*time.Duration(invoke.TimeoutMs))
			}

			runningHandlers.Add(1)
			go func() {
				defer runningHandlers.Done()
				defer cancel()
				a.invokeHandler(timeoutCtx, invoke)
			}()
		case pb.DispatchMessageType_DISPATCH_MESSAGE_WORK_COMPLETED:
			a.logger.Info("work completed, shutting down")
			return errWorkCompleted
		default:
			a.logger.Warn("Unknown dispatch message type", zap.String("type", req.Type.String()))
		}
	}
}

func (a *Agent) invokeHandler(ctx context.Context, invoke *pb.DispatchHandlerInvoke) {
	handlerInfo, ok := a.registeredHandlers[invoke.HandlerId]
	if !ok {
		a.logger.Error("handler not found", zap.String("handler", invoke.HandlerName))
		return
	}

	done := make(chan struct{})

	report := &pb.ReportInvocationRequest{
		HandlerInvoke:        invoke,
		StartClientTimestamp: timestamppb.Now(),
	}

	go func() {
		apiStub := a.client.api()

		wrapped := zapcore.RegisterHooks(a.logger.Core(), func(entry zapcore.Entry) error {
			report.Logs = append(report.Logs, &pb.Log{
				Level:     entry.Level.CapitalString(),
				Message:   entry.Message,
				Timestamp: timestamppb.New(entry.Time),
			})
			return nil
		})

		loggerFromCore := zap.New(wrapped)

		handlerContext := NewHandlerContext(invoke, ctx, apiStub, loggerFromCore)
		result, duration, err := a.executeHandlerWithRecover(handlerInfo.handler, handlerContext)
		report.DurationMs = int32(duration.Milliseconds())

		if err != nil {
			a.setReportError(report, "unexpected", err)
		} else {
			a.setReportResult(report, result)
		}

		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		a.setReportError(report, "timeout", nil)
	}

	if report.GetError() == nil {
		a.logger.Debug(
			"handler executed",
			zap.String("handler", invoke.HandlerName),
			zap.Duration("duration", time.Duration(report.DurationMs)*time.Millisecond),
		)
	} else {
		a.logger.Error(
			"handler executed with error",
			zap.String("handler", invoke.HandlerName),
			zap.Duration("duration", time.Duration(report.DurationMs)*time.Millisecond),
			zap.Any("error", report.GetError()),
		)
	}
	_, err := a.client.agent().ReportInvocation(context.Background(), report)
	if err != nil {
		a.logger.Error("failed to report invocation", zap.Error(err))
	}
}

func (a *Agent) executeHandlerWithRecover(handler Handler, ctx HandlerContext) (result interface{}, d time.Duration, err error) {
	// on a panic, we should recover and log the error
	now := time.Now()
	defer func() {
		d = time.Since(now)
		err = nil
		if r := recover(); r != nil {
			a.logger.Error("panic in handler", zap.Any("panic", r))
			err = fmt.Errorf("panic in handler: %v", r)
		}
	}()
	result = handler(ctx)
	return result, d, nil
}

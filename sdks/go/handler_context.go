package axon

import (
	"context"
	"sync/atomic"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type handlerContextKey string

const apiKey handlerContextKey = "api"
const logKey handlerContextKey = "log"

type HandlerContext interface {
	context.Context
	Args() map[string]string
	Api() pb.CortexApiClient
	CortexJsonApiCall(method string, path string, jsonBody string) (*pb.CallResponse, error)
	Logger() *zap.Logger
}

type handlerContext struct {
	context.Context
	args  map[string]string
	calls *apiCallTracker
}

// apiCallTracker records whether a Cortex API call was running when the
// handler's deadline hit, which makes the timeout Axon's fault, not the
// handler's.
type apiCallTracker struct {
	inFlight atomic.Int32
	cutOff   atomic.Bool
}

func (t *apiCallTracker) blockedOnApi() bool {
	return t.inFlight.Load() > 0 || t.cutOff.Load()
}

type trackedApiClient struct {
	pb.CortexApiClient
	calls *apiCallTracker
}

func (c trackedApiClient) Call(ctx context.Context, in *pb.CallRequest, opts ...grpc.CallOption) (*pb.CallResponse, error) {
	c.calls.inFlight.Add(1)
	defer c.calls.inFlight.Add(-1)
	resp, err := c.CortexApiClient.Call(ctx, in, opts...)
	if err != nil && ctx.Err() != nil {
		// Set before inFlight drops, so a timeout check never sees neither.
		c.calls.cutOff.Store(true)
	}
	return resp, err
}

func (h *handlerContext) Args() map[string]string {
	if h.args == nil {
		h.args = map[string]string{}
	}
	return h.args
}

func (h *handlerContext) Api() pb.CortexApiClient {
	return h.Value(apiKey).(pb.CortexApiClient)
}

func (h *handlerContext) CortexJsonApiCall(method string, path string, jsonBody string) (*pb.CallResponse, error) {
	return h.Api().Call(h, &pb.CallRequest{
		Method:      method,
		Path:        path,
		Body:        jsonBody,
		ContentType: "application/json",
	})
}

func (h *handlerContext) Logger() *zap.Logger {
	return h.Value(logKey).(*zap.Logger)
}

func NewHandlerContext(invoke *pb.DispatchHandlerInvoke, ctx context.Context, api pb.CortexApiClient, logger *zap.Logger) HandlerContext {

	name := invoke.HandlerName
	logger = logger.With(zap.String("handler-name", name))

	calls := &apiCallTracker{}
	ctx = context.WithValue(ctx, logKey, logger)
	ctx = context.WithValue(ctx, apiKey, pb.CortexApiClient(trackedApiClient{CortexApiClient: api, calls: calls}))

	return &handlerContext{
		Context: ctx,
		args:    invoke.Args,
		calls:   calls,
	}
}

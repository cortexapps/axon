package axon

import (
	"context"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"go.uber.org/zap"
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
	args map[string]string
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

	ctx = context.WithValue(ctx, logKey, logger)
	ctx = context.WithValue(ctx, apiKey, api)

	return &handlerContext{
		Context: ctx,
		args:    invoke.Args,
	}
}

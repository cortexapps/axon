package handler

import (
	"context"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"go.uber.org/zap"
)

// Manager manages a single handler and is responsible for triggering its invocations
type InvokeHandlerEntry struct {
	HandlerEntry
	handler func(context.Context, *pb.DispatchRequest) (*pb.DispatchHandlerInvoke, error)
	manager Manager
	logger  *zap.Logger
}

func NewInvokeHandlerInvoke(entry HandlerEntry, jsonPayload string) Invocable {
	args := map[string]string{
		"body": string(jsonPayload),
	}
	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_INVOKE, args)
	if invoke.Timeout == 0 {
		invoke.Timeout = defaultTimeout
	}
	return invoke
}

func NewInvokeHandlerEntry(
	manager Manager,
	logger *zap.Logger,
	dispatchId string,
	name string,
	timeout time.Duration,
	options ...*pb.HandlerOption,
) HandlerEntry {

	logger.Info("Creating invoke handler", zap.String("name", name))
	handler := func(context.Context, *pb.DispatchRequest) (*pb.DispatchHandlerInvoke, error) {
		return nil, nil
	}
	entry := &InvokeHandlerEntry{
		HandlerEntry: NewHandlerEntry(
			"",
			dispatchId,
			name,
			timeout,
		),
		logger:  logger,
		handler: handler,
		manager: manager,
	}
	return entry
}

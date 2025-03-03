package handler

import (
	"context"
	"net/url"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"go.uber.org/zap"
)

// Manager manages a single handler and is responsible for triggering its invocations
type WebhookHandlerEntry struct {
	HandlerEntry
	handler   func(context.Context, *pb.DispatchRequest) (*pb.DispatchHandlerInvoke, error)
	manager   Manager
	webhookId string
	logger    *zap.Logger
}

func NewWebhookHandlerInvoke(entry HandlerEntry, url *url.URL, payload string, contentType string) HandlerInvoke {
	invoke := HandlerInvoke{
		Id:     entry.Id(),
		Name:   entry.Name(),
		Reason: pb.HandlerInvokeType_WEBHOOK,
		Args: map[string]string{
			"body":         string(payload),
			"content-type": contentType,
			"url":          url.String(),
		},
		Timeout: entry.Timeout(),
	}

	if invoke.Timeout == 0 {
		invoke.Timeout = defaultTimeout
	}
	return invoke
}

/*
*
TODO Cleanup. Shouhdn have a required option of the webhook id
*/
func NewWebhookHandlerEntry(
	manager Manager,
	logger *zap.Logger,
	dispatchId string,
	name string,
	timeout time.Duration,
	options ...*pb.HandlerOption,
) HandlerEntry {

	if len(options) != 1 {
		logger.Panic("Webhook handler must have exactly one option")
	}

	webhookId := options[0].GetInvoke().Value

	logger.Info("Creating webhook handler", zap.String("webhookId", webhookId))
	handler := func(context.Context, *pb.DispatchRequest) (*pb.DispatchHandlerInvoke, error) {
		return nil, nil
	}
	entry := &WebhookHandlerEntry{
		HandlerEntry: NewHandlerEntry(
			"",
			dispatchId,
			name,
			timeout,
		),
		logger:    logger,
		handler:   handler,
		manager:   manager,
		webhookId: webhookId,
	}
	return entry
}

func (h *WebhookHandlerEntry) Tag() string {
	return h.webhookId
}

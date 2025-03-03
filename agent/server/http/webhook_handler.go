package http

import (
	"io"
	"net/http"
	"strings"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/handler"
	"go.uber.org/zap"
)

const webhookPathRoot = "/webhook/"

type webhookHandler struct {
	io.Closer
	config         config.AgentConfig
	logger         *zap.Logger
	handlerManager handler.Manager
}

func NewWebhookHandler(config config.AgentConfig, logger *zap.Logger, handlerManager handler.Manager) RegisterableHandler {

	handler := &webhookHandler{
		config:         config,
		logger:         logger,
		handlerManager: handlerManager,
	}
	return handler
}

func (h *webhookHandler) RegisterRoutes(mux *http.ServeMux) error {
	mux.Handle(webhookPathRoot, h)
	return nil
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.logger.Info("Received webhook", zap.String("path", r.URL.Path))

	switch r.Method {
	case http.MethodPost, http.MethodPut:
		break
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}

	if h.handlerManager == nil {
		h.logger.Error("No handler manager")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// look up the webhook
	path := r.URL.Path[len(webhookPathRoot):]
	pathParts := strings.Split(path, "/")
	if len(pathParts) == 0 {
		h.logger.Error("Invalid webhook path", zap.String("path", r.URL.Path))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	webhookId := pathParts[0]
	entry := h.handlerManager.GetByTag(webhookId)
	if entry == nil {
		h.logger.Error("Webhook not found", zap.String("webhookId", webhookId))
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// once we have the webhook, we just invoke it with the payload.
	contentType := r.Header.Get("Content-Type")
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error("Failed to read body", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = h.handlerManager.Trigger(handler.NewWebhookHandlerInvoke(entry, r.URL, string(bodyBytes), contentType))

	if err != nil {
		h.logger.Error("Failed to trigger webhook", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

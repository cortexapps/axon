package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/handler"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const webhookPathRoot = "/webhook/"

type webhookHandler struct {
	io.Closer
	config          config.AgentConfig
	logger          *zap.Logger
	handlerManager  handler.Manager
	webhookReceived *prometheus.CounterVec
}

func NewWebhookHandler(config config.AgentConfig, logger *zap.Logger, handlerManager handler.Manager, registry *prometheus.Registry) RegisterableHandler {

	handler := &webhookHandler{
		config:         config,
		logger:         logger,
		handlerManager: handlerManager,
		webhookReceived: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "axon_webhook_received",
				Help: "Number of webhooks received",
			},
			[]string{"webhookId", "status"},
		),
	}
	if registry != nil {
		registry.MustRegister(handler.webhookReceived)
	}
	return handler
}

func (h *webhookHandler) RegisterRoutes(mux *mux.Router) error {
	mux.PathPrefix(webhookPathRoot).Handler(h)
	return nil
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.logger.Info("Received webhook", zap.String("path", r.URL.Path))

	var webhookId string

	writeStatus := func(status int) {
		h.webhookReceived.WithLabelValues(webhookId, fmt.Sprintf("%d", status)).Inc()
		w.WriteHeader(status)
	}

	switch r.Method {
	case http.MethodPost, http.MethodPut:
		break
	default:
		writeStatus(http.StatusMethodNotAllowed)
	}

	if h.handlerManager == nil {
		h.logger.Error("No handler manager")
		writeStatus(http.StatusInternalServerError)
		return
	}

	// look up the webhook
	path := r.URL.Path[len(webhookPathRoot):]
	pathParts := strings.Split(path, "/")
	if len(pathParts) == 0 {
		h.logger.Error("Invalid webhook path", zap.String("path", r.URL.Path))
		writeStatus(http.StatusBadRequest)
		return
	}
	webhookId = pathParts[0]

	entry := h.handlerManager.GetByTag(webhookId)
	if entry == nil {
		h.logger.Error("Webhook not found", zap.String("webhookId", webhookId))
		writeStatus(http.StatusNotFound)
		return
	}

	// once we have the webhook, we just invoke it with the payload.
	contentType := r.Header.Get("Content-Type")
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error("Failed to read body", zap.Error(err))
		writeStatus(http.StatusInternalServerError)
		return
	}

	err = h.handlerManager.Trigger(handler.NewWebhookHandlerInvoke(entry, r.URL, string(bodyBytes), contentType))

	if err != nil {
		h.logger.Error("Failed to trigger webhook", zap.Error(err))
		writeStatus(http.StatusInternalServerError)
		return
	}

	response := map[string]any{
		"status":    "ok",
		"webhookId": webhookId,
	}
	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.Error("Failed to marshal response", zap.Error(err))
		writeStatus(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(jsonResponse)
	if err != nil {
		h.logger.Error("Failed to write response", zap.Error(err))
		writeStatus(http.StatusInternalServerError)
		return
	}
	h.logger.Info("Webhook processed successfully", zap.String("webhookId", webhookId))
}

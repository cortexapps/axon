package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const AxonPathRoot = "/__axon"

type axonHandler struct {
	io.Closer
	config config.AgentConfig
	logger *zap.Logger
	client pb.AxonAgentClient
}

func NewAxonHandler(config config.AgentConfig, logger *zap.Logger) RegisterableHandler {

	handler := &axonHandler{
		config: config,
		logger: logger,
	}
	return handler
}

func (h *axonHandler) grpcClient() (pb.AxonAgentClient, error) {
	if h.client == nil {
		conn, err := grpc.NewClient(
			fmt.Sprintf("localhost:%d", h.config.GrpcPort),
			grpc.WithTransportCredentials(insecure.NewCredentials()))

		if err != nil {
			h.logger.Error("failed to create connection to agent", zap.Error(err))
			return nil, err
		}
		h.client = pb.NewAxonAgentClient(conn)
	}
	return h.client, nil
}

func (h *axonHandler) RegisterRoutes(mux *http.ServeMux) error {
	mux.HandleFunc(AxonPathRoot+"/healthcheck", h.healthcheck)
	mux.HandleFunc(AxonPathRoot+"/info", h.info)
	mux.HandleFunc(AxonPathRoot+"/handlers", h.listHandlers)
	mux.HandleFunc(AxonPathRoot+"/handlers/{handler}", h.getHandler)	
	return nil
}

func (h *axonHandler) returnError(err error, w http.ResponseWriter) bool {
	if err == nil {
		return false
	}
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(err.Error()))
	return true
}

func (h *axonHandler) returnJson(obj interface{}, w http.ResponseWriter) {
	json, err := json.Marshal(obj)
	if h.returnError(err, w) {
		return
	}
	w.Header().Add("Content-Type", "application/json")
	w.Write(json)
}

func (h *axonHandler) healthcheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	result := map[string]interface{}{
		"OK": true,
	}
	h.returnJson(result, w)
}

func (h *axonHandler) info(w http.ResponseWriter, r *http.Request) {

	result := &struct {
		Integration string   `json:"integration"`
		Alias       string   `json:"alias"`
		Handlers    []string `json:"handlers"`
		InstanceID  string   `json:"instance_id"`
	}{
		InstanceID:  h.config.InstanceId,
		Integration: h.config.Integration,
		Alias:       h.config.IntegrationAlias,
		Handlers:    []string{},
	}

	handlers, err := h.fetchHandlers(r)
	if h.returnError(err, w) {
		return
	}
	for _, handler := range handlers {
		result.Handlers = append(result.Handlers, handler.Name)
	}

	h.returnJson(result, w)
}

func (h *axonHandler) fetchHandlers(r *http.Request) ([]*pb.HandlerInfo, error) {
	client, err := h.grpcClient()
	if err != nil {
		return nil, err
	}

	result, err := client.ListHandlers(r.Context(), &pb.ListHandlersRequest{})
	if err != nil {
		return nil, err
	}

	if result.Handlers == nil {
		result.Handlers = []*pb.HandlerInfo{}
	}
	return result.Handlers, nil
}

func (h *axonHandler) listHandlers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	handlers, err := h.fetchHandlers(r)
	if h.returnError(err, w) {
		return
	}

	h.returnJson(handlers, w)
}

func (h *axonHandler) getHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	handlerName := r.PathValue("handler")

	client, err := h.grpcClient()
	if h.returnError(err, w) {
		return
	}

	result, err := client.GetHandlerHistory(r.Context(), &pb.GetHandlerHistoryRequest{
		HandlerName: handlerName,
		Tail:        100,
		IncludeLogs: true,
	})
	if h.returnError(err, w) {
		return
	}
	h.returnJson(result.History, w)

}

func (h *axonHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	panic("Don't call me")
}

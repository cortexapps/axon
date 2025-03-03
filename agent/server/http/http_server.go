package http

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// HttpServer handles the webhook + cortex api proxy (an interface for a proxy that forwards requests to the Cortex API)
// which allows us to handle authorization and rate limiting in a central place,
// whether called from GRPC or HTTP.
type Server interface {
	io.Closer
	RegisterHandler(h RegisterableHandler)
	Start(port int) (int, error)
}

type RegisterableHandler interface {
	http.Handler
	RegisterRoutes(mux *http.ServeMux) error
}

func NewHttpServer(logger *zap.Logger) Server {

	server := &httpServer{
		logger: logger,
		mux:    http.NewServeMux(),
	}
	return server
}

type httpServer struct {
	io.Closer
	port     int
	listener net.Listener
	server   *http.Server
	logger   *zap.Logger
	mux      *http.ServeMux
}

func (h *httpServer) RegisterHandler(handler RegisterableHandler) {
	handler.RegisterRoutes(h.mux)
}

func (h *httpServer) Start(port int) (int, error) {

	if h.server != nil {
		panic("Server already started")
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		panic(err)
	}
	go func() {

		h.server = &http.Server{
			Handler: h.mux,
		}

		err := h.server.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	h.listener = ln
	h.port = ln.Addr().(*net.TCPAddr).Port
	return h.port, nil
}

func (h *httpServer) Close() error {
	if h.server != nil {
		h.server.Close()
		h.server = nil
	}
	if h.listener != nil {
		h.port = 0
		return h.listener.Close()
	}
	return nil
}

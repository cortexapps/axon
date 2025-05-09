package http

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
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
	RegisterRoutes(mux *mux.Router) error
}

type ServerOption func(*serverOptions)

type serverOptions struct {
	name     string
	registry *prometheus.Registry
}

func WithName(name string) ServerOption {
	return func(s *serverOptions) {
		s.name = name
	}
}

func WithRegistry(registry *prometheus.Registry) ServerOption {
	return func(s *serverOptions) {
		s.registry = registry
	}
}

func NewHttpServer(logger *zap.Logger, opts ...ServerOption) Server {

	serverOpts := &serverOptions{}
	for _, o := range opts {
		o(serverOpts)
	}

	counterName := "axon_http_requests"
	latencyCounterName := "axon_http_request_latency_seconds"
	if serverOpts.name != "" {
		counterName = fmt.Sprintf("%s_%s", serverOpts.name, counterName)
		latencyCounterName = fmt.Sprintf("%s_%s", serverOpts.name, latencyCounterName)
	}

	router := mux.NewRouter()

	server := &httpServer{
		logger: logger,
		mux:    router,
		requestCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: counterName,
				Help: "Number of HTTP requests",
			},
			[]string{"method", "path", "status"},
		),
		requestLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    latencyCounterName,
				Help:    "Duration of HTTP requests in milliseconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "path", "status"},
		),
	}

	server.mux.Use(server.requestMiddleware)

	if serverOpts.registry != nil {
		serverOpts.registry.MustRegister(server.requestCounter)
		serverOpts.registry.MustRegister(server.requestLatency)
	}
	return server
}

type httpServer struct {
	io.Closer
	port           int
	listener       net.Listener
	server         *http.Server
	logger         *zap.Logger
	mux            *mux.Router
	requestCounter *prometheus.CounterVec
	requestLatency *prometheus.HistogramVec
}

func (h *httpServer) RegisterHandler(handler RegisterableHandler) {
	handler.RegisterRoutes(h.mux)
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.statusCode = code
	rr.ResponseWriter.WriteHeader(code)
}

func (h *httpServer) requestMiddleware(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)
		h.requestCounter.WithLabelValues(r.Method, r.URL.Path, fmt.Sprintf("%d", rec.statusCode)).Inc()
		h.requestLatency.WithLabelValues(r.Method, r.URL.Path, fmt.Sprintf("%d", rec.statusCode)).Observe(duration.Seconds())
		h.logger.Info("HTTP incoming request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rec.statusCode),
			zap.Duration("duration", duration),
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("user_agent", r.UserAgent()),
		)
	})
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

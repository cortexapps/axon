package http

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// HttpServer handles the webhook + cortex api proxy (an interface for a proxy that forwards requests to the Cortex API)
// which allows us to handle authorization and rate limiting in a central place,
// whether called from GRPC or HTTP.
type Server interface {
	io.Closer
	RegisterHandler(h RegisterableHandler)
	Start() (int, error)
	Port() int
}

type RegisterableHandler interface {
	http.Handler
	RegisterRoutes(mux *mux.Router) error
}

type ServerOption func(*serverOptions)

type serverOptions struct {
	name         string
	registry     *prometheus.Registry
	port         int
	loopbackOnly bool
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

func WithPort(port int) ServerOption {
	return func(s *serverOptions) {
		s.port = port
	}
}

// WithLoopbackOnly binds to 127.0.0.1 rather than every interface. Required
// for any server that injects credentials: reachable off-host, it is an open
// proxy that attaches one for whoever asks.
func WithLoopbackOnly() ServerOption {
	return func(s *serverOptions) {
		s.loopbackOnly = true
	}
}

func AsHandler(f any) any {
	return fx.Annotate(
		f,
		fx.As(new(RegisterableHandler)),
		fx.ResultTags(`group:"http_handlers"`),
	)
}

type HttpServerParams struct {
	fx.In
	Lifecycle fx.Lifecycle `optional:"true"`
	Logger    *zap.Logger
	Config    config.AgentConfig
	Handlers  []RegisterableHandler `group:"http_handlers"`
	Registry  *prometheus.Registry  `optional:"true"`
}

func NewHttpServerFx(name string, port int) func(HttpServerParams, ...ServerOption) Server {
	return func(p HttpServerParams, opts ...ServerOption) Server {
		opts = append(opts, WithName(name), WithPort(port))
		return NewHttpServer(p, opts...)
	}
}

func NewHttpServer(p HttpServerParams, opts ...ServerOption) Server {

	if p.Registry != nil {
		opts = append(opts, WithRegistry(p.Registry))
	}

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
		logger: p.Logger,
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
		config:       p.Config,
		loopbackOnly: serverOpts.loopbackOnly,
	}

	server.mux.Use(server.requestMiddleware)

	if serverOpts.registry != nil {
		serverOpts.registry.MustRegister(server.requestCounter)
		serverOpts.registry.MustRegister(server.requestLatency)
	}

	for _, handler := range p.Handlers {
		server.RegisterHandler(handler)
	}
	server.port = serverOpts.port

	if p.Lifecycle != nil {
		p.Lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				port, err := server.Start()
				if err != nil && serverOpts.port != 0 && port != serverOpts.port {
					panic("Port mismatch: server started on a different port than expected")
				}
				p.Logger.Info("HTTP server started", zap.Int("port", serverOpts.port), zap.Error(err))
				return err
			},
			OnStop: func(ctx context.Context) error {
				server.Close()
				return nil
			},
		})
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
	config         config.AgentConfig
	loopbackOnly   bool
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
	if code != http.StatusOK {
		rr.ResponseWriter.WriteHeader(code)
	}
}

// needed for websockets/HTTP2
var _ http.Hijacker = (*responseRecorder)(nil)

func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rr.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("response writer does not support hijacking")
}

func (h *httpServer) requestMiddleware(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}

		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("content-length", int(r.ContentLength)),
		}

		if h.config.VerboseOutput && r.ContentLength > 0 && r.Body != nil {

			br := bufio.NewReader(r.Body)
			bodyBytes, err := io.ReadAll(br)
			if err != nil {

				h.logger.Error("Failed to read request body", zap.Error(err))
				return
			}
			body := string(bodyBytes)
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			fields = append(fields, zap.String("body", body))
		}
		h.logger.Debug("HTTP request ==>",
			fields...,
		)
		next.ServeHTTP(rec, r)
		duration := time.Since(start)
		path := metricPath(r)
		h.requestCounter.WithLabelValues(r.Method, path, fmt.Sprintf("%d", rec.statusCode)).Inc()
		h.requestLatency.WithLabelValues(r.Method, path, fmt.Sprintf("%d", rec.statusCode)).Observe(duration.Seconds())
		h.logger.Info("<== HTTP request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rec.statusCode),
			zap.Duration("duration", duration),
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("user_agent", r.UserAgent()),
		)
	})
}

// unmatchedMetricPath is the "path" metric label used when a request has no
// resolvable route template. It is a fixed value so it can never contribute to
// unbounded label cardinality.
const unmatchedMetricPath = "<unmatched>"

// metricPath returns the value to use for the "path" label of the request
// metrics. It uses the matched mux route template (e.g. "/webhook/" or
// "/webhook/{id}") rather than the raw request path, so that requests carrying
// unique path segments (webhook ids, resource ids, uuids, ...) collapse to a
// single bounded series instead of leaking one permanent counter + histogram
// per distinct URL.
func metricPath(r *http.Request) string {
	if route := mux.CurrentRoute(r); route != nil {
		if tmpl, err := route.GetPathTemplate(); err == nil && tmpl != "" {
			return tmpl
		}
	}
	return unmatchedMetricPath
}

var defaultReadTimeout = time.Second

func (h *httpServer) Start() (int, error) {

	if h.server != nil {
		panic("Server already started")
	}

	// Explicit 127.0.0.1, not "localhost": that also resolves to ::1, where
	// nothing is listening.
	host := ""
	if h.loopbackOnly {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, h.port))
	if err != nil {
		panic(err)
	}
	// construct the server before spawning the serve goroutine and hand it a
	// local reference, so Close (which nils h.server) never races with it
	server := &http.Server{
		Handler:     h.mux,
		ReadTimeout: defaultReadTimeout,
	}
	h.server = server
	go func() {
		err := server.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
	h.listener = ln
	h.port = ln.Addr().(*net.TCPAddr).Port
	return h.port, nil
}

func (h *httpServer) Port() int {
	if h.port == 0 {
		panic("Port called before Start")
	}
	return h.port
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

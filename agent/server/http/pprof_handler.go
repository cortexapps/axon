package http

import (
	"io"
	"net/http"
	"net/http/pprof"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

const PprofPathRoot = "/pprof"

type pprofHandler struct {
	io.Closer
	config config.AgentConfig
	logger *zap.Logger
}

func NewPprofHandler(config config.AgentConfig, logger *zap.Logger) RegisterableHandler {
	return &pprofHandler{
		config: config,
		logger: logger,
	}
}

func (h *pprofHandler) RegisterRoutes(m *mux.Router) error {
	sub := m.PathPrefix(PprofPathRoot).Subrouter()

	// The index page generates relative links to the named profiles, so they
	// resolve under /pprof/. Individual profiles must be registered explicitly
	// since pprof.Index only auto-dispatches under the hardcoded /debug/pprof/.
	sub.HandleFunc("/", pprof.Index)
	sub.HandleFunc("/cmdline", pprof.Cmdline)
	sub.HandleFunc("/profile", pprof.Profile)
	sub.HandleFunc("/symbol", pprof.Symbol)
	sub.HandleFunc("/trace", pprof.Trace)

	for _, name := range []string{"heap", "goroutine", "allocs", "block", "mutex", "threadcreate"} {
		sub.Handle("/"+name, pprof.Handler(name))
	}

	h.logger.Info("pprof profiling endpoint enabled", zap.String("path", PprofPathRoot))

	return nil
}

func (h *pprofHandler) ServeHTTP(_ http.ResponseWriter, _ *http.Request) {
	panic("ServeHTTP should not be called directly")
}

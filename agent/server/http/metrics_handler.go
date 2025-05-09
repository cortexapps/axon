package http

import (
	"io"
	"net/http"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const heartbeatInterval = 5 * time.Second

type metricsHandler struct {
	io.Closer
	config   config.AgentConfig
	logger   *zap.Logger
	registry *prometheus.Registry
	done     chan struct{}
}

func NewPrometheusRegistry() *prometheus.Registry {
	return prometheus.NewRegistry()
}

func NewMetricsHandler(config config.AgentConfig, logger *zap.Logger, registry *prometheus.Registry) RegisterableHandler {

	handler := &metricsHandler{
		config:   config,
		logger:   logger,
		registry: registry,
		done:     make(chan struct{}),
	}

	heartBeatCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "axon_heartbeat",
			Help: "Number of heartbeats received",
		},
		[]string{"integration", "alias", "instance"},
	)
	handler.registry.MustRegister(heartBeatCounter)

	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				heartBeatCounter.WithLabelValues(config.Integration, config.IntegrationAlias, config.InstanceId).Inc()
			case <-handler.done:
				return
			}
		}
	}()

	return handler
}

func (h *metricsHandler) Close() error {
	close(h.done)
	if h.registry != nil {
		h.registry.Unregister(h.registry)
	}
	h.registry = nil
	return nil
}

func (h *metricsHandler) RegisterRoutes(mux *mux.Router) error {
	mux.Handle("/metrics", promhttp.HandlerFor(h.registry, promhttp.HandlerOpts{}))
	return nil
}

func (h *metricsHandler) ServeHTTP(_ http.ResponseWriter, _ *http.Request) {
	panic("ServeHTTP should not be called directly")
}

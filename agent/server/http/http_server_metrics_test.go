package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// routeHandler registers a single route with the given pattern and always
// responds 200. It lets tests drive the request middleware through mux the
// same way production handlers do.
type routeHandler struct {
	register func(m *mux.Router, h http.Handler)
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *routeHandler) RegisterRoutes(m *mux.Router) error {
	h.register(m, h)
	return nil
}

func newMetricsTestServer(t *testing.T, registry *prometheus.Registry, handlers ...RegisterableHandler) *httpServer {
	t.Helper()
	params := HttpServerParams{
		Logger:   zap.NewNop(),
		Config:   config.AgentConfig{},
		Handlers: handlers,
		Registry: registry,
	}
	srv := NewHttpServer(params).(*httpServer)
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// pathLabelValues returns the "path" label value of every series of metricName.
func pathLabelValues(t *testing.T, registry *prometheus.Registry, metricName string) []string {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)

	var values []string
	for _, fam := range families {
		if fam.GetName() != metricName {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "path" {
					values = append(values, lp.GetValue())
				}
			}
		}
	}
	return values
}

// A PathPrefix route (the shape used by the webhook handler) is the source of
// the cardinality explosion: each distinct /webhook/<id> URL used to create its
// own permanent counter + histogram series. All distinct paths must collapse to
// the single registered route template.
func TestRequestMetricsCollapsePrefixRouteToTemplate(t *testing.T) {
	registry := prometheus.NewRegistry()
	srv := newMetricsTestServer(t, registry, &routeHandler{
		register: func(m *mux.Router, h http.Handler) { m.PathPrefix("/webhook/").Handler(h) },
	})

	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	for i := 0; i < 25; i++ {
		resp, err := http.Get(fmt.Sprintf("%s/webhook/id-%d", ts.URL, i))
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	for _, metric := range []string{"axon_http_requests", "axon_http_request_latency_seconds"} {
		require.Equal(t, []string{"/webhook/"}, pathLabelValues(t, registry, metric),
			"%s: distinct request paths must collapse to a single route-template series", metric)
	}
}

// A route with a path variable must record the template (/webhook/{id}), not
// each concrete value.
func TestRequestMetricsCollapseVariableRouteToTemplate(t *testing.T) {
	registry := prometheus.NewRegistry()
	srv := newMetricsTestServer(t, registry, &routeHandler{
		register: func(m *mux.Router, h http.Handler) { m.Handle("/webhook/{id}", h) },
	})

	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	for i := 0; i < 25; i++ {
		resp, err := http.Get(fmt.Sprintf("%s/webhook/id-%d", ts.URL, i))
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	for _, metric := range []string{"axon_http_requests", "axon_http_request_latency_seconds"} {
		require.Equal(t, []string{"/webhook/{id}"}, pathLabelValues(t, registry, metric),
			"%s: distinct request paths must collapse to a single route-template series", metric)
	}
}

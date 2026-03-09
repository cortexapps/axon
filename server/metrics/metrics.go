package metrics

import (
	"net/http"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
)

// Labels used for tagging all metrics.
const (
	LabelServerID    = "server_id"
	LabelTenantID    = "tenant_id"
	LabelIntegration = "integration"
	LabelAlias       = "alias"
	LabelMethod      = "method"
	LabelStatusCode  = "status_code"
	LabelErrorType   = "error_type"
)

// Metrics holds all server-side metric instruments.
type Metrics struct {
	Scope    tally.Scope
	Closer   func()
	Registry *prom.Registry

	ConnectionsActive  tally.Gauge
	ConnectionsTotal   tally.Counter
	HeartbeatSent      tally.Counter
	HeartbeatReceived  tally.Counter
	HeartbeatMissed    tally.Counter
	DispatchInflight    tally.Gauge
	DispatchErrors      tally.Counter
	DispatchBytesSent   tally.Counter
	DispatchBytesRecv   tally.Counter
	AuthFailures        tally.Counter
}

// New creates a new Metrics instance backed by a Prometheus reporter.
func New(serverID string) *Metrics {
	registry := prom.NewRegistry()

	reporter := tallyprom.NewReporter(tallyprom.Options{
		Registerer: registry,
		Gatherer:   registry,
	})

	scope, closer := tally.NewRootScope(tally.ScopeOptions{
		Tags:           map[string]string{LabelServerID: serverID},
		CachedReporter: reporter,
		Prefix:         "tunnel",
		Separator:      tallyprom.DefaultSeparator,
	}, 0)

	m := &Metrics{
		Scope:    scope,
		Closer:   func() { closer.Close() },
		Registry: registry,

		ConnectionsActive:  scope.Gauge("connections.active"),
		ConnectionsTotal:   scope.Counter("connections.total"),
		HeartbeatSent:      scope.Counter("heartbeat.sent"),
		HeartbeatReceived:  scope.Counter("heartbeat.received"),
		HeartbeatMissed:    scope.Counter("heartbeat.missed"),
		DispatchInflight:    scope.Gauge("dispatch.inflight"),
		DispatchErrors:     scope.Counter("dispatch.errors"),
		DispatchBytesSent:  scope.Counter("dispatch.bytes_sent"),
		DispatchBytesRecv:  scope.Counter("dispatch.bytes_received"),
		AuthFailures:       scope.Counter("auth.failures"),
	}

	return m
}

// DispatchCount returns a tagged counter for dispatch operations.
func (m *Metrics) DispatchCount(tenantID, integration, alias, method string, statusCode int) {
	m.Scope.Tagged(map[string]string{
		LabelTenantID:    tenantID,
		LabelIntegration: integration,
		LabelAlias:       alias,
		LabelMethod:      method,
		LabelStatusCode:  http.StatusText(statusCode),
	}).Counter("dispatch.count").Inc(1)
}

// DispatchDuration records dispatch latency.
func (m *Metrics) DispatchDuration(tenantID, integration, alias string, d float64) {
	m.Scope.Tagged(map[string]string{
		LabelTenantID:    tenantID,
		LabelIntegration: integration,
		LabelAlias:       alias,
	}).Histogram("dispatch.duration_ms", tally.DefaultBuckets).RecordValue(d)
}

// DispatchError records a dispatch error.
func (m *Metrics) DispatchError(tenantID, integration, alias, errorType string) {
	m.Scope.Tagged(map[string]string{
		LabelTenantID:    tenantID,
		LabelIntegration: integration,
		LabelAlias:       alias,
		LabelErrorType:   errorType,
	}).Counter("dispatch.errors").Inc(1)
}

// StreamDuration records how long a tunnel stream was alive.
func (m *Metrics) StreamDuration(tenantID, integration, alias string) tally.Stopwatch {
	return m.Scope.Tagged(map[string]string{
		LabelTenantID:    tenantID,
		LabelIntegration: integration,
		LabelAlias:       alias,
	}).Timer("stream.duration_seconds").Start()
}

// Handler returns an HTTP handler for the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

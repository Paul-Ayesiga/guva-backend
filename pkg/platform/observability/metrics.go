package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewMetricsRegistry returns a fresh Prometheus registry with Go runtime
// and process collectors pre-registered, plus an HTTP handler ready to
// mount at /metrics. Each service constructs one of these in main and
// uses it for all of its own collectors.
//
// Why per-service registries (no package-global): different services have
// different scrape labels and we never want unrelated packages
// auto-registering into a single global. The reference service has used
// this pattern from day one — see services/reference/internal/server.
func NewMetricsRegistry() (*prometheus.Registry, http.Handler) {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return r, promhttp.HandlerFor(r, promhttp.HandlerOpts{Registry: r})
}

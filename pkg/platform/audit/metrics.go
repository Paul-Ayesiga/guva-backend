package audit

import "github.com/prometheus/client_golang/prometheus"

// Producer-side audit metrics. RegisterMetrics installs them into the
// caller's Prometheus registry — each service builds its own registry
// (observability.NewMetricsRegistry) and calls audit.RegisterMetrics(r)
// to opt in. The Worker updates the gauge after every drain tick.

var (
	// audit_outbox_unsent_count is the number of rows in audit_outbox
	// whose sent_at is still NULL. Healthy state is near zero; sustained
	// growth means the Worker can't drain to Kafka (broker down, network,
	// bug). The platform-wide alert in alerts.yml fires on this.
	unsentGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "guva",
		Subsystem: "audit",
		Name:      "outbox_unsent_count",
		Help:      "Rows in audit_outbox not yet published to Kafka. Per-producer.",
	}, []string{"producer"})

	// audit_outbox_drain_total counts successful drain batches. Together
	// with the gauge, it tells you "is the Worker even ticking?".
	drainCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "guva",
		Subsystem: "audit",
		Name:      "outbox_drain_total",
		Help:      "Number of audit_outbox drain batches that successfully published. Per-producer.",
	}, []string{"producer"})

	// audit_outbox_drain_errors_total counts drain failures (DB read,
	// Kafka write, mark-sent). High rate paired with growing gauge is
	// the unambiguous "drain is broken" signal.
	drainErrorCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "guva",
		Subsystem: "audit",
		Name:      "outbox_drain_errors_total",
		Help:      "Number of audit_outbox drain attempts that errored. Per-producer.",
	}, []string{"producer"})
)

// RegisterMetrics installs the package's collectors into the given
// registry. Safe to call once per process; calling twice on the same
// registry panics (prometheus collectors are unique-by-name).
func RegisterMetrics(r prometheus.Registerer) {
	r.MustRegister(unsentGauge, drainCounter, drainErrorCounter)
}

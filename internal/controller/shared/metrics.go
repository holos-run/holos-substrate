package shared

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricsNamespace = "holos_controller"

	OutcomeSuccess = "success"
	OutcomeError   = "error"
)

var ReconcileTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "reconcile_total",
		Help:      "Total number of reconciles completed, labeled by API group, resource kind, and outcome.",
	},
	[]string{"group", "kind", "outcome"},
)

func init() {
	metrics.Registry.MustRegister(ReconcileTotal)
}

func OutcomeLabel(err error) string {
	if err != nil {
		return OutcomeError
	}
	return OutcomeSuccess
}

func RecordReconcile(group, kind string, err error) {
	ReconcileTotal.WithLabelValues(group, kind, OutcomeLabel(err)).Inc()
}

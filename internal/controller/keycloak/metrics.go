package keycloak

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/holos-run/holos-paas/internal/keycloak"
)

// Custom controller metrics (AC #3). These supplement controller-runtime's
// built-in per-controller reconcile metrics (controller_runtime_reconcile_total,
// _errors_total, _time_seconds, workqueue depth/latency, …) with a few
// domain-specific collectors so an operator can see reconcile and Keycloak-API
// outcomes per resource kind on the same Prometheus /metrics endpoint the manager
// already serves.
//
// They are registered once into controller-runtime's metrics.Registry (the
// registry backing the manager's metrics server) via init, so importing this
// package — which every reconciler in it already does — is enough to expose them;
// there is no separate wiring step in main.go.
//
// Label cardinality is kept bounded on purpose: kind is one of a small fixed set
// ("instance"/"group"), outcome is "success"/"error", and operation is a fixed
// set of Keycloak client verbs. None are derived from user input, so these
// counters cannot blow up the time-series count. The metric namespace
// (holos_controller) is intentionally shared with internal/controller/quay so all
// the controller's custom series sort together; the metric names differ
// (keycloak_admin_api_requests_total vs quay_api_requests_total) so the two
// packages never register a colliding collector.

// metricsNamespace prefixes every custom collector so they sort together with the
// quay reconciler's series and do not collide with controller-runtime's
// controller_runtime_* series.
const metricsNamespace = "holos_controller"

// Resource-kind label values for the reconcile counter. Kept as constants so a
// reconciler increments with a stable, low-cardinality token rather than an
// ad-hoc string.
const (
	kindInstance = "instance"
	kindGroup    = "group"
)

// Outcome label values shared by the reconcile and Keycloak-API counters.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"
)

// Keycloak Admin-API operation label values. One per logical client operation
// the reconcilers drive, so an operator can see which Keycloak calls fail.
const (
	opAuthenticate          = "authenticate"
	opGetGroupByPath        = "get_group_by_path"
	opEnsureGroupByPath     = "ensure_group_by_path"
	opDeleteGroup           = "delete_group"
	opFindClientByClientID  = "find_client_by_client_id"
	opGetClientRole         = "get_client_role"
	opAssignClientRole      = "assign_client_role"
	opRemoveClientRole      = "remove_client_role"
	opCreateGroupResource   = "create_group_resource"
	opCreateGroupPolicy     = "create_group_policy"
	opCreateScopePermission = "create_scope_permission"
	opDeleteScopePermission = "delete_scope_permission"
)

var (
	// reconcileTotal counts completed reconciles per resource kind and outcome
	// (success vs error). It complements controller-runtime's reconcile_total by
	// splitting on the keycloak.holos.run kind, which the built-in metric labels
	// only by controller name.
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "keycloak_reconcile_total",
			Help:      "Total number of keycloak.holos.run reconciles completed, labeled by resource kind and outcome.",
		},
		[]string{"kind", "outcome"},
	)

	// keycloakAPIRequestsTotal counts Keycloak Admin REST API requests the
	// reconcilers issue, labeled by logical operation and outcome, so Keycloak-side
	// failures (auth, 5xx, conflicts) are observable distinctly from reconcile
	// failures.
	keycloakAPIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "keycloak_admin_api_requests_total",
			Help:      "Total number of Keycloak Admin API requests issued by the controller, labeled by operation and outcome.",
		},
		[]string{"operation", "outcome"},
	)
)

// init registers the custom collectors into controller-runtime's metrics
// Registry so they are served on the manager's /metrics endpoint. MustRegister
// panics on a duplicate registration, which can only happen on a programming
// error (registering the same collector twice), so failing loudly at startup is
// correct.
func init() {
	metrics.Registry.MustRegister(reconcileTotal, keycloakAPIRequestsTotal)
}

// outcomeLabel maps an error to the shared success/error outcome label value.
func outcomeLabel(err error) string {
	if err != nil {
		return outcomeError
	}
	return outcomeSuccess
}

// recordReconcile increments the reconcile counter for a resource kind with the
// outcome derived from err (nil ⇒ success).
func recordReconcile(kind string, err error) {
	reconcileTotal.WithLabelValues(kind, outcomeLabel(err)).Inc()
}

// recordKeycloakAPI increments the Keycloak-API request counter for an operation
// with the outcome derived from err (nil ⇒ success).
func recordKeycloakAPI(operation string, err error) {
	keycloakAPIRequestsTotal.WithLabelValues(operation, outcomeLabel(err)).Inc()
}

// ignoreNotFound maps a Keycloak NotFound error to nil so a GET whose 404 is an
// expected control-flow branch (the create path) records as a successful Keycloak
// request rather than an error. Any other error passes through unchanged.
func ignoreNotFound(err error) error {
	if keycloak.IsNotFound(err) {
		return nil
	}
	return err
}

// ignoreConflict maps a Keycloak Conflict error to nil so a create whose 409 is
// an expected claim-model branch (a racing actor created the object) records as a
// successful Keycloak request rather than an error. Any other error passes through
// unchanged.
func ignoreConflict(err error) error {
	if keycloak.IsConflict(err) {
		return nil
	}
	return err
}

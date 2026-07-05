package quay

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	ctrlshared "github.com/holos-run/holos-paas/internal/controller/shared"
	"github.com/holos-run/holos-paas/internal/quay"
)

// Custom controller metrics supplement controller-runtime's built-in
// per-controller reconcile metrics (controller_runtime_reconcile_total,
// _errors_total, _time_seconds, workqueue depth/latency, …) with a few
// domain-specific collectors so an operator can see reconcile and Quay-API
// outcomes per resource kind on the same Prometheus /metrics endpoint the
// manager already serves.
//
// They are registered once into controller-runtime's metrics.Registry (the
// registry backing the manager's metrics server) via init, so importing this
// package — which both reconcilers in it already do — is enough to expose them;
// there is no separate wiring step in main.go.
//
// Label cardinality is kept bounded on purpose: group/kind/outcome values and
// operation names are fixed strings, never derived from user input. The shared
// reconcile counter lives in internal/controller/shared with a group label; this
// package owns only the Quay API counter.

// metricsNamespace prefixes every custom collector so they sort together and do
// not collide with controller-runtime's controller_runtime_* series.
const metricsNamespace = "holos_controller"

// Resource-kind label values for the reconcile counters. Kept as constants so a
// reconciler increments with a stable, low-cardinality token rather than an
// ad-hoc string.
const (
	kindOrganization = "organization"
	kindRepository   = "repository"
)

const (
	outcomeSuccess = ctrlshared.OutcomeSuccess
	outcomeError   = ctrlshared.OutcomeError
)

var reconcileTotal = ctrlshared.ReconcileTotal

// Quay-API operation label values. One per logical client operation the
// reconcilers drive, so an operator can see which Quay calls fail.
const (
	opGetOrganization         = "get_organization"
	opCreateOrganization      = "create_organization"
	opUpdateOrganization      = "update_organization"
	opDeleteOrganization      = "delete_organization"
	opGetOrganizationRobot    = "get_organization_robot"
	opCreateOrganizationRobot = "create_organization_robot"
	opDeleteOrganizationRobot = "delete_organization_robot"
	opGetRepository           = "get_repository"
	opCreateRepository        = "create_repository"
	opUpdateRepository        = "update_repository"
	opDeleteRepository        = "delete_repository"
	opListNotifications       = "list_notifications"
	opCreateNotification      = "create_notification"
	opDeleteNotification      = "delete_notification"
	opListTeams               = "list_teams"
	opUpsertTeam              = "upsert_team"
	opDeleteTeam              = "delete_team"
	opGetTeamMembers          = "get_team_members"
	opEnableTeamSync          = "enable_team_sync"
	opDisableTeamSync         = "disable_team_sync"
	opListPrototypes          = "list_prototypes"
	opCreatePrototype         = "create_prototype"
	opUpdatePrototype         = "update_prototype"
	opDeletePrototype         = "delete_prototype"
)

var (
	// quayAPIRequestsTotal counts Quay REST API requests the reconcilers issue,
	// labeled by logical operation and outcome, so Quay-side failures (auth,
	// 5xx, conflicts) are observable distinctly from reconcile failures.
	quayAPIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "quay_api_requests_total",
			Help:      "Total number of Quay API requests issued by the controller, labeled by operation and outcome.",
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
	metrics.Registry.MustRegister(quayAPIRequestsTotal)
}

// outcomeLabel maps an error to the shared success/error outcome label value.
func outcomeLabel(err error) string {
	return ctrlshared.OutcomeLabel(err)
}

// recordReconcile increments the reconcile counter for a resource kind with the
// outcome derived from err (nil ⇒ success).
func recordReconcile(kind string, err error) {
	ctrlshared.RecordReconcile("quay", kind, err)
}

// recordQuayAPI increments the Quay-API request counter for an operation with
// the outcome derived from err (nil ⇒ success).
func recordQuayAPI(operation string, err error) {
	quayAPIRequestsTotal.WithLabelValues(operation, outcomeLabel(err)).Inc()
}

// ignoreNotFound maps a Quay NotFound error to nil so a GET whose 404 is an
// expected control-flow branch (the create path) records as a successful Quay
// request rather than an error. Any other error passes through unchanged.
func ignoreNotFound(err error) error {
	return ctrlshared.IgnoreMatching(err, quay.IsNotFound)
}

// ignoreConflict maps a Quay Conflict error to nil so a create whose 409 is an
// expected claim-model branch (a racing actor created the object) records as a
// successful Quay request rather than an error. Any other error passes through
// unchanged.
func ignoreConflict(err error) error {
	return ctrlshared.IgnoreMatching(err, quay.IsConflict)
}

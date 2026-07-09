package keycloak

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	ctrlshared "github.com/holos-run/holos-substrate/internal/controller/shared"
	"github.com/holos-run/holos-substrate/internal/keycloak"
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
// Label cardinality is kept bounded on purpose: group/kind/outcome values and
// operation names are fixed strings, never derived from user input. The shared
// reconcile counter lives in internal/controller/shared with a group label; this
// package owns only the Keycloak Admin API counter.

// metricsNamespace prefixes every custom collector so they sort together with the
// quay reconciler's series and do not collide with controller-runtime's
// controller_runtime_* series.
const metricsNamespace = "holos_controller"

// Resource-kind label values for the reconcile counter. Kept as constants so a
// reconciler increments with a stable, low-cardinality token rather than an
// ad-hoc string.
const (
	kindInstance   = "instance"
	kindGroup      = "group"
	kindMembership = "membership"
	kindUser       = "user"
	kindClient     = "client"
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
	opListGroupClientRoles  = "list_group_client_roles"
	opAssignClientRole      = "assign_client_role"
	opRemoveClientRole      = "remove_client_role"
	opCreateGroupResource   = "create_group_resource"
	opCreateGroupPolicy     = "create_group_policy"
	opCreateScopePermission = "create_scope_permission"
	opFindAuthz             = "find_authz"
	opDeleteScopePermission = "delete_scope_permission"

	// KeycloakUser reconciler operations.
	opFindUserByEmail     = "find_user_by_email"
	opCreateUser          = "create_user"
	opDeleteUser          = "delete_user"
	opListUserGroups      = "list_user_groups"
	opAddUserToGroup      = "add_user_to_group"
	opRemoveUserFromGroup = "remove_user_from_group"
	opCreateFederatedID   = "create_federated_identity"
	opDeleteFederatedID   = "delete_federated_identity"
	opListFederatedIDs    = "list_federated_identities"

	// KeycloakClient reconciler operations.
	opCreateClient           = "create_client"
	opUpdateClient           = "update_client"
	opDeleteClient           = "delete_client"
	opGetClientSecret        = "get_client_secret"
	opCreateClientRole       = "create_client_role"
	opEnsureClientRoleMapper = "ensure_client_role_mapper"
)

var (
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
	metrics.Registry.MustRegister(keycloakAPIRequestsTotal)
}

// outcomeLabel maps an error to the shared success/error outcome label value.
func outcomeLabel(err error) string {
	return ctrlshared.OutcomeLabel(err)
}

// recordReconcile increments the reconcile counter for a resource kind with the
// outcome derived from err (nil ⇒ success).
func recordReconcile(kind string, err error) {
	ctrlshared.RecordReconcile("keycloak", kind, err)
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
	return ctrlshared.IgnoreMatching(err, keycloak.IsNotFound)
}

// ignoreConflict maps a Keycloak Conflict error to nil so a create whose 409 is
// an expected claim-model branch (a racing actor created the object) records as a
// successful Keycloak request rather than an error. Any other error passes through
// unchanged.
func ignoreConflict(err error) error {
	return ctrlshared.IgnoreMatching(err, keycloak.IsConflict)
}

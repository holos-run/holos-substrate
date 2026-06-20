package keycloak

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	"github.com/holos-run/holos-paas/internal/keycloak"
	"github.com/holos-run/holos-paas/internal/referencegrant"
)

// groupFinalizer guards Keycloak-side cleanup: while it is present, deleting the
// KeycloakGroup CR runs the finalizer (which deletes the Keycloak group only when
// this CR created it) before the CR is removed from the API server. Its value is
// the resource's qualified name so it is unambiguous among any other finalizers.
const groupFinalizer = "group.keycloak.holos.run/finalizer"

// adminPermissionsClientID is the clientId of the realm-management client that
// hosts Fine-Grained Admin Permissions v2 (FGAP v2) Authorization Services. The
// group reconciler resolves it once via FindClientByClientID to program custodian
// delegation as authorization objects (a group resource, a group policy, and a
// scope permission) on it (ADR-20, "Custodian delegation — FGAP v2 group scope").
const adminPermissionsClientID = "admin-permissions"

// reservedGroupPrefixes and reservedGroupNames are the platform-reserved Keycloak
// group identities the controller refuses to manage (ADR-20). A KeycloakGroup
// whose path's leaf or any segment hits one of these is rejected with
// Ready=False, reason Reserved, rather than provisioned — so a namespaced tenant
// CR cannot claim or clobber a platform group (e.g. platform-owner) or a
// Keycloak built-in role group.
var (
	reservedGroupPrefixes = []string{"platform-", "platform/"}
	reservedGroupNames    = map[string]bool{
		"authenticated": true,
		"platform":      true,
	}
)

// GroupClient is the seam the KeycloakGroup reconciler drives Keycloak through.
// It is the subset of internal/keycloak.Client's group, client-role, and FGAP v2
// operations the reconciler needs, named as an interface so tests inject a fake
// without HTTP. The concrete *keycloak.Client satisfies it.
type GroupClient interface {
	// GetGroupByPath fetches the group at the given full path; a missing group
	// returns an error for which keycloak.IsNotFound reports true.
	GetGroupByPath(ctx context.Context, path string) (*keycloak.Group, error)
	// EnsureGroupByPath idempotently ensures every node along the path exists and
	// returns the leaf group's UUID. It is the create path: a 409 from a concurrent
	// creator is treated as benign.
	EnsureGroupByPath(ctx context.Context, path string) (string, error)
	// DeleteGroupByPathIfExists deletes the group at the path, treating an
	// already-absent group as success (idempotent) — the finalizer's cleanup.
	DeleteGroupByPathIfExists(ctx context.Context, path string) error

	// FindClientByClientID returns the OIDC client whose clientId matches, or nil
	// when none exists (an absent client is not an error). Used to resolve the
	// consumer client a conferred role is scoped to, and the admin-permissions
	// client hosting FGAP v2.
	FindClientByClientID(ctx context.Context, clientID string) (*keycloak.OIDCClient, error)
	// GetClientRole fetches one client role by name (notably its UUID, needed to
	// assign it to a group); a missing role reports keycloak.IsNotFound.
	GetClientRole(ctx context.Context, clientUUID, roleName string) (*keycloak.ClientRole, error)
	// AssignClientRoleToGroup grants the client role to the group; re-assigning an
	// already-held role is idempotent on Keycloak's side.
	AssignClientRoleToGroup(ctx context.Context, groupID, clientUUID string, role keycloak.ClientRole) error
	// ListGroupClientRoles returns the client roles currently mapped to the group
	// for the given client, so the reconciler can prune roles no longer desired.
	ListGroupClientRoles(ctx context.Context, groupID, clientUUID string) ([]keycloak.ClientRole, error)
	// RemoveClientRoleFromGroup unassigns the client role from the group; removing
	// a role the group does not hold is idempotent on Keycloak's side.
	RemoveClientRoleFromGroup(ctx context.Context, groupID, clientUUID string, role keycloak.ClientRole) error

	// CreateGroupResource registers a group as an FGAP v2 permission resource on
	// the admin-permissions client, returning its UUID. An already-existing
	// resource reports keycloak.IsConflict.
	CreateGroupResource(ctx context.Context, permClientUUID string, resource keycloak.AuthzResource) (string, error)
	// CreateGroupPolicy creates an FGAP v2 group policy naming the custodian
	// group(s), returning its UUID. An already-existing policy reports IsConflict.
	CreateGroupPolicy(ctx context.Context, permClientUUID string, policy keycloak.GroupPolicy) (string, error)
	// CreateScopePermission binds the manage-members/manage-membership scopes over
	// the group resource to the custodian group policy, returning its UUID. An
	// already-existing permission reports IsConflict.
	CreateScopePermission(ctx context.Context, permClientUUID string, permission keycloak.ScopePermission) (string, error)
	// DeleteScopePermissionIfExists deletes an FGAP v2 scope permission or group
	// policy by its UUID (Keycloak's generic policy endpoint deletes either),
	// treating an already-absent object as success — used to prune the delegation
	// for a custodian dropped from spec.custodians.
	DeleteScopePermissionIfExists(ctx context.Context, permClientUUID, permissionID string) error
}

// GroupClientFactory builds a GroupClient from a resolved Keycloak credential,
// the instance URL/realm, and the CA bundle the instance spec carries. The
// default factory returns a real *keycloak.Client; tests substitute a fake.
type GroupClientFactory func(cred *keycloakCredential, url, realm string, caBundle []byte) GroupClient

// NewKeycloakGroupClient is the production GroupClientFactory.
func NewKeycloakGroupClient(cred *keycloakCredential, url, realm string, caBundle []byte) GroupClient {
	return newKeycloakClient(cred, url, realm, caBundle)
}

// Compile-time assertion that the real Keycloak client satisfies the reconciler's
// seam.
var _ GroupClient = (*keycloak.Client)(nil)

// GroupReconciler reconciles a keycloak.holos.run KeycloakGroup against the
// Keycloak realm of its referenced KeycloakInstance: it ensures the nested group
// exists, confers the declared client roles, configures custodian delegation, and
// on delete runs a finalizer that deletes only a group it created. Status follows
// the Gateway-API convention and meaningful transitions emit Events.
type GroupReconciler struct {
	// Client is the manager's cached client for the KeycloakGroup CR and status,
	// and for resolving the referenced KeycloakInstance and KeycloakClient CRs.
	client.Client
	// APIReader is the manager's non-caching reader, used to Get the credential
	// Secret without a cluster-wide Secret cache.
	APIReader client.Reader
	// Recorder emits Kubernetes Events for created/adopted/failed/deleted
	// transitions.
	Recorder record.EventRecorder
	// Namespace is the controller's own namespace, where credential Secrets are
	// resolved. Defaults to DefaultControllerNamespace via controllerNamespace().
	Namespace string
	// NewClient builds the Keycloak client from a resolved credential. Defaults to
	// NewKeycloakGroupClient; tests override it with a fake factory.
	NewClient GroupClientFactory
}

// Reconcile drives a KeycloakGroup toward its desired state. Loop shape: fetch CR
// → ensure finalizer → on delete run Keycloak delete then remove finalizer → else
// reserved-name guard → resolve instance (+ReferenceGrant) → resolve credential →
// ensure/adopt the nested group (claim model) → confer client roles → configure
// custodian delegation → mark Ready. Recoverable errors map to a False condition
// with an actionable reason and a Warning event, and return an error so the
// request requeues with backoff.
func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)
	defer func() { recordReconcile(kindGroup, retErr) }()

	group := &keycloakv1alpha1.KeycloakGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !group.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, group)
	}

	if controllerutil.AddFinalizer(group, groupFinalizer) {
		if err := r.Update(ctx, group); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueImmediately}, nil
	}

	return r.reconcileNormal(ctx, logger, group)
}

// reconcileNormal performs the reserved-name guard, resolves the instance and
// credential, then creates or adopts the Keycloak group and reconciles its roles
// and custodians.
func (r *GroupReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, group *keycloakv1alpha1.KeycloakGroup) (ctrl.Result, error) {
	// Reserved-name guard (ADR-20): never manage a platform-reserved group.
	if reserved, why := isReservedGroupPath(group.Spec.Path); reserved {
		return r.reject(ctx, group, ReasonReserved, why)
	}

	instance, result, err := r.resolveInstance(ctx, group)
	if instance == nil || err != nil {
		return result, err
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, group, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		return r.fail(ctx, group, err)
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Claim model (ADR-20, mirroring ADR-19): a group this CR did not create and
	// does not already own is a Conflict unless spec.adopt opts in. Ownership is
	// recorded by status.Created. GET the group and branch.
	_, getErr := kc.GetGroupByPath(ctx, group.Spec.Path)
	recordKeycloakAPI(opGetGroupByPath, ignoreNotFound(getErr))
	switch {
	case keycloak.IsNotFound(getErr):
		return r.reconcileCreate(ctx, logger, kc, instance, group)
	case getErr == nil:
		return r.reconcileExisting(ctx, logger, kc, instance, group)
	default:
		return r.fail(ctx, group, fmt.Errorf("getting Keycloak group %q: %w", group.Spec.Path, getErr))
	}
}

// reconcileCreate creates the nested group (idempotent ensure-by-path), records
// ownership (status.Created and the immutable status.GroupID), then reconciles
// roles and custodians.
func (r *GroupReconciler) reconcileCreate(ctx context.Context, logger logr.Logger, kc GroupClient, instance *keycloakv1alpha1.KeycloakInstance, group *keycloakv1alpha1.KeycloakGroup) (ctrl.Result, error) {
	groupID, err := kc.EnsureGroupByPath(ctx, group.Spec.Path)
	recordKeycloakAPI(opEnsureGroupByPath, err)
	if err != nil {
		return r.fail(ctx, group, fmt.Errorf("creating Keycloak group %q: %w", group.Spec.Path, err))
	}
	group.Status.Created = true
	group.Status.Adopted = false
	group.Status.GroupID = groupID
	return r.reconcileMembershipThenSucceed(ctx, logger, kc, instance, group, groupID, ReasonCreated,
		fmt.Sprintf("created Keycloak group %q", group.Spec.Path))
}

// reconcileExisting handles a Keycloak group that already exists. When this CR
// already owns it (status.Created) it verifies the group's immutable UUID still
// matches status.GroupID — a mismatch means the original group was deleted and a
// foreign group recreated at the same path, which is a Conflict (never reconciled
// or deleted) rather than a silent seizure. Otherwise it adopts (spec.adopt,
// status.Adopted=true, never deleted on removal) or records a terminal Conflict.
func (r *GroupReconciler) reconcileExisting(ctx context.Context, logger logr.Logger, kc GroupClient, instance *keycloakv1alpha1.KeycloakInstance, group *keycloakv1alpha1.KeycloakGroup) (ctrl.Result, error) {
	groupID, err := r.resolveGroupID(ctx, kc, group)
	if err != nil {
		return r.fail(ctx, group, err)
	}

	// Already owned by this CR: a prior reconcile created it. Confirm the group at
	// the path is still the one we created (UUID match) before reconciling it.
	if group.Status.Created {
		if group.Status.GroupID != "" && group.Status.GroupID != groupID {
			return r.groupReplaced(ctx, logger, group, groupID)
		}
		group.Status.GroupID = groupID
		return r.reconcileMembershipThenSucceed(ctx, logger, kc, instance, group, groupID, ReasonCreated,
			fmt.Sprintf("reconciled Keycloak group %q", group.Spec.Path))
	}

	if group.Spec.Adopt {
		group.Status.Created = false
		group.Status.Adopted = true
		group.Status.GroupID = groupID
		return r.reconcileMembershipThenSucceed(ctx, logger, kc, instance, group, groupID, ReasonAdopted,
			fmt.Sprintf("adopted existing Keycloak group %q", group.Spec.Path))
	}

	message := fmt.Sprintf("Keycloak group %q already exists and was not created by this resource; set spec.adopt to claim it", group.Spec.Path)
	return r.recordConflict(ctx, logger, group, message)
}

// groupReplaced records a Conflict when the group at the spec path no longer
// carries the UUID this CR created (status.GroupID) — the original group was
// deleted and a foreign one recreated at the same path. The replacement is never
// reconciled into or deleted; an operator must resolve the collision.
func (r *GroupReconciler) groupReplaced(ctx context.Context, logger logr.Logger, group *keycloakv1alpha1.KeycloakGroup, actualID string) (ctrl.Result, error) {
	message := fmt.Sprintf("Keycloak group %q now has UUID %q but this resource created UUID %q; the group was replaced out of band and is not reconciled or deleted", group.Spec.Path, actualID, group.Status.GroupID)
	return r.recordConflict(ctx, logger, group, message)
}

// recordConflict sets a terminal Conflict condition and emits a Warning, writing
// status only on a change so an already-recorded conflict does not spin a watch
// loop.
func (r *GroupReconciler) recordConflict(ctx context.Context, logger logr.Logger, group *keycloakv1alpha1.KeycloakGroup, message string) (ctrl.Result, error) {
	changed := setConflict(&group.Status.Conditions, message, group.Generation)
	changed = changed || group.Status.ObservedGeneration != group.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(group, corev1.EventTypeWarning, ReasonConflict, message)
	logger.Info("KeycloakGroup conflict", "path", group.Spec.Path)
	if err := r.updateStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// resolveGroupID looks up the leaf group's UUID by its full path.
func (r *GroupReconciler) resolveGroupID(ctx context.Context, kc GroupClient, group *keycloakv1alpha1.KeycloakGroup) (string, error) {
	g, err := kc.GetGroupByPath(ctx, group.Spec.Path)
	recordKeycloakAPI(opGetGroupByPath, err)
	if err != nil {
		return "", fmt.Errorf("resolving Keycloak group %q: %w", group.Spec.Path, err)
	}
	return g.ID, nil
}

// reconcileMembershipThenSucceed reconciles the declared client roles and
// custodian delegation to their desired sets (assigning new ones AND pruning ones
// dropped from the spec), then marks the resource Ready. It is the single funnel
// every success path runs so roles and custodians are reconciled only after the
// group itself is provisioned. The pruning is what makes conferral
// reconcile-to-desired-set rather than add-only: a role or custodian removed from
// the spec is actively unassigned in Keycloak so a downgrade in Kubernetes does
// not leave a stale privilege active.
func (r *GroupReconciler) reconcileMembershipThenSucceed(ctx context.Context, logger logr.Logger, kc GroupClient, instance *keycloakv1alpha1.KeycloakInstance, group *keycloakv1alpha1.KeycloakGroup, groupID, reason, message string) (ctrl.Result, error) {
	if err := r.conferClientRoles(ctx, kc, group, groupID); err != nil {
		return r.fail(ctx, group, err)
	}
	if err := r.configureCustodians(ctx, kc, group, groupID); err != nil {
		return r.fail(ctx, group, err)
	}
	return r.succeed(ctx, logger, group, reason, message)
}

// conferClientRoles reconciles the group's client-role assignments to the desired
// set declared in spec.clientRoles: it assigns every declared (clientRef, role)
// and unassigns any role this CR previously managed (recorded in
// status.managedClientRoles) that is no longer desired — so a role removed from
// the spec is actively revoked rather than left active (the add-only gap). Each
// declared entry resolves the referenced KeycloakClient CR to its Keycloak
// clientId, finds the OIDC client, gets the named client role, and assigns it (the
// join that makes a member of the group hold the client role the client's role
// mapper emits into the groups claim, ADR-20). status.managedClientRoles is
// rewritten to the new desired set so the next reconcile knows what it owns.
func (r *GroupReconciler) conferClientRoles(ctx context.Context, kc GroupClient, group *keycloakv1alpha1.KeycloakGroup, groupID string) error {
	// Resolve the desired roles, keyed "<clientId>/<role>", to the OIDC client UUID
	// and the role representation needed to assign or remove them.
	type roleTarget struct {
		clientUUID string
		role       keycloak.ClientRole
	}
	desired := map[string]roleTarget{}
	desiredKeys := make([]string, 0, len(group.Spec.ClientRoles))
	for _, ref := range group.Spec.ClientRoles {
		clientID, err := r.resolveClientID(ctx, group.Namespace, ref.ClientRef)
		if err != nil {
			return err
		}
		oidc, err := kc.FindClientByClientID(ctx, clientID)
		recordKeycloakAPI(opFindClientByClientID, err)
		if err != nil {
			return fmt.Errorf("finding Keycloak client %q for role %q: %w", clientID, ref.Role, err)
		}
		if oidc == nil {
			return fmt.Errorf("no Keycloak client %q (from clientRef %q) exists", clientID, ref.ClientRef)
		}
		role, err := kc.GetClientRole(ctx, oidc.ID, ref.Role)
		recordKeycloakAPI(opGetClientRole, err)
		if err != nil {
			return fmt.Errorf("getting client role %q on Keycloak client %q: %w", ref.Role, clientID, err)
		}
		key := clientID + "/" + ref.Role
		desired[key] = roleTarget{clientUUID: oidc.ID, role: *role}
		desiredKeys = append(desiredKeys, key)
	}

	// Assign every desired role (idempotent on Keycloak's side).
	for _, key := range desiredKeys {
		t := desired[key]
		assignErr := kc.AssignClientRoleToGroup(ctx, groupID, t.clientUUID, t.role)
		recordKeycloakAPI(opAssignClientRole, assignErr)
		if assignErr != nil {
			return fmt.Errorf("assigning client role %q to Keycloak group %q: %w", t.role.Name, group.Spec.Path, assignErr)
		}
	}

	// Prune roles this CR previously managed but that are no longer desired. Each
	// stale entry is "<clientId>/<role>"; resolve the client and remove the role.
	for _, prev := range group.Status.ManagedClientRoles {
		if _, ok := desired[prev]; ok {
			continue
		}
		clientID, roleName, ok := splitManagedRole(prev)
		if !ok {
			continue
		}
		oidc, err := kc.FindClientByClientID(ctx, clientID)
		recordKeycloakAPI(opFindClientByClientID, err)
		if err != nil {
			return fmt.Errorf("finding Keycloak client %q to prune role %q: %w", clientID, roleName, err)
		}
		if oidc == nil {
			// The client is gone; the role mapping went with it. Nothing to prune.
			continue
		}
		role, err := kc.GetClientRole(ctx, oidc.ID, roleName)
		recordKeycloakAPI(opGetClientRole, ignoreNotFound(err))
		if keycloak.IsNotFound(err) {
			// The role no longer exists; nothing to remove.
			continue
		}
		if err != nil {
			return fmt.Errorf("getting client role %q to prune on Keycloak client %q: %w", roleName, clientID, err)
		}
		rmErr := kc.RemoveClientRoleFromGroup(ctx, groupID, oidc.ID, *role)
		recordKeycloakAPI(opRemoveClientRole, rmErr)
		if rmErr != nil {
			return fmt.Errorf("unassigning stale client role %q from Keycloak group %q: %w", roleName, group.Spec.Path, rmErr)
		}
	}

	group.Status.ManagedClientRoles = desiredKeys
	return nil
}

// splitManagedRole splits a "<clientId>/<role>" managed-role key back into its
// clientId and role. clientIds are URLs containing slashes, so it splits on the
// LAST slash (the role name carries none). It reports false for a malformed key.
func splitManagedRole(key string) (clientID, role string, ok bool) {
	idx := strings.LastIndex(key, "/")
	if idx <= 0 || idx == len(key)-1 {
		return "", "", false
	}
	return key[:idx], key[idx+1:], true
}

// resolveClientID resolves a clientRef (a KeycloakClient object name in namespace)
// to the underlying Keycloak clientId (the KeycloakClient's spec.clientId), so the
// reference stays a valid Kubernetes object name even though the Keycloak clientId
// is a URL (ADR-20, ClientRoleReference).
func (r *GroupReconciler) resolveClientID(ctx context.Context, namespace, clientRef string) (string, error) {
	kclient := &keycloakv1alpha1.KeycloakClient{}
	key := types.NamespacedName{Namespace: namespace, Name: clientRef}
	if err := r.Get(ctx, key, kclient); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("KeycloakClient %s/%s referenced by clientRef does not exist", namespace, clientRef)
		}
		return "", fmt.Errorf("resolving KeycloakClient %s/%s: %w", namespace, clientRef, err)
	}
	if kclient.Spec.ClientID == "" {
		return "", fmt.Errorf("KeycloakClient %s/%s has an empty spec.clientId", namespace, clientRef)
	}
	return kclient.Spec.ClientID, nil
}

// managedCustodianSep separates the fields of a status.managedCustodians entry,
// which records "<custodianPath>|<permissionID>|<policyID>" so a custodian dropped
// from spec.custodians can have its scope permission and group policy deleted by
// id on the next reconcile.
const managedCustodianSep = "|"

// configureCustodians reconciles FGAP v2 custodian delegation to the desired set
// declared in spec.custodians. For each declared custodian it registers this group
// as a permission resource on the admin-permissions client, creates a group policy
// naming the custodian group, and binds the manage-members/manage-membership scopes
// to that policy via a scope permission — so a custodian manages this group's
// membership without realm-admin rights (ADR-20). It then prunes the delegation
// (scope permission + group policy) for any custodian this CR previously managed
// (recorded in status.managedCustodians) that is no longer desired, so a custodian
// removed from the spec can no longer manage membership (the add-only gap).
// status.managedCustodians is rewritten to the new desired set.
//
// The creates tolerate a 409 (a prior reconcile already programmed them) as
// success; on that conflict path the object's UUID is unknown, so the created id
// recorded for pruning may be empty. The prune therefore deletes by id only when
// one was captured — the common case where this controller created the objects.
func (r *GroupReconciler) configureCustodians(ctx context.Context, kc GroupClient, group *keycloakv1alpha1.KeycloakGroup, groupID string) error {
	prev := parseManagedCustodians(group.Status.ManagedCustodians)

	// When the spec declares custodians, ensure the delegation objects exist.
	var permClient *keycloak.OIDCClient
	if len(group.Spec.Custodians) > 0 || len(prev) > 0 {
		c, err := kc.FindClientByClientID(ctx, adminPermissionsClientID)
		recordKeycloakAPI(opFindClientByClientID, err)
		if err != nil {
			return fmt.Errorf("finding the %q client for custodian delegation: %w", adminPermissionsClientID, err)
		}
		if c == nil {
			if len(group.Spec.Custodians) > 0 {
				return fmt.Errorf("the %q client is not present; FGAP v2 must be enabled to configure custodians", adminPermissionsClientID)
			}
			// No spec custodians and the perm client is gone: nothing left to prune.
			group.Status.ManagedCustodians = nil
			return nil
		}
		permClient = c
	}

	desired := map[string]managedCustodian{}
	desiredOrder := make([]string, 0, len(group.Spec.Custodians))
	if len(group.Spec.Custodians) > 0 {
		resourceID, err := kc.CreateGroupResource(ctx, permClient.ID, keycloak.AuthzResource{
			Name:   group.Spec.Path,
			Type:   "Groups",
			Scopes: []keycloak.AuthzScope{{Name: keycloak.ScopeManageMembers}, {Name: keycloak.ScopeManageMembership}},
		})
		recordKeycloakAPI(opCreateGroupResource, ignoreConflict(err))
		if err != nil && !keycloak.IsConflict(err) {
			return fmt.Errorf("registering FGAP resource for Keycloak group %q: %w", group.Spec.Path, err)
		}

		for _, custodian := range group.Spec.Custodians {
			custodianGroup, err := kc.GetGroupByPath(ctx, custodian.Path)
			recordKeycloakAPI(opGetGroupByPath, err)
			if err != nil {
				return fmt.Errorf("resolving custodian group %q for Keycloak group %q: %w", custodian.Path, group.Spec.Path, err)
			}

			policyName := fmt.Sprintf("holos:custodian:%s:%s", group.Spec.Path, custodian.Path)
			policyID, err := kc.CreateGroupPolicy(ctx, permClient.ID, keycloak.GroupPolicy{
				Name:   policyName,
				Type:   "group",
				Groups: []keycloak.GroupPolicyMember{{ID: custodianGroup.ID}},
			})
			recordKeycloakAPI(opCreateGroupPolicy, ignoreConflict(err))
			if err != nil && !keycloak.IsConflict(err) {
				return fmt.Errorf("creating custodian policy for Keycloak group %q: %w", group.Spec.Path, err)
			}

			permID, permErr := kc.CreateScopePermission(ctx, permClient.ID, keycloak.ScopePermission{
				Name:      fmt.Sprintf("holos:custodian-perm:%s:%s", group.Spec.Path, custodian.Path),
				Resources: []string{idOrName(resourceID, group.Spec.Path)},
				Scopes:    []string{keycloak.ScopeManageMembers, keycloak.ScopeManageMembership},
				Policies:  []string{idOrName(policyID, policyName)},
			})
			recordKeycloakAPI(opCreateScopePermission, ignoreConflict(permErr))
			if permErr != nil && !keycloak.IsConflict(permErr) {
				return fmt.Errorf("binding custodian permission for Keycloak group %q: %w", group.Spec.Path, permErr)
			}

			mc := managedCustodian{path: custodian.Path, permID: permID, policyID: policyID}
			// Preserve previously-captured ids across a later reconcile whose create
			// returned a 409 (empty id) — the prune needs them to delete by id.
			if old, ok := prev[custodian.Path]; ok {
				if mc.permID == "" {
					mc.permID = old.permID
				}
				if mc.policyID == "" {
					mc.policyID = old.policyID
				}
			}
			desired[custodian.Path] = mc
			desiredOrder = append(desiredOrder, custodian.Path)
		}
	}

	// Prune the delegation for custodians no longer desired: delete the scope
	// permission first (it references the policy), then the group policy. Both go
	// through the generic policy-delete endpoint and tolerate an already-absent id.
	for path, old := range prev {
		if _, ok := desired[path]; ok {
			continue
		}
		if old.permID != "" {
			err := kc.DeleteScopePermissionIfExists(ctx, permClient.ID, old.permID)
			recordKeycloakAPI(opDeleteScopePermission, err)
			if err != nil {
				return fmt.Errorf("pruning custodian permission for Keycloak group %q: %w", group.Spec.Path, err)
			}
		}
		if old.policyID != "" {
			err := kc.DeleteScopePermissionIfExists(ctx, permClient.ID, old.policyID)
			recordKeycloakAPI(opDeleteScopePermission, err)
			if err != nil {
				return fmt.Errorf("pruning custodian policy for Keycloak group %q: %w", group.Spec.Path, err)
			}
		}
	}

	group.Status.ManagedCustodians = formatManagedCustodians(desired, desiredOrder)
	return nil
}

// managedCustodian is the parsed form of a status.managedCustodians entry.
type managedCustodian struct {
	path     string
	permID   string
	policyID string
}

// parseManagedCustodians decodes status.managedCustodians ("path|permID|policyID"
// entries) into a map keyed by custodian path.
func parseManagedCustodians(entries []string) map[string]managedCustodian {
	out := map[string]managedCustodian{}
	for _, e := range entries {
		parts := strings.Split(e, managedCustodianSep)
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		out[parts[0]] = managedCustodian{path: parts[0], permID: parts[1], policyID: parts[2]}
	}
	return out
}

// formatManagedCustodians encodes the desired custodians back to the
// "path|permID|policyID" status slice, in the given path order for stable output.
func formatManagedCustodians(desired map[string]managedCustodian, order []string) []string {
	if len(order) == 0 {
		return nil
	}
	out := make([]string, 0, len(order))
	for _, path := range order {
		mc := desired[path]
		out = append(out, strings.Join([]string{mc.path, mc.permID, mc.policyID}, managedCustodianSep))
	}
	return out
}

// idOrName returns the UUID when known, else the name (Keycloak's scope-permission
// resources/policies fields accept either; on the conflict path the UUID is unknown
// so the name is used).
func idOrName(id, name string) string {
	if id != "" {
		return id
	}
	return name
}

// reconcileDelete runs the finalizer. Per the claim model the Keycloak group is
// deleted only when this CR created it (status.Created); an adopted group is
// released (the finalizer drops without deleting), so removing a CR that merely
// claimed a pre-existing group never destroys it. A Keycloak error during delete
// fails the reconcile and requeues, so the finalizer is not removed until cleanup
// succeeds.
func (r *GroupReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, group *keycloakv1alpha1.KeycloakGroup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(group, groupFinalizer) {
		return ctrl.Result{}, nil
	}

	// Adopted group (or one never created by this CR) → release, do not delete. No
	// credential or instance resolution is needed: the controller does not touch
	// Keycloak, it only relinquishes its claim.
	if !group.Status.Created {
		r.Recorder.Event(group, corev1.EventTypeNormal, ReasonReleased,
			fmt.Sprintf("released Keycloak group %q without deleting (adopted, not created by this resource)", group.Spec.Path))
		return r.removeFinalizer(ctx, group)
	}

	instance, result, err := r.resolveInstance(ctx, group)
	if instance == nil {
		// The instance is unresolvable (missing, not ready, or reference denied).
		// Do not strand the CR by dropping the finalizer when cleanup could not run;
		// resolveInstance has already surfaced the condition and requeues.
		return result, err
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, group, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		r.Recorder.Event(group, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		return ctrl.Result{}, err
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Verify the group currently at the path is still the one this CR created
	// before deleting it. If it is gone, there is nothing to delete; if its UUID no
	// longer matches status.GroupID, the original was replaced by a foreign group at
	// the same path — release the finalizer without deleting, so a recreate by
	// another actor is not destroyed (the same race quay's controller closes with
	// its ownership marker).
	current, getErr := kc.GetGroupByPath(ctx, group.Spec.Path)
	recordKeycloakAPI(opGetGroupByPath, ignoreNotFound(getErr))
	switch {
	case keycloak.IsNotFound(getErr):
		return r.removeFinalizer(ctx, group)
	case getErr != nil:
		r.Recorder.Event(group, corev1.EventTypeWarning, ReasonKeycloakError,
			fmt.Sprintf("verifying Keycloak group %q before delete: %v", group.Spec.Path, getErr))
		return ctrl.Result{}, fmt.Errorf("verifying Keycloak group %q before delete: %w", group.Spec.Path, getErr)
	}
	if group.Status.GroupID != "" && current.ID != group.Status.GroupID {
		r.Recorder.Event(group, corev1.EventTypeNormal, ReasonReleased,
			fmt.Sprintf("released Keycloak group %q without deleting (UUID %q no longer matches the created UUID %q; recreated by another actor)", group.Spec.Path, current.ID, group.Status.GroupID))
		return r.removeFinalizer(ctx, group)
	}

	delErr := kc.DeleteGroupByPathIfExists(ctx, group.Spec.Path)
	recordKeycloakAPI(opDeleteGroup, delErr)
	if delErr != nil {
		r.Recorder.Event(group, corev1.EventTypeWarning, ReasonKeycloakError,
			fmt.Sprintf("deleting Keycloak group %q: %v", group.Spec.Path, delErr))
		return ctrl.Result{}, fmt.Errorf("deleting Keycloak group %q: %w", group.Spec.Path, delErr)
	}

	r.Recorder.Event(group, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("deleted Keycloak group %q", group.Spec.Path))
	return r.removeFinalizer(ctx, group)
}

// resolveInstance resolves the KeycloakInstance referenced by the group's
// instanceRef. When the reference crosses a namespace boundary it enforces a
// security.holos.run ReferenceGrant in the instance's namespace; a denied
// reference sets Ready=False (reason ReferenceNotGranted) and requeues. A missing
// or not-yet-Ready instance sets Ready=False (reason InstanceNotReady) and
// requeues. On success it returns the resolved instance and a zero result; on any
// non-success path it returns a nil instance plus the result/error the caller
// should return verbatim.
func (r *GroupReconciler) resolveInstance(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup) (*keycloakv1alpha1.KeycloakInstance, ctrl.Result, error) {
	ref := group.Spec.InstanceRef
	instanceNamespace := ref.Namespace
	if instanceNamespace == "" {
		instanceNamespace = group.Namespace
	}

	// Cross-namespace reference: require a ReferenceGrant in the instance namespace.
	if instanceNamespace != group.Namespace {
		allowed, err := referencegrant.Allowed(ctx, r.Client,
			referencegrant.FromRef{
				Group:     keycloakv1alpha1.GroupVersion.Group,
				Kind:      "KeycloakGroup",
				Namespace: group.Namespace,
			},
			referencegrant.ToRef{
				Group:     keycloakv1alpha1.GroupVersion.Group,
				Kind:      "KeycloakInstance",
				Namespace: instanceNamespace,
				Name:      ref.Name,
			},
		)
		if err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("checking ReferenceGrant for KeycloakInstance %s/%s: %w", instanceNamespace, ref.Name, err)
		}
		if !allowed {
			// A missing grant is recoverable: creating the grant later must unblock
			// the group. Use notReady (recoverable, requeue + watch-driven recovery),
			// not reject (terminal), so it self-heals when the grant appears.
			message := fmt.Sprintf("cross-namespace reference to KeycloakInstance %s/%s is not authorized by a security.holos.run ReferenceGrant", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, group, ReasonReferenceNotGranted, message)
			return nil, result, rerr
		}
	}

	instance := &keycloakv1alpha1.KeycloakInstance{}
	key := types.NamespacedName{Namespace: instanceNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, instance); err != nil {
		if apierrors.IsNotFound(err) {
			message := fmt.Sprintf("referenced KeycloakInstance %s/%s does not exist", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, group, ReasonInstanceNotReady, message)
			return nil, result, rerr
		}
		return nil, ctrl.Result{}, fmt.Errorf("resolving KeycloakInstance %s/%s: %w", instanceNamespace, ref.Name, err)
	}
	if !instanceReady(instance) {
		message := fmt.Sprintf("referenced KeycloakInstance %s/%s is not Ready", instanceNamespace, ref.Name)
		result, rerr := r.notReady(ctx, group, ReasonInstanceNotReady, message)
		return nil, result, rerr
	}
	return instance, ctrl.Result{}, nil
}

// instanceReady reports whether the KeycloakInstance's Ready condition is True AND
// reflects the instance's current generation. Requiring the condition's
// observedGeneration to match the instance generation prevents a stale Ready=True
// (left over from an older spec whose new URL/realm/credential has not been
// accepted yet) from letting a group reconcile against an unverified instance.
func instanceReady(instance *keycloakv1alpha1.KeycloakInstance) bool {
	for _, c := range instance.Status.Conditions {
		if c.Type == ConditionReady {
			return c.Status == "True" && c.ObservedGeneration == instance.Generation
		}
	}
	return false
}

// isReservedGroupPath reports whether a group path targets a platform-reserved
// Keycloak identity the controller refuses to manage (ADR-20). Any segment of the
// path matching a reserved name or carrying a reserved prefix triggers the guard.
func isReservedGroupPath(path string) (bool, string) {
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		lower := strings.ToLower(seg)
		if reservedGroupNames[lower] {
			return true, fmt.Sprintf("group path segment %q is platform-reserved and cannot be managed by a KeycloakGroup", seg)
		}
		for _, prefix := range reservedGroupPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return true, fmt.Sprintf("group path segment %q uses the platform-reserved prefix %q and cannot be managed by a KeycloakGroup", seg, prefix)
			}
		}
	}
	return false, ""
}

// succeed stamps Ready/Programmed/Accepted true, emits a Normal event, and writes
// status only when something actually changed.
func (r *GroupReconciler) succeed(ctx context.Context, logger logr.Logger, group *keycloakv1alpha1.KeycloakGroup, reason, message string) (ctrl.Result, error) {
	changed := markReady(&group.Status.Conditions, reason, message, group.Generation)
	changed = changed || group.Status.ObservedGeneration != group.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(group, corev1.EventTypeNormal, reason, message)
	logger.Info("reconciled KeycloakGroup", "path", group.Spec.Path, "reason", reason)
	if err := r.updateStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reject records a terminal-rejection condition (Accepted/Programmed/Ready all
// False) for a spec the controller refuses to act on (a reserved name or a denied
// cross-namespace reference) and emits a Warning, writing status only on a change.
// It returns a zero result with no error: the spec must change to recover, so a
// requeue would only spin.
func (r *GroupReconciler) reject(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup, reason, message string) (ctrl.Result, error) {
	changed := markRejected(&group.Status.Conditions, reason, message, group.Generation)
	changed = changed || group.Status.ObservedGeneration != group.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(group, corev1.EventTypeWarning, reason, message)
	if err := r.updateStatus(ctx, group); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// notReady records a recoverable not-ready condition (Programmed/Ready False,
// Accepted untouched) for a declarative dependency that is not yet satisfied — the
// referenced KeycloakInstance is missing or not Ready, or a cross-namespace
// ReferenceGrant is absent — and requeues on the requeueDependency backoff so the
// reconcile retries once the dependency appears (backstopping the watch-driven
// recovery SetupWithManager wires). It writes status + emits a Warning only on a
// change, so a persistently-unsatisfied dependency does not re-emit identical
// events on every retry.
func (r *GroupReconciler) notReady(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup, reason, message string) (ctrl.Result, error) {
	if changed := markNotReady(&group.Status.Conditions, reason, message, group.Generation); changed {
		r.Recorder.Event(group, corev1.EventTypeWarning, reason, message)
		if err := r.updateStatus(ctx, group); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueDependency}, nil
}

// handleCredentialError maps a credential-resolution error to a reconcile result,
// mirroring the instance reconciler: a missing Secret/key sets CredentialsNotFound
// and requeues; a transient API error requeues with backoff.
func (r *GroupReconciler) handleCredentialError(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&group.Status.Conditions, ReasonCredentialsNotFound, err.Error(), group.Generation); changed {
		r.Recorder.Event(group, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, group); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

// fail records a Keycloak error as a False condition + Warning event and returns
// the error so the request requeues with backoff, writing status only on a change.
func (r *GroupReconciler) fail(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup, err error) (ctrl.Result, error) {
	if changed := markNotReady(&group.Status.Conditions, ReasonKeycloakError, err.Error(), group.Generation); changed {
		r.Recorder.Event(group, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		if statusErr := r.updateStatus(ctx, group); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after Keycloak error")
		}
	}
	return ctrl.Result{}, err
}

// removeFinalizer drops the group finalizer and persists the change so the API
// server can delete the CR.
func (r *GroupReconciler) removeFinalizer(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(group, groupFinalizer)
	if err := r.Update(ctx, group); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict. The retry is load-bearing for the ownership marker
// (status.Created/Adopted): a create side effect already happened in Keycloak, so
// the marker MUST persist. On conflict it refetches the latest object and
// re-applies the computed status before retrying.
func (r *GroupReconciler) updateStatus(ctx context.Context, group *keycloakv1alpha1.KeycloakGroup) error {
	group.Status.ObservedGeneration = group.Generation
	desired := group.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, group); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(group), group); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&group.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating KeycloakGroup status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager.
func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keycloakgroup-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewKeycloakGroupClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakGroup{}).
		// Re-enqueue dependent groups when their referenced KeycloakInstance changes
		// (e.g. it transitions to Ready), so a group blocked on InstanceNotReady
		// recovers promptly rather than only on the requeueDependency backoff.
		Watches(
			&keycloakv1alpha1.KeycloakInstance{},
			handler.EnqueueRequestsFromMapFunc(r.groupsForInstance),
		).
		Complete(r)
}

// groupsForInstance maps a changed KeycloakInstance to reconcile requests for
// every KeycloakGroup that references it. A group references the instance either
// in its own namespace (instanceRef.namespace empty) or cross-namespace
// (instanceRef.namespace set to the instance's namespace), so both forms are
// matched by name + effective namespace.
func (r *GroupReconciler) groupsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	instance, ok := obj.(*keycloakv1alpha1.KeycloakInstance)
	if !ok {
		return nil
	}
	var groups keycloakv1alpha1.KeycloakGroupList
	if err := r.List(ctx, &groups); err != nil {
		log.FromContext(ctx).Error(err, "listing KeycloakGroups to map a KeycloakInstance change")
		return nil
	}
	var requests []reconcile.Request
	for i := range groups.Items {
		g := &groups.Items[i]
		refNamespace := g.Spec.InstanceRef.Namespace
		if refNamespace == "" {
			refNamespace = g.Namespace
		}
		if g.Spec.InstanceRef.Name == instance.Name && refNamespace == instance.Namespace {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: g.Namespace, Name: g.Name}})
		}
	}
	return requests
}

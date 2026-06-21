package keycloak

import (
	"context"
	"fmt"
	"sort"
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

// userFinalizer guards Keycloak-side cleanup of a KeycloakUser: while present,
// deleting the CR runs the finalizer (which deletes the Keycloak user only when
// this CR created it, or prunes the memberships off an adopted user) before the
// CR is removed from the API server. Its value is the resource's qualified name.
const userFinalizer = "user.keycloak.holos.run/finalizer"

// UserClient is the seam the KeycloakUser reconciler drives Keycloak through. It
// is the subset of internal/keycloak.Client's user, group, and
// federated-identity operations the reconciler needs, named as an interface so
// tests inject a fake without HTTP. The concrete *keycloak.Client satisfies it.
type UserClient interface {
	// FindUserByEmail returns the user whose email exactly matches, or nil when
	// none exists (an absent user is not an error — the reconciler pre-creates on
	// nil).
	FindUserByEmail(ctx context.Context, email string) (*keycloak.User, error)
	// CreateUser creates the user and returns its UUID. An already-existing user
	// is surfaced as an error for which keycloak.IsConflict reports true.
	CreateUser(ctx context.Context, user keycloak.User) (string, error)
	// DeleteUserIfExists deletes the user by UUID, treating an already-absent user
	// as success (the finalizer's idempotent cleanup).
	DeleteUserIfExists(ctx context.Context, userID string) error

	// GetGroupByPath fetches the group at the given full path; a missing group
	// returns an error for which keycloak.IsNotFound reports true. Used to resolve
	// a declared membership path to the group UUID the membership endpoint needs.
	GetGroupByPath(ctx context.Context, path string) (*keycloak.Group, error)
	// AddUserToGroup adds the user to the group (idempotent on Keycloak's side).
	AddUserToGroup(ctx context.Context, userID, groupID string) error
	// RemoveUserFromGroupIfMember removes the user from the group, treating a
	// not-a-member result as success so the prune is idempotent.
	RemoveUserFromGroupIfMember(ctx context.Context, userID, groupID string) error

	// CreateFederatedIdentityIfNotExists links the user to an upstream IdP account
	// so first federated login auto-links this pre-created record. A matching
	// existing link is treated as success; a link to a different upstream account
	// is surfaced as an error.
	CreateFederatedIdentityIfNotExists(ctx context.Context, userID, provider string, link keycloak.FederatedIdentity) error
	// DeleteFederatedIdentityIfExists removes the user's link to an IdP, treating
	// an already-absent link as success — the release path's cleanup of a link this
	// CR added to an adopted user.
	DeleteFederatedIdentityIfExists(ctx context.Context, userID, provider string) error
	// ListFederatedIdentities returns the user's existing federated-identity links,
	// so a prune can verify the current link's upstream subject matches the one this
	// CR created before deleting it (never deleting a link recreated out of band to a
	// different subject).
	ListFederatedIdentities(ctx context.Context, userID string) ([]keycloak.FederatedIdentity, error)
}

// UserClientFactory builds a UserClient from a resolved Keycloak credential, the
// instance URL/realm, and the CA bundle the instance spec carries. The default
// factory returns a real *keycloak.Client; tests substitute a fake.
type UserClientFactory func(cred *keycloakCredential, url, realm string, caBundle []byte) UserClient

// NewKeycloakUserClient is the production UserClientFactory.
func NewKeycloakUserClient(cred *keycloakCredential, url, realm string, caBundle []byte) UserClient {
	return newKeycloakClient(cred, url, realm, caBundle)
}

// Compile-time assertion that the real Keycloak client satisfies the seam.
var _ UserClient = (*keycloak.Client)(nil)

// UserReconciler reconciles a keycloak.holos.run KeycloakUser against the realm
// of its referenced KeycloakInstance: it pre-provisions the user by email (only
// when absent), reconciles the declared group memberships to the desired set,
// configures the IdP federated-identity link, and on delete runs a finalizer
// that deletes only a user it created (releasing an adopted one). Status follows
// the Gateway-API convention and meaningful transitions emit Events.
type UserReconciler struct {
	// Client is the manager's cached client for the KeycloakUser CR and status,
	// and for resolving the referenced KeycloakInstance.
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
	// NewKeycloakUserClient; tests override it with a fake factory.
	NewClient UserClientFactory
}

// Reconcile drives a KeycloakUser toward its desired state: fetch CR → ensure
// finalizer → on delete run Keycloak cleanup then remove finalizer → else
// resolve instance (+ReferenceGrant) → resolve credential → find/create the user
// (claim model) → reconcile group memberships → ensure the IdP link → mark Ready.
func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)
	defer func() { recordReconcile(kindUser, retErr) }()

	user := &keycloakv1alpha1.KeycloakUser{}
	if err := r.Get(ctx, req.NamespacedName, user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !user.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, user)
	}

	if controllerutil.AddFinalizer(user, userFinalizer) {
		if err := r.Update(ctx, user); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueImmediately}, nil
	}

	return r.reconcileNormal(ctx, logger, user)
}

// reconcileNormal resolves the instance and credential, then finds-or-creates
// the Keycloak user (claim model) and reconciles its memberships and IdP link.
func (r *UserReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, user *keycloakv1alpha1.KeycloakUser) (ctrl.Result, error) {
	instance, result, err := r.resolveInstance(ctx, user)
	if instance == nil || err != nil {
		return result, err
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, user, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		return r.fail(ctx, user, err)
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Claim model (ADR-20, mirroring ADR-19): a user this CR did not create and
	// does not already own is a Conflict unless spec.adopt opts in. The user is
	// keyed by its immutable email. FindUserByEmail returns nil when absent.
	existing, findErr := kc.FindUserByEmail(ctx, user.Spec.Email)
	recordKeycloakAPI(opFindUserByEmail, findErr)
	if findErr != nil {
		return r.fail(ctx, user, fmt.Errorf("looking up Keycloak user by email %q: %w", user.Spec.Email, findErr))
	}
	if existing == nil {
		return r.reconcileCreate(ctx, logger, kc, user)
	}
	return r.reconcileExisting(ctx, logger, kc, user, existing)
}

// reconcileCreate provisions the user when the email lookup reported it absent.
// CreateUser tolerates a 409 (a concurrent actor created the same email in the
// race window after the lookup): on that conflict the user is re-resolved and
// re-evaluated against the claim model rather than silently seized.
func (r *UserReconciler) reconcileCreate(ctx context.Context, logger logr.Logger, kc UserClient, user *keycloakv1alpha1.KeycloakUser) (ctrl.Result, error) {
	userID, err := kc.CreateUser(ctx, r.desiredUser(user))
	recordKeycloakAPI(opCreateUser, ignoreConflict(err))
	if keycloak.IsConflict(err) {
		// Lost the create race: the user now exists but this CR did not create it.
		// Re-resolve and re-evaluate against the claim model.
		existing, findErr := kc.FindUserByEmail(ctx, user.Spec.Email)
		recordKeycloakAPI(opFindUserByEmail, findErr)
		if findErr != nil {
			return r.fail(ctx, user, fmt.Errorf("re-resolving Keycloak user after create conflict for %q: %w", user.Spec.Email, findErr))
		}
		if existing == nil {
			return r.fail(ctx, user, fmt.Errorf("conflict creating Keycloak user %q but no such user is present", user.Spec.Email))
		}
		return r.reconcileExisting(ctx, logger, kc, user, existing)
	}
	if err != nil {
		return r.fail(ctx, user, fmt.Errorf("creating Keycloak user %q: %w", user.Spec.Email, err))
	}
	user.Status.Created = true
	user.Status.Adopted = false
	user.Status.UserID = userID
	return r.reconcileSideEffectsThenSucceed(ctx, logger, kc, user, userID, ReasonCreated,
		fmt.Sprintf("created Keycloak user %q", user.Spec.Email))
}

// reconcileExisting handles a Keycloak user that already exists. When this CR
// already owns it (status.Created/Adopted) it verifies the user's immutable UUID
// still matches status.UserID — a mismatch means the original was deleted and a
// foreign user recreated for the same email, a Conflict rather than a silent
// seizure. Otherwise it adopts (spec.adopt) or records a terminal Conflict.
func (r *UserReconciler) reconcileExisting(ctx context.Context, logger logr.Logger, kc UserClient, user *keycloakv1alpha1.KeycloakUser, existing *keycloak.User) (ctrl.Result, error) {
	if user.Status.Created || user.Status.Adopted {
		if user.Status.UserID != "" && user.Status.UserID != existing.ID {
			return r.userReplaced(ctx, logger, user, existing.ID)
		}
		user.Status.UserID = existing.ID
		reason := ReasonCreated
		if user.Status.Adopted {
			reason = ReasonAdopted
		}
		return r.reconcileSideEffectsThenSucceed(ctx, logger, kc, user, existing.ID, reason,
			fmt.Sprintf("reconciled Keycloak user %q", user.Spec.Email))
	}

	if user.Spec.Adopt {
		user.Status.Created = false
		user.Status.Adopted = true
		user.Status.UserID = existing.ID
		return r.reconcileSideEffectsThenSucceed(ctx, logger, kc, user, existing.ID, ReasonAdopted,
			fmt.Sprintf("adopted existing Keycloak user %q", user.Spec.Email))
	}

	message := fmt.Sprintf("Keycloak user %q already exists and was not created by this resource; set spec.adopt to claim it", user.Spec.Email)
	return r.recordConflict(ctx, logger, user, message)
}

// desiredUser builds the Keycloak user representation to create. The username
// defaults to the email when spec.username is empty. EmailVerified is set true so
// the realm's first-broker-login Trust Email flow can auto-link without a
// verification step (ADR-20); the realm/IdP half of that flow is owned by the
// platform realm config, not this CR.
func (r *UserReconciler) desiredUser(user *keycloakv1alpha1.KeycloakUser) keycloak.User {
	username := user.Spec.Username
	if username == "" {
		username = user.Spec.Email
	}
	return keycloak.User{
		Username:      username,
		Email:         user.Spec.Email,
		Enabled:       true,
		EmailVerified: true,
	}
}

// userReplaced records a Conflict when the user for the spec email no longer
// carries the UUID this CR created (status.UserID) — the original was deleted and
// a foreign one recreated for the same email. The replacement is never
// reconciled into or deleted; an operator must resolve the collision.
func (r *UserReconciler) userReplaced(ctx context.Context, logger logr.Logger, user *keycloakv1alpha1.KeycloakUser, actualID string) (ctrl.Result, error) {
	message := fmt.Sprintf("Keycloak user %q now has UUID %q but this resource created UUID %q; the user was replaced out of band and is not reconciled or deleted", user.Spec.Email, actualID, user.Status.UserID)
	return r.recordConflict(ctx, logger, user, message)
}

// reconcileSideEffectsThenSucceed reconciles the declared group memberships to
// the desired set (joining new ones AND pruning ones dropped from spec.groups),
// ensures the IdP federated-identity link, then marks the resource Ready. It is
// the single funnel every success path runs so side effects are applied only
// after the user itself exists. extraChanged is set when an ownership/managed-set
// status field changed so a steady-state reconcile still persists it.
func (r *UserReconciler) reconcileSideEffectsThenSucceed(ctx context.Context, logger logr.Logger, kc UserClient, user *keycloakv1alpha1.KeycloakUser, userID, reason, message string) (ctrl.Result, error) {
	beforeUserID := user.Status.UserID
	beforeGroups := append([]string(nil), user.Status.ManagedGroups...)
	beforeIDP := user.Status.ManagedIdentityProvider

	if err := r.reconcileMemberships(ctx, kc, user, userID); err != nil {
		return r.fail(ctx, user, err)
	}
	if err := r.reconcileIdentityProviderLink(ctx, kc, user, userID); err != nil {
		return r.fail(ctx, user, err)
	}

	extraChanged := beforeUserID != user.Status.UserID ||
		!equalStrings(beforeGroups, user.Status.ManagedGroups) ||
		beforeIDP != user.Status.ManagedIdentityProvider
	return r.succeed(ctx, logger, user, reason, message, extraChanged)
}

// managedGroupSep separates the fields of a status.managedGroups entry, which
// records "<groupPath>|<groupUUID>" so a membership prune can verify the group
// currently at the path is still the same object this CR joined the user to. A
// group deleted and recreated at the same path gets a fresh UUID, so the prune
// skips it rather than revoking membership from a replacement group this CR never
// joined (the path-vs-UUID guard, mirroring the KeycloakGroup reconciler).
const managedGroupSep = "|"

// parseManagedGroups decodes status.managedGroups ("path|uuid" entries) into a
// map keyed by group path. A legacy bare-path entry (no separator, written before
// this UUID-pinning landed) decodes with an empty UUID, which the prune treats as
// "verify by current lookup" — it is upgraded to a pinned entry on the next join.
func parseManagedGroups(entries []string) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		path, uuid, found := strings.Cut(e, managedGroupSep)
		if path == "" {
			continue
		}
		if !found {
			out[path] = "" // legacy bare-path entry
			continue
		}
		out[path] = uuid
	}
	return out
}

// serializeManagedGroups encodes the managed group set back to the "path|uuid"
// status slice, sorted by path for stable output (nil when empty so the omitempty
// status field round-trips cleanly).
func serializeManagedGroups(managed map[string]string) []string {
	if len(managed) == 0 {
		return nil
	}
	paths := make([]string, 0, len(managed))
	for p := range managed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p+managedGroupSep+managed[p])
	}
	return out
}

// reconcileMemberships reconciles the user's group memberships to the desired set
// declared in spec.groups: it joins every declared group and removes any group
// this CR previously managed (status.managedGroups) that is no longer desired —
// so a membership removed from the spec is actively revoked rather than left in
// place (the add-only gap). status.managedGroups is rewritten to the new desired
// set, updated live as each side effect lands so a mid-loop failure leaves status
// reflecting exactly the memberships currently in Keycloak.
//
// Each managed entry pins the group UUID, so a prune only revokes membership when
// the group currently at the path still has the recorded UUID: a group deleted and
// recreated at the same path out of band is skipped rather than having the
// replacement's membership revoked (the path-vs-UUID guard).
func (r *UserReconciler) reconcileMemberships(ctx context.Context, kc UserClient, user *keycloakv1alpha1.KeycloakUser, userID string) error {
	desired := map[string]bool{}
	for _, p := range user.Spec.Groups {
		desired[p] = true
	}

	managed := parseManagedGroups(user.Status.ManagedGroups)
	defer func() { user.Status.ManagedGroups = serializeManagedGroups(managed) }()

	// Join every declared group (idempotent on Keycloak's side), pinning its UUID.
	for _, path := range user.Spec.Groups {
		group, err := kc.GetGroupByPath(ctx, path)
		recordKeycloakAPI(opGetGroupByPath, err)
		if err != nil {
			return fmt.Errorf("resolving group %q for Keycloak user %q: %w", path, user.Spec.Email, err)
		}
		addErr := kc.AddUserToGroup(ctx, userID, group.ID)
		recordKeycloakAPI(opAddUserToGroup, addErr)
		if addErr != nil {
			return fmt.Errorf("adding Keycloak user %q to group %q: %w", user.Spec.Email, path, addErr)
		}
		managed[path] = group.ID
	}

	// Prune memberships this CR previously managed but that are no longer desired.
	for path, recordedUUID := range managed {
		if desired[path] {
			continue
		}
		group, err := kc.GetGroupByPath(ctx, path)
		recordKeycloakAPI(opGetGroupByPath, ignoreNotFound(err))
		if keycloak.IsNotFound(err) {
			// The group is gone; the membership went with it. Stop tracking it.
			delete(managed, path)
			continue
		}
		if err != nil {
			return fmt.Errorf("resolving group %q to prune membership for Keycloak user %q: %w", path, user.Spec.Email, err)
		}
		// UUID-pinned prune: only revoke when the group at the path is still the one
		// this CR joined. A recreated group (different UUID) is not ours to revoke;
		// stop tracking the stale entry without touching the replacement.
		if recordedUUID != "" && group.ID != recordedUUID {
			delete(managed, path)
			continue
		}
		rmErr := kc.RemoveUserFromGroupIfMember(ctx, userID, group.ID)
		recordKeycloakAPI(opRemoveUserFromGroup, rmErr)
		if rmErr != nil {
			return fmt.Errorf("removing Keycloak user %q from stale group %q: %w", user.Spec.Email, path, rmErr)
		}
		delete(managed, path)
	}
	return nil
}

// reconcileIdentityProviderLink ensures the federated-identity link declared in
// spec.identityProviderLink exists on the user, so a first federated login
// auto-links this pre-created record instead of creating a duplicate (ADR-20).
//
// Two link modes (ADR-20):
//   - Subject-keyed link (identityProviderLink.userId set): the Keycloak
//     federated-identity record requires the upstream subject ({userId}) as its
//     key, so this CR creates the Admin-API link explicitly and records the
//     provider in status.ManagedIdentityProvider so its release can prune it.
//   - Email-only auto-link (userId omitted): there is no upstream subject to key a
//     federated-identity record on yet — the link is established by the realm's
//     first-broker-login flow (Detect Existing Broker User + Trust Email) on the
//     user's first login, matched by the verified email. This reconciler does NOT
//     pre-create an Admin-API link in that mode (it would require a subject and
//     produce a broken/unusable link); it leaves the link to the realm flow and
//     records no managed provider.
//
// Split of responsibility (ADR-20): even in the subject-keyed mode this reconciler
// ensures only the per-user federated-identity record. The first-broker-login flow
// that makes the auto-link actually happen on login is realm configuration owned by
// the platform realm config (keycloak-config-cli), NOT this CR.
func (r *UserReconciler) reconcileIdentityProviderLink(ctx context.Context, kc UserClient, user *keycloakv1alpha1.KeycloakUser, userID string) error {
	link := user.Spec.IdentityProviderLink

	// Determine the provider+subject this CR should now manage an Admin-API link
	// for: only a subject-keyed link (userId set) is managed; a removed link or an
	// email-only auto-link (userId omitted, realm-flow-driven) manages none.
	desiredAlias, desiredSubject := "", ""
	if link != nil && link.UserID != "" {
		desiredAlias, desiredSubject = link.Alias, link.UserID
	}

	// Reconcile-to-desired-set: if this CR previously managed a link to a different
	// provider/subject (or to none now), delete that stale link before recording the
	// new state — otherwise removing/switching identityProviderLink would leave a
	// stale federated identity that still grants IdP login and that finalization no
	// longer knows to prune. The delete is subject-verified so a link recreated out
	// of band to a different upstream subject is never deleted.
	prevAlias, prevSubject := parseManagedIdentityProvider(user.Status.ManagedIdentityProvider)
	if prevAlias != "" && (prevAlias != desiredAlias || prevSubject != desiredSubject) {
		if err := r.deleteFederatedIfSubjectMatches(ctx, kc, user, userID, prevAlias, prevSubject); err != nil {
			return err
		}
		user.Status.ManagedIdentityProvider = ""
	}

	if desiredAlias == "" {
		return nil
	}

	fi := keycloak.FederatedIdentity{
		IdentityProvider: link.Alias,
		UserID:           link.UserID,
		UserName:         link.UserName,
	}
	err := kc.CreateFederatedIdentityIfNotExists(ctx, userID, link.Alias, fi)
	recordKeycloakAPI(opCreateFederatedID, err)
	if err != nil {
		return fmt.Errorf("creating federated-identity link to %q for Keycloak user %q: %w", link.Alias, user.Spec.Email, err)
	}
	user.Status.ManagedIdentityProvider = serializeManagedIdentityProvider(desiredAlias, desiredSubject)
	return nil
}

// managedIDPSep separates the alias and the upstream subject (userId) in the
// status.managedIdentityProvider entry ("<alias>|<userId>"), so a prune can verify
// the current link's subject matches the one this CR created before deleting it.
const managedIDPSep = "|"

// parseManagedIdentityProvider decodes status.managedIdentityProvider into its
// alias and managed upstream subject. A legacy bare-alias entry (no separator,
// written before subject-pinning landed) decodes with an empty subject, which the
// prune treats as "delete the current link for the alias" (the prior behavior).
func parseManagedIdentityProvider(entry string) (alias, subject string) {
	if entry == "" {
		return "", ""
	}
	alias, subject, _ = strings.Cut(entry, managedIDPSep)
	return alias, subject
}

// serializeManagedIdentityProvider encodes the managed alias+subject for status.
func serializeManagedIdentityProvider(alias, subject string) string {
	return alias + managedIDPSep + subject
}

// deleteFederatedIfSubjectMatches deletes the user's federated-identity link for
// alias only when the link currently present points at the recorded upstream
// subject — so a link recreated out of band to a different subject (another
// actor's federation) is never deleted. When the recorded subject is empty (a
// legacy entry) it falls back to the unconditional delete the prior behavior used.
// An already-absent link is success.
func (r *UserReconciler) deleteFederatedIfSubjectMatches(ctx context.Context, kc UserClient, user *keycloakv1alpha1.KeycloakUser, userID, alias, subject string) error {
	if subject != "" {
		links, err := kc.ListFederatedIdentities(ctx, userID)
		recordKeycloakAPI(opListFederatedIDs, err)
		if err != nil {
			return fmt.Errorf("listing federated-identity links to verify %q for Keycloak user %q: %w", alias, user.Spec.Email, err)
		}
		found := false
		for _, l := range links {
			if l.IdentityProvider != alias {
				continue
			}
			found = true
			if l.UserID != subject {
				// The link was recreated to a different upstream subject out of band;
				// it is not the one this CR created. Do not delete another actor's link.
				return nil
			}
		}
		if !found {
			// No link for this alias remains; nothing to delete.
			return nil
		}
	}
	rmErr := kc.DeleteFederatedIdentityIfExists(ctx, userID, alias)
	recordKeycloakAPI(opDeleteFederatedID, rmErr)
	if rmErr != nil {
		return fmt.Errorf("removing federated-identity link %q for Keycloak user %q: %w", alias, user.Spec.Email, rmErr)
	}
	return nil
}

// recordConflict sets a terminal Conflict condition and emits a Warning, writing
// status only on a change so an already-recorded conflict does not spin a loop.
func (r *UserReconciler) recordConflict(ctx context.Context, logger logr.Logger, user *keycloakv1alpha1.KeycloakUser, message string) (ctrl.Result, error) {
	changed := setConflict(&user.Status.Conditions, message, user.Generation)
	changed = changed || user.Status.ObservedGeneration != user.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(user, corev1.EventTypeWarning, ReasonConflict, message)
	logger.Info("KeycloakUser conflict", "email", user.Spec.Email)
	if err := r.updateStatus(ctx, user); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileDelete runs the finalizer. Per the claim model the Keycloak user is
// deleted only when this CR created it (status.Created); an adopted user is
// released (the finalizer drops without deleting), so removing a CR that merely
// claimed a pre-existing user never destroys it. In the adopted case the
// memberships this CR added are pruned explicitly first (a created user's delete
// cascades them). A Keycloak error during cleanup fails the reconcile and
// requeues, so the finalizer is not removed until cleanup succeeds.
func (r *UserReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, user *keycloakv1alpha1.KeycloakUser) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(user, userFinalizer) {
		return ctrl.Result{}, nil
	}

	// Nothing needs Keycloak-side cleanup when the CR never created a user
	// (rejected, blocked on a missing/not-ready instance, a denied ReferenceGrant,
	// a missing credential, or an unresolved Conflict) AND it added no side effects
	// to an adopted user — neither a managed membership nor a managed IdP link.
	// Drop the finalizer immediately rather than resolving a Ready instance +
	// credential that may never resolve. A created user always needs its delete, so
	// it is never short-circuited here.
	noManaged := len(user.Status.ManagedGroups) == 0 && user.Status.ManagedIdentityProvider == ""
	if !user.Status.Created && noManaged {
		return r.removeFinalizer(ctx, user)
	}

	instance, result, err := r.resolveInstance(ctx, user)
	if instance == nil {
		return result, err
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, user, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		r.Recorder.Event(user, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		return ctrl.Result{}, err
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Created user → delete it; the delete cascades its memberships and IdP link.
	if user.Status.Created {
		if user.Status.UserID != "" {
			delErr := kc.DeleteUserIfExists(ctx, user.Status.UserID)
			recordKeycloakAPI(opDeleteUser, delErr)
			if delErr != nil {
				r.Recorder.Event(user, corev1.EventTypeWarning, ReasonKeycloakError,
					fmt.Sprintf("deleting Keycloak user %q: %v", user.Spec.Email, delErr))
				return ctrl.Result{}, fmt.Errorf("deleting Keycloak user %q: %w", user.Spec.Email, delErr)
			}
		}
		r.Recorder.Event(user, corev1.EventTypeNormal, "Deleted",
			fmt.Sprintf("deleted Keycloak user %q", user.Spec.Email))
		return r.removeFinalizer(ctx, user)
	}

	// Adopted user → release: prune only the side effects this CR added (group
	// memberships AND the federated-identity link), never deleting the surviving
	// user the platform did not create.
	if err := r.pruneManagedGroups(ctx, kc, user); err != nil {
		r.Recorder.Event(user, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		return ctrl.Result{}, err
	}
	if err := r.pruneManagedIdentityProvider(ctx, kc, user); err != nil {
		r.Recorder.Event(user, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		return ctrl.Result{}, err
	}
	r.Recorder.Event(user, corev1.EventTypeNormal, ReasonReleased,
		fmt.Sprintf("released adopted Keycloak user %q (pruned controller-added memberships and IdP link, user not deleted)", user.Spec.Email))
	return r.removeFinalizer(ctx, user)
}

// pruneManagedIdentityProvider removes the federated-identity link this CR added
// to an adopted user (status.managedIdentityProvider), then clears the managed
// status. It is used on adopted release, where the surviving user is not deleted
// so the link this CR created must be removed explicitly. The delete is
// subject-verified: a link recreated out of band to a different upstream subject
// is left intact. An already-absent link is treated as success.
func (r *UserReconciler) pruneManagedIdentityProvider(ctx context.Context, kc UserClient, user *keycloakv1alpha1.KeycloakUser) error {
	alias, subject := parseManagedIdentityProvider(user.Status.ManagedIdentityProvider)
	if alias == "" || user.Status.UserID == "" {
		user.Status.ManagedIdentityProvider = ""
		return nil
	}
	if err := r.deleteFederatedIfSubjectMatches(ctx, kc, user, user.Status.UserID, alias, subject); err != nil {
		return err
	}
	user.Status.ManagedIdentityProvider = ""
	return nil
}

// pruneManagedGroups removes the user from every group this CR joined it to
// (status.managedGroups), then clears the managed status. It is used on adopted
// release, where the surviving user is not deleted so its memberships must be
// removed explicitly. It is UUID-pinned like reconcileMemberships: a group
// recreated at the same path (different UUID) is skipped rather than having the
// replacement's membership revoked, and an already-gone group is treated as
// success.
func (r *UserReconciler) pruneManagedGroups(ctx context.Context, kc UserClient, user *keycloakv1alpha1.KeycloakUser) error {
	if user.Status.UserID == "" {
		user.Status.ManagedGroups = nil
		return nil
	}
	for path, recordedUUID := range parseManagedGroups(user.Status.ManagedGroups) {
		group, err := kc.GetGroupByPath(ctx, path)
		recordKeycloakAPI(opGetGroupByPath, ignoreNotFound(err))
		if keycloak.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("resolving group %q to revoke membership on release: %w", path, err)
		}
		if recordedUUID != "" && group.ID != recordedUUID {
			// A different group occupies the path now; not ours to revoke.
			continue
		}
		rmErr := kc.RemoveUserFromGroupIfMember(ctx, user.Status.UserID, group.ID)
		recordKeycloakAPI(opRemoveUserFromGroup, rmErr)
		if rmErr != nil {
			return fmt.Errorf("revoking membership in group %q for Keycloak user %q on release: %w", path, user.Spec.Email, rmErr)
		}
	}
	user.Status.ManagedGroups = nil
	return nil
}

// resolveInstance resolves the KeycloakInstance referenced by the user's
// instanceRef, enforcing a security.holos.run ReferenceGrant when the reference
// crosses a namespace boundary. A denied reference sets Ready=False (reason
// ReferenceNotGranted) and requeues; a missing or not-yet-Ready instance sets
// Ready=False (reason InstanceNotReady) and requeues. On success it returns the
// resolved instance and a zero result; on any non-success path it returns a nil
// instance plus the result/error the caller should return verbatim.
func (r *UserReconciler) resolveInstance(ctx context.Context, user *keycloakv1alpha1.KeycloakUser) (*keycloakv1alpha1.KeycloakInstance, ctrl.Result, error) {
	ref := user.Spec.InstanceRef
	instanceNamespace := ref.Namespace
	if instanceNamespace == "" {
		instanceNamespace = user.Namespace
	}

	if instanceNamespace != user.Namespace {
		allowed, err := referencegrant.Allowed(ctx, r.Client,
			referencegrant.FromRef{
				Group:     keycloakv1alpha1.GroupVersion.Group,
				Kind:      "KeycloakUser",
				Namespace: user.Namespace,
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
			message := fmt.Sprintf("cross-namespace reference to KeycloakInstance %s/%s is not authorized by a security.holos.run ReferenceGrant", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, user, ReasonReferenceNotGranted, message)
			return nil, result, rerr
		}
	}

	instance := &keycloakv1alpha1.KeycloakInstance{}
	key := types.NamespacedName{Namespace: instanceNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, instance); err != nil {
		if apierrors.IsNotFound(err) {
			message := fmt.Sprintf("referenced KeycloakInstance %s/%s does not exist", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, user, ReasonInstanceNotReady, message)
			return nil, result, rerr
		}
		return nil, ctrl.Result{}, fmt.Errorf("resolving KeycloakInstance %s/%s: %w", instanceNamespace, ref.Name, err)
	}
	if !instanceReady(instance) {
		message := fmt.Sprintf("referenced KeycloakInstance %s/%s is not Ready", instanceNamespace, ref.Name)
		result, rerr := r.notReady(ctx, user, ReasonInstanceNotReady, message)
		return nil, result, rerr
	}
	return instance, ctrl.Result{}, nil
}

// succeed stamps Ready/Programmed/Accepted true, emits a Normal event, and writes
// status only when something changed — a condition flipped, observedGeneration
// advanced, or extraChanged is set (load-bearing for the ownership/managed-set
// status fields so a backfill or prune on an already-current object persists).
func (r *UserReconciler) succeed(ctx context.Context, logger logr.Logger, user *keycloakv1alpha1.KeycloakUser, reason, message string, extraChanged bool) (ctrl.Result, error) {
	changed := markReady(&user.Status.Conditions, reason, message, user.Generation)
	changed = changed || extraChanged || user.Status.ObservedGeneration != user.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(user, corev1.EventTypeNormal, reason, message)
	logger.Info("reconciled KeycloakUser", "email", user.Spec.Email, "reason", reason)
	if err := r.updateStatus(ctx, user); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// notReady records a recoverable not-ready condition for an unsatisfied
// declarative dependency and requeues on the requeueDependency backoff, writing
// status + emitting a Warning only on a change.
func (r *UserReconciler) notReady(ctx context.Context, user *keycloakv1alpha1.KeycloakUser, reason, message string) (ctrl.Result, error) {
	if changed := markNotReady(&user.Status.Conditions, reason, message, user.Generation); changed {
		r.Recorder.Event(user, corev1.EventTypeWarning, reason, message)
		if err := r.updateStatus(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueDependency}, nil
}

// handleCredentialError maps a credential-resolution error to a reconcile result:
// a missing Secret/key sets CredentialsNotFound and requeues; a transient API
// error requeues with backoff.
func (r *UserReconciler) handleCredentialError(ctx context.Context, user *keycloakv1alpha1.KeycloakUser, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&user.Status.Conditions, ReasonCredentialsNotFound, err.Error(), user.Generation); changed {
		r.Recorder.Event(user, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, user); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

// fail records a Keycloak error as a False condition + Warning event and returns
// the error so the request requeues with backoff, writing status only on a
// change.
func (r *UserReconciler) fail(ctx context.Context, user *keycloakv1alpha1.KeycloakUser, err error) (ctrl.Result, error) {
	if changed := markNotReady(&user.Status.Conditions, ReasonKeycloakError, err.Error(), user.Generation); changed {
		r.Recorder.Event(user, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		if statusErr := r.updateStatus(ctx, user); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after Keycloak error")
		}
	}
	return ctrl.Result{}, err
}

// removeFinalizer drops the user finalizer and persists the change so the API
// server can delete the CR.
func (r *UserReconciler) removeFinalizer(ctx context.Context, user *keycloakv1alpha1.KeycloakUser) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(user, userFinalizer)
	if err := r.Update(ctx, user); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict. The retry is load-bearing for the ownership marker
// (status.Created/Adopted/UserID): a create side effect already happened in
// Keycloak, so the marker MUST persist. On conflict it refetches and re-applies
// the computed status before retrying.
func (r *UserReconciler) updateStatus(ctx context.Context, user *keycloakv1alpha1.KeycloakUser) error {
	user.Status.ObservedGeneration = user.Generation
	desired := user.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, user); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(user), user); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&user.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating KeycloakUser status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager.
func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keycloakuser-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewKeycloakUserClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakUser{}).
		// Re-enqueue dependent users when their referenced KeycloakInstance changes
		// (e.g. it transitions to Ready), so a user blocked on InstanceNotReady
		// recovers promptly rather than only on the requeueDependency backoff.
		Watches(
			&keycloakv1alpha1.KeycloakInstance{},
			handler.EnqueueRequestsFromMapFunc(r.usersForInstance),
		).
		Complete(r)
}

// usersForInstance maps a changed KeycloakInstance to reconcile requests for
// every KeycloakUser that references it (in its own namespace or cross-namespace).
func (r *UserReconciler) usersForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	instance, ok := obj.(*keycloakv1alpha1.KeycloakInstance)
	if !ok {
		return nil
	}
	var users keycloakv1alpha1.KeycloakUserList
	if err := r.List(ctx, &users); err != nil {
		log.FromContext(ctx).Error(err, "listing KeycloakUsers to map a KeycloakInstance change")
		return nil
	}
	var requests []reconcile.Request
	for i := range users.Items {
		u := &users.Items[i]
		refNamespace := u.Spec.InstanceRef.Namespace
		if refNamespace == "" {
			refNamespace = u.Namespace
		}
		if u.Spec.InstanceRef.Name == instance.Name && refNamespace == instance.Namespace {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: u.Namespace, Name: u.Name}})
		}
	}
	return requests
}

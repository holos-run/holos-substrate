package keycloak

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// clientFinalizer guards Keycloak-side cleanup of a KeycloakClient: while
// present, deleting the CR runs the finalizer (which deletes the Keycloak client
// only when this CR created it) before the CR is removed. Its value is the
// resource's qualified name.
const clientFinalizer = "client.keycloak.holos.run/finalizer"

// clientRoleMapperName is the name of the oidc-usermodel-client-role-mapper this
// reconciler ensures on each project client — the per-client analog of the
// platform's quay-client-roles mapper, retargeted to the project client so its
// own client-role names land in the shared groups claim (ADR-20).
const clientRoleMapperName = "client-roles"

// groupsClaimName is the shared OIDC claim the role mapper emits client-role
// names into — the same "groups" claim the platform realm config keys RBAC on
// (GROUPS_CLAIM in holos/components/keycloak/realm-config/buildplan.cue), so a
// project client's owner/editor/viewer role names surface as
// my-project-{owner,editor,viewer} in the groups claim on login (ADR-20).
const groupsClaimName = "groups"

// ClientClient is the seam the KeycloakClient reconciler drives Keycloak through.
// It is the subset of internal/keycloak.Client's client, client-role,
// protocol-mapper, and client-secret operations the reconciler needs, named as an
// interface so tests inject a fake without HTTP. The concrete *keycloak.Client
// satisfies it.
type ClientClient interface {
	// FindClientByClientID returns the OIDC client whose clientId matches, or nil
	// when none exists (an absent client is not an error — the reconciler creates).
	FindClientByClientID(ctx context.Context, clientID string) (*keycloak.OIDCClient, error)
	// CreateClient creates the OIDC client and returns its UUID. An already-existing
	// client is surfaced as an error for which keycloak.IsConflict reports true.
	CreateClient(ctx context.Context, client keycloak.OIDCClient) (string, error)
	// UpdateClientFields applies only the set (non-nil) managed fields to the
	// client losslessly (fetch-merge-PUT), never clobbering unmanaged keys.
	UpdateClientFields(ctx context.Context, clientUUID string, fields keycloak.ClientFields) error
	// DeleteClient deletes the client by its immutable UUID. The finalizer deletes
	// by the verified UUID (not by clientId) so a foreign client recreated at the
	// same clientId between verification and deletion is never deleted.
	DeleteClient(ctx context.Context, clientUUID string) error

	// CreateClientRoleIfNotExists creates a client role on the client, treating an
	// already-existing role as success (idempotent).
	CreateClientRoleIfNotExists(ctx context.Context, clientUUID string, role keycloak.ClientRole) error
	// EnsureClientRoleMapper idempotently ensures the oidc-usermodel-client-role-mapper
	// named name emits clientID's client roles into the claimName claim, correcting
	// a drifted same-named mapper.
	EnsureClientRoleMapper(ctx context.Context, clientUUID, name, clientID, claimName string) error

	// GetClientSecret returns the confidential client's generated secret value, for
	// delivery to the consumer's Secret.
	GetClientSecret(ctx context.Context, clientUUID string) (*keycloak.ClientSecret, error)
}

// ClientClientFactory builds a ClientClient from a resolved Keycloak credential,
// the instance URL/realm, and the CA bundle the instance spec carries. The
// default factory returns a real *keycloak.Client; tests substitute a fake.
type ClientClientFactory func(cred *keycloakCredential, url, realm string, caBundle []byte) ClientClient

// NewKeycloakClientClient is the production ClientClientFactory.
func NewKeycloakClientClient(cred *keycloakCredential, url, realm string, caBundle []byte) ClientClient {
	return newKeycloakClient(cred, url, realm, caBundle)
}

// Compile-time assertion that the real Keycloak client satisfies the seam.
var _ ClientClient = (*keycloak.Client)(nil)

// ClientReconciler reconciles a keycloak.holos.run KeycloakClient against the
// realm of its referenced KeycloakInstance: it ensures the URL-named OIDC client
// exists with the declared redirect URIs/web origins/type, ensures the declared
// client roles exist, ensures the client-role→groups-claim mapper exists, and —
// for a confidential client — delivers the generated secret to the declared
// secretRef (generate-once). On delete it runs a finalizer that deletes only a
// client it created. Status follows the Gateway-API convention.
type ClientReconciler struct {
	// Client is the manager's cached client for the KeycloakClient CR and status,
	// for resolving the referenced KeycloakInstance, and for delivering the
	// confidential client secret into a Secret in the resource's namespace.
	client.Client
	// APIReader is the manager's non-caching reader, used to Get the credential
	// Secret and the (existing) delivered client-secret Secret without a
	// cluster-wide Secret cache.
	APIReader client.Reader
	// Recorder emits Kubernetes Events for created/updated/failed/deleted
	// transitions.
	Recorder record.EventRecorder
	// Namespace is the controller's own namespace, where credential Secrets are
	// resolved. Defaults to DefaultControllerNamespace via controllerNamespace().
	Namespace string
	// NewClient builds the Keycloak client from a resolved credential. Defaults to
	// NewKeycloakClientClient; tests override it with a fake factory.
	NewClient ClientClientFactory
}

// Reconcile drives a KeycloakClient toward its desired state: fetch CR → ensure
// finalizer → on delete run Keycloak cleanup then remove finalizer → resolve
// instance (+ReferenceGrant) → resolve credential → ensure/update the client →
// ensure client roles + mapper → deliver the confidential secret → mark Ready.
// The reconciler is transparent: it manages exactly the clientId declared in the
// spec, reserving and refusing no client IDs on org-policy grounds (HOL-1421).
func (r *ClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)
	defer func() { recordReconcile(kindClient, retErr) }()

	kclient := &keycloakv1alpha1.KeycloakClient{}
	if err := r.Get(ctx, req.NamespacedName, kclient); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !kclient.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, kclient)
	}

	if controllerutil.AddFinalizer(kclient, clientFinalizer) {
		if err := r.Update(ctx, kclient); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueImmediately}, nil
	}

	return r.reconcileNormal(ctx, logger, kclient)
}

// reconcileNormal resolves the instance and credential, then applies the claim
// model (create / adopt / conflict) before converging the client's roles, mapper,
// and confidential secret delivery. It reconciles exactly the declared clientId,
// reserving and refusing no client IDs (HOL-1421).
func (r *ClientReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, kclient *keycloakv1alpha1.KeycloakClient) (ctrl.Result, error) {
	instance, result, err := r.resolveInstance(ctx, kclient)
	if instance == nil || err != nil {
		return result, err
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, kclient, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		return r.fail(ctx, kclient, err)
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Claim model (ADR-20, mirroring ADR-19): because a Keycloak client lives in
	// the realm's single global client namespace while this CR is namespaced, a
	// client this CR did not create and does not already own must NOT be silently
	// converged (which would seize and reconfigure another app's OIDC client).
	// GET by clientId and branch on the ownership status + spec.adopt.
	existing, findErr := kc.FindClientByClientID(ctx, kclient.Spec.ClientID)
	recordKeycloakAPI(opFindClientByClientID, findErr)
	if findErr != nil {
		return r.fail(ctx, kclient, fmt.Errorf("finding Keycloak client %q: %w", kclient.Spec.ClientID, findErr))
	}
	if existing == nil {
		return r.reconcileCreate(ctx, logger, kc, kclient)
	}
	return r.reconcileExisting(ctx, logger, kc, kclient, existing)
}

// reconcileCreate provisions the client when the clientId lookup reported it
// absent. CreateClient tolerates a 409 (a concurrent actor created the same
// clientId in the race window after the lookup): on that conflict the client is
// re-resolved and re-evaluated against the claim model rather than seized.
func (r *ClientReconciler) reconcileCreate(ctx context.Context, logger logr.Logger, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient) (ctrl.Result, error) {
	uuid, err := kc.CreateClient(ctx, r.desiredClient(kclient))
	recordKeycloakAPI(opCreateClient, ignoreConflict(err))
	if keycloak.IsConflict(err) {
		existing, findErr := kc.FindClientByClientID(ctx, kclient.Spec.ClientID)
		recordKeycloakAPI(opFindClientByClientID, findErr)
		if findErr != nil {
			return r.fail(ctx, kclient, fmt.Errorf("re-resolving Keycloak client after create conflict for %q: %w", kclient.Spec.ClientID, findErr))
		}
		if existing == nil {
			return r.fail(ctx, kclient, fmt.Errorf("conflict creating Keycloak client %q but no such client is present", kclient.Spec.ClientID))
		}
		return r.reconcileExisting(ctx, logger, kc, kclient, existing)
	}
	if err != nil {
		return r.fail(ctx, kclient, fmt.Errorf("creating Keycloak client %q: %w", kclient.Spec.ClientID, err))
	}
	kclient.Status.Created = true
	kclient.Status.Adopted = false
	kclient.Status.ClientUUID = uuid
	return r.convergeThenSucceed(ctx, logger, kc, kclient, uuid, ReasonCreated,
		fmt.Sprintf("created Keycloak client %q", kclient.Spec.ClientID))
}

// reconcileExisting handles a clientId that already exists in the realm. When
// this CR already owns it (status.Created/Adopted) it verifies the immutable UUID
// still matches status.ClientUUID — a mismatch means the original was deleted and
// a foreign client recreated at the same clientId, a Conflict rather than a silent
// seizure. Otherwise it adopts (spec.adopt) and converges, or records a terminal
// Conflict without touching the foreign client.
func (r *ClientReconciler) reconcileExisting(ctx context.Context, logger logr.Logger, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient, existing *keycloak.OIDCClient) (ctrl.Result, error) {
	if kclient.Status.Created || kclient.Status.Adopted {
		if kclient.Status.ClientUUID != "" && kclient.Status.ClientUUID != existing.ID {
			message := fmt.Sprintf("Keycloak client %q now has UUID %q but this resource provisioned UUID %q; the client was replaced out of band and is not reconciled or deleted", kclient.Spec.ClientID, existing.ID, kclient.Status.ClientUUID)
			return r.recordConflict(ctx, logger, kclient, message)
		}
		kclient.Status.ClientUUID = existing.ID
		reason := ReasonCreated
		if kclient.Status.Adopted {
			reason = ReasonAdopted
		}
		return r.convergeThenSucceed(ctx, logger, kc, kclient, existing.ID, reason,
			fmt.Sprintf("reconciled Keycloak client %q", kclient.Spec.ClientID))
	}

	if kclient.Spec.Adopt {
		kclient.Status.Created = false
		kclient.Status.Adopted = true
		kclient.Status.ClientUUID = existing.ID
		return r.convergeThenSucceed(ctx, logger, kc, kclient, existing.ID, ReasonAdopted,
			fmt.Sprintf("adopted existing Keycloak client %q", kclient.Spec.ClientID))
	}

	message := fmt.Sprintf("Keycloak client %q already exists and was not created by this resource; set spec.adopt to claim it", kclient.Spec.ClientID)
	return r.recordConflict(ctx, logger, kclient, message)
}

// convergeThenSucceed converges the managed client fields, roles, mapper, and
// confidential secret delivery (in that order), then marks Ready. It is the
// single funnel every claim-model success path runs.
func (r *ClientReconciler) convergeThenSucceed(ctx context.Context, logger logr.Logger, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient, clientUUID, reason, message string) (ctrl.Result, error) {
	if err := r.updateClient(ctx, kc, kclient, clientUUID); err != nil {
		return r.fail(ctx, kclient, err)
	}
	if err := r.ensureClientRoles(ctx, kc, kclient, clientUUID); err != nil {
		return r.fail(ctx, kclient, err)
	}
	if err := r.ensureRoleMapper(ctx, kc, kclient, clientUUID); err != nil {
		return r.fail(ctx, kclient, err)
	}
	if err := r.deliverClientSecret(ctx, kc, kclient, clientUUID); err != nil {
		return r.fail(ctx, kclient, err)
	}
	return r.succeed(ctx, logger, kclient, reason, message)
}

// recordConflict sets a terminal Conflict condition and emits a Warning, writing
// status only on a change so an already-recorded conflict does not spin a loop.
func (r *ClientReconciler) recordConflict(ctx context.Context, logger logr.Logger, kclient *keycloakv1alpha1.KeycloakClient, message string) (ctrl.Result, error) {
	changed := setConflict(&kclient.Status.Conditions, message, kclient.Generation)
	changed = changed || kclient.Status.ObservedGeneration != kclient.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonConflict, message)
	logger.Info("KeycloakClient conflict", "clientId", kclient.Spec.ClientID)
	if err := r.updateStatus(ctx, kclient); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// desiredClient builds the OIDC client representation to create from the spec. A
// public client is created with the PKCE S256 attribute so its authorization-code
// flow requires PKCE by default (the platform public-client guardrail,
// keycloak-clients.md); a confidential client carries no PKCE attribute.
func (r *ClientReconciler) desiredClient(kclient *keycloakv1alpha1.KeycloakClient) keycloak.OIDCClient {
	public := kclient.Spec.Type == keycloakv1alpha1.KeycloakClientTypePublic
	c := keycloak.OIDCClient{
		ClientID:     kclient.Spec.ClientID,
		Name:         kclient.Name,
		Enabled:      true,
		PublicClient: public,
		RedirectURIs: kclient.Spec.RedirectURIs,
		WebOrigins:   kclient.Spec.WebOrigins,
	}
	if public {
		c.Attributes = map[string]string{keycloak.PKCECodeChallengeMethodAttr: keycloak.PKCEMethodS256}
	}
	return c
}

// updateClient converges the managed fields (type, redirect URIs, web origins,
// enabled, and the PKCE code-challenge attribute) of an existing client losslessly
// via UpdateClientFields, which preserves every unmanaged ClientRepresentation key
// (protocol, service-account flags, default scopes, and any non-PKCE attributes).
// A public client gets the S256 PKCE attribute MERGED in; a confidential client
// gets that attribute actively REMOVED so an adopted/pre-existing confidential
// client carrying a stale S256 converges to no-PKCE (rather than keeping a stale
// PKCE requirement the merge-only path would never clear).
func (r *ClientReconciler) updateClient(ctx context.Context, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient, clientUUID string) error {
	enabled := true
	public := kclient.Spec.Type == keycloakv1alpha1.KeycloakClientTypePublic
	redirects := append([]string(nil), kclient.Spec.RedirectURIs...)
	origins := append([]string(nil), kclient.Spec.WebOrigins...)
	fields := keycloak.ClientFields{
		Enabled:      &enabled,
		PublicClient: &public,
		RedirectURIs: &redirects,
		WebOrigins:   &origins,
	}
	if public {
		fields.Attributes = map[string]string{keycloak.PKCECodeChallengeMethodAttr: keycloak.PKCEMethodS256}
	} else {
		// Confidential: actively clear any stale PKCE code-challenge attribute so an
		// adopted client that previously required PKCE converges to no-PKCE.
		fields.RemoveAttributes = []string{keycloak.PKCECodeChallengeMethodAttr}
	}
	err := kc.UpdateClientFields(ctx, clientUUID, fields)
	recordKeycloakAPI(opUpdateClient, err)
	if err != nil {
		return fmt.Errorf("updating Keycloak client %q: %w", kclient.Spec.ClientID, err)
	}
	return nil
}

// ensureClientRoles ensures every client role declared in spec.clientRoles exists
// on the client (idempotent create-if-absent). The role name IS the groups-claim
// value the role mapper emits (ADR-20), so the role set is the claim vocabulary
// the project client exposes.
func (r *ClientReconciler) ensureClientRoles(ctx context.Context, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient, clientUUID string) error {
	for _, ref := range kclient.Spec.ClientRoles {
		err := kc.CreateClientRoleIfNotExists(ctx, clientUUID, keycloak.ClientRole{Name: ref.Role})
		recordKeycloakAPI(opCreateClientRole, err)
		if err != nil {
			return fmt.Errorf("ensuring client role %q on Keycloak client %q: %w", ref.Role, kclient.Spec.ClientID, err)
		}
	}
	return nil
}

// ensureRoleMapper ensures the oidc-usermodel-client-role-mapper exists on the
// client, configured to emit this client's own client-role names into the shared
// groups claim — the mechanism that surfaces a role group's conferred client role
// (the my-project-{owner,editor,viewer} claim value) on login (ADR-20). It is the
// per-client analog of the platform's quay-client-roles mapper.
func (r *ClientReconciler) ensureRoleMapper(ctx context.Context, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient, clientUUID string) error {
	err := kc.EnsureClientRoleMapper(ctx, clientUUID, clientRoleMapperName, kclient.Spec.ClientID, groupsClaimName)
	recordKeycloakAPI(opEnsureClientRoleMapper, err)
	if err != nil {
		return fmt.Errorf("ensuring client-role mapper on Keycloak client %q: %w", kclient.Spec.ClientID, err)
	}
	return nil
}

// deliverClientSecret delivers a confidential client's generated secret to the
// Secret named by spec.secretRef in the resource's own namespace, generate-once:
// it creates the Secret only when absent and never overwrites an existing one, so
// the value stays stable across reconciles (the Runtime Secret Handling
// guardrail, mirroring the quay-oidc bootstrap). A public client carries no
// secret, so this is a no-op for it.
func (r *ClientReconciler) deliverClientSecret(ctx context.Context, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient, clientUUID string) error {
	if kclient.Spec.Type != keycloakv1alpha1.KeycloakClientTypeConfidential {
		return nil
	}
	ref := kclient.Spec.SecretRef
	if ref == nil {
		// The spec CEL validation requires secretRef for a confidential client; this
		// guard is defense-in-depth so a missing ref fails the reconcile rather than
		// panicking.
		return fmt.Errorf("confidential Keycloak client %q has no spec.secretRef to deliver its secret into", kclient.Spec.ClientID)
	}

	// Generate-once: if the delivered Secret already carries a non-empty value at
	// the requested key, leave it untouched (the value must stay stable across
	// reconciles per the Runtime Secret Handling guardrail). But a Secret that
	// already exists WITHOUT the requested key is NOT delivered: creating it again
	// returns AlreadyExists, which must not be mistaken for success — that would
	// mark the client Ready while the consumer never received its secret. Surface
	// that as an error so an operator resolves the collision (a foreign Secret
	// occupying the name) rather than silently reporting a non-delivery as done.
	key := types.NamespacedName{Namespace: kclient.Namespace, Name: ref.Name}
	existing := &corev1.Secret{}
	getErr := r.APIReader.Get(ctx, key, existing)
	switch {
	case getErr == nil:
		// The Secret must be one THIS KeycloakClient owns (controller owner-reference
		// by UID) before it is accepted as delivered — otherwise a name/key collision
		// with a foreign Secret that happens to carry the same key would make the
		// resource report Ready while consumers receive the wrong secret. A foreign
		// Secret (no matching owner reference) is a collision the operator must
		// resolve, whether or not it carries the key.
		if !ownedBy(existing, kclient) {
			return fmt.Errorf("client-secret Secret %s/%s exists but is not owned by this KeycloakClient; refusing to treat a foreign Secret as the delivered client secret", key.Namespace, key.Name)
		}
		if v, ok := existing.Data[ref.Key]; ok && len(v) > 0 {
			return nil
		}
		return fmt.Errorf("client-secret Secret %s/%s is owned by this KeycloakClient but is missing a non-empty %q key", key.Namespace, key.Name, ref.Key)
	case !apierrors.IsNotFound(getErr):
		return fmt.Errorf("checking delivered client-secret Secret %s/%s: %w", key.Namespace, key.Name, getErr)
	}

	secret, err := kc.GetClientSecret(ctx, clientUUID)
	recordKeycloakAPI(opGetClientSecret, err)
	if err != nil {
		return fmt.Errorf("reading generated secret for Keycloak client %q: %w", kclient.Spec.ClientID, err)
	}
	if secret.Value == "" {
		return fmt.Errorf("empty client secret returned by Keycloak for client %q", kclient.Spec.ClientID)
	}

	delivered := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: kclient.Namespace, Name: ref.Name},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{ref.Key: []byte(secret.Value)},
	}
	if err := controllerutil.SetControllerReference(kclient, delivered, r.Scheme()); err != nil {
		return fmt.Errorf("setting owner reference on delivered client-secret Secret: %w", err)
	}
	if err := r.Create(ctx, delivered); err != nil {
		// AlreadyExists here means a Secret of that name appeared between the get
		// above and this create. Since the get found no usable key, this Secret is
		// not one this reconciler delivered — surface the collision rather than
		// reporting a non-delivery as success.
		return fmt.Errorf("delivering client secret to Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// ownedBy reports whether secret carries a controller owner reference pointing at
// this KeycloakClient (matched by UID), i.e. it is a Secret this CR delivered
// rather than a foreign Secret that merely shares the name.
func ownedBy(secret *corev1.Secret, kclient *keycloakv1alpha1.KeycloakClient) bool {
	for _, ref := range secret.OwnerReferences {
		if ref.UID == kclient.UID && ref.Kind == "KeycloakClient" {
			return true
		}
	}
	return false
}

// reconcileDelete runs the finalizer. Per the claim model the Keycloak client is
// deleted only when this CR CREATED it (status.Created); an adopted client is
// released (the finalizer drops without deleting), so removing a CR that merely
// claimed a pre-existing client never destroys it. A never-provisioned CR
// (rejected, blocked, missing credential, or an unresolved Conflict) drops the
// finalizer with no Keycloak cleanup. Before deleting, the client's current UUID
// is verified against status.ClientUUID so a foreign client recreated at the same
// clientId out of band is released, not deleted. The delivered client-secret
// Secret is garbage-collected by its owner reference, so it needs no explicit
// cleanup here.
func (r *ClientReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, kclient *keycloakv1alpha1.KeycloakClient) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(kclient, clientFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only a client this CR created needs a Keycloak-side delete. An adopted or
	// never-created client (rejected, blocked, conflict, or merely claimed) has no
	// client this CR owns to delete — drop the finalizer immediately rather than
	// resolving an instance + credential that may never resolve.
	if !kclient.Status.Created {
		return r.removeFinalizer(ctx, kclient)
	}

	instance, result, err := r.resolveInstance(ctx, kclient)
	if instance == nil {
		return result, err
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, kclient, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		return ctrl.Result{}, err
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Verify the client currently at the clientId is still the one this CR created
	// (UUID match) before deleting. If it is gone there is nothing to do; if its
	// UUID no longer matches, a foreign client was recreated at the same clientId —
	// release it without deletion rather than destroying another actor's client.
	current, getErr := kc.FindClientByClientID(ctx, kclient.Spec.ClientID)
	recordKeycloakAPI(opFindClientByClientID, getErr)
	if getErr != nil {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonKeycloakError,
			fmt.Sprintf("verifying Keycloak client %q before cleanup: %v", kclient.Spec.ClientID, getErr))
		return ctrl.Result{}, fmt.Errorf("verifying Keycloak client %q before cleanup: %w", kclient.Spec.ClientID, getErr)
	}
	if current == nil {
		return r.removeFinalizer(ctx, kclient)
	}
	if kclient.Status.ClientUUID != "" && current.ID != kclient.Status.ClientUUID {
		r.Recorder.Event(kclient, corev1.EventTypeNormal, ReasonReleased,
			fmt.Sprintf("released Keycloak client %q without changes (UUID %q no longer matches the provisioned UUID %q; replaced out of band)", kclient.Spec.ClientID, current.ID, kclient.Status.ClientUUID))
		return r.removeFinalizer(ctx, kclient)
	}

	// Delete by the verified UUID, not the clientId: a fresh by-clientId lookup
	// here would reopen the TOCTOU window where a foreign client recreated at the
	// same clientId between the verification above and the delete could be deleted.
	// An already-absent UUID (a concurrent delete won) is treated as success.
	delErr := kc.DeleteClient(ctx, current.ID)
	recordKeycloakAPI(opDeleteClient, ignoreNotFound(delErr))
	if delErr != nil && !keycloak.IsNotFound(delErr) {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonKeycloakError,
			fmt.Sprintf("deleting Keycloak client %q: %v", kclient.Spec.ClientID, delErr))
		return ctrl.Result{}, fmt.Errorf("deleting Keycloak client %q: %w", kclient.Spec.ClientID, delErr)
	}

	r.Recorder.Event(kclient, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("deleted Keycloak client %q", kclient.Spec.ClientID))
	return r.removeFinalizer(ctx, kclient)
}

// resolveInstance resolves the KeycloakInstance referenced by the client's
// instanceRef, enforcing a security.holos.run ReferenceGrant when the reference
// crosses a namespace boundary. Denied/missing/not-ready paths set Ready=False
// and requeue; on success it returns the resolved instance and a zero result.
func (r *ClientReconciler) resolveInstance(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient) (*keycloakv1alpha1.KeycloakInstance, ctrl.Result, error) {
	ref := kclient.Spec.InstanceRef
	instanceNamespace := ref.Namespace
	if instanceNamespace == "" {
		instanceNamespace = kclient.Namespace
	}

	if instanceNamespace != kclient.Namespace {
		allowed, err := referencegrant.Allowed(ctx, r.Client,
			referencegrant.FromRef{
				Group:     keycloakv1alpha1.GroupVersion.Group,
				Kind:      "KeycloakClient",
				Namespace: kclient.Namespace,
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
			result, rerr := r.notReady(ctx, kclient, ReasonReferenceNotGranted, message)
			return nil, result, rerr
		}
	}

	instance := &keycloakv1alpha1.KeycloakInstance{}
	key := types.NamespacedName{Namespace: instanceNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, instance); err != nil {
		if apierrors.IsNotFound(err) {
			message := fmt.Sprintf("referenced KeycloakInstance %s/%s does not exist", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, kclient, ReasonInstanceNotReady, message)
			return nil, result, rerr
		}
		return nil, ctrl.Result{}, fmt.Errorf("resolving KeycloakInstance %s/%s: %w", instanceNamespace, ref.Name, err)
	}
	if !instanceReady(instance) {
		message := fmt.Sprintf("referenced KeycloakInstance %s/%s is not Ready", instanceNamespace, ref.Name)
		result, rerr := r.notReady(ctx, kclient, ReasonInstanceNotReady, message)
		return nil, result, rerr
	}
	return instance, ctrl.Result{}, nil
}

// succeed stamps Ready/Programmed/Accepted true, emits a Normal event, and writes
// status only when a condition flipped or observedGeneration advanced.
func (r *ClientReconciler) succeed(ctx context.Context, logger logr.Logger, kclient *keycloakv1alpha1.KeycloakClient, reason, message string) (ctrl.Result, error) {
	changed := markReady(&kclient.Status.Conditions, reason, message, kclient.Generation)
	changed = changed || kclient.Status.ObservedGeneration != kclient.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(kclient, corev1.EventTypeNormal, reason, message)
	logger.Info("reconciled KeycloakClient", "clientId", kclient.Spec.ClientID, "reason", reason)
	if err := r.updateStatus(ctx, kclient); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// notReady records a recoverable not-ready condition for an unsatisfied
// declarative dependency and requeues on the requeueDependency backoff, writing
// status + emitting a Warning only on a change.
func (r *ClientReconciler) notReady(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient, reason, message string) (ctrl.Result, error) {
	if changed := markNotReady(&kclient.Status.Conditions, reason, message, kclient.Generation); changed {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, reason, message)
		if err := r.updateStatus(ctx, kclient); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueDependency}, nil
}

// handleCredentialError maps a credential-resolution error to a reconcile result.
func (r *ClientReconciler) handleCredentialError(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&kclient.Status.Conditions, ReasonCredentialsNotFound, err.Error(), kclient.Generation); changed {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, kclient); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

// fail records a Keycloak error as a False condition + Warning event and returns
// the error so the request requeues with backoff, writing status only on a
// change.
func (r *ClientReconciler) fail(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient, err error) (ctrl.Result, error) {
	if changed := markNotReady(&kclient.Status.Conditions, ReasonKeycloakError, err.Error(), kclient.Generation); changed {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		if statusErr := r.updateStatus(ctx, kclient); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after Keycloak error")
		}
	}
	return ctrl.Result{}, err
}

// removeFinalizer drops the client finalizer and persists the change.
func (r *ClientReconciler) removeFinalizer(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(kclient, clientFinalizer)
	if err := r.Update(ctx, kclient); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict by refetching and re-applying the computed status.
func (r *ClientReconciler) updateStatus(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient) error {
	kclient.Status.ObservedGeneration = kclient.Generation
	desired := kclient.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, kclient); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(kclient), kclient); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&kclient.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating KeycloakClient status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager.
func (r *ClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keycloakclient-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewKeycloakClientClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakClient{}).
		// The delivered confidential-client Secret is NOT Owns-watched: the
		// controller holds only secrets get;create (never list/watch), so it must not
		// establish a cluster-wide Secret informer. The Secret is generate-once and
		// reaped by its owner reference on CR deletion, so no Secret watch is needed.
		// Re-enqueue dependent clients when their referenced KeycloakInstance changes.
		Watches(
			&keycloakv1alpha1.KeycloakInstance{},
			handler.EnqueueRequestsFromMapFunc(r.clientsForInstance),
		).
		Complete(r)
}

// clientsForInstance maps a changed KeycloakInstance to reconcile requests for
// every KeycloakClient that references it (in its own namespace or cross-namespace).
func (r *ClientReconciler) clientsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	instance, ok := obj.(*keycloakv1alpha1.KeycloakInstance)
	if !ok {
		return nil
	}
	var clients keycloakv1alpha1.KeycloakClientList
	if err := r.List(ctx, &clients); err != nil {
		log.FromContext(ctx).Error(err, "listing KeycloakClients to map a KeycloakInstance change")
		return nil
	}
	var requests []reconcile.Request
	for i := range clients.Items {
		c := &clients.Items[i]
		refNamespace := c.Spec.InstanceRef.Namespace
		if refNamespace == "" {
			refNamespace = c.Namespace
		}
		if c.Spec.InstanceRef.Name == instance.Name && refNamespace == instance.Namespace {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name}})
		}
	}
	return requests
}

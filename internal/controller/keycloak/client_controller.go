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

// reservedClientIDs are the platform-reserved Keycloak clientIds a tenant
// KeycloakClient refuses to manage (ADR-20). They are the platform OIDC clients
// owned by the keycloak-config-cli realm config (argocd, kargo) and the
// in-cluster Quay client (https://quay.holos.localhost); a namespaced tenant CR
// must not claim or clobber them.
var reservedClientIDs = map[string]bool{
	"argocd":                       true,
	"kargo":                        true,
	"https://quay.holos.localhost": true,
}

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
	// DeleteClientByClientIDIfExists deletes the client by clientId, treating an
	// already-absent client as success (the finalizer's idempotent cleanup).
	DeleteClientByClientIDIfExists(ctx context.Context, clientID string) error

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
// finalizer → on delete run Keycloak cleanup then remove finalizer → else
// reserved-name guard → resolve instance (+ReferenceGrant) → resolve credential
// → ensure/update the client → ensure client roles + mapper → deliver the
// confidential secret → mark Ready.
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

// reconcileNormal performs the reserved-name guard, resolves the instance and
// credential, then creates-or-updates the OIDC client and reconciles its roles,
// mapper, and confidential secret delivery.
func (r *ClientReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, kclient *keycloakv1alpha1.KeycloakClient) (ctrl.Result, error) {
	// Reserved-name guard (ADR-20): never manage a platform-reserved client.
	if reservedClientIDs[kclient.Spec.ClientID] {
		return r.reject(ctx, kclient, ReasonReserved,
			fmt.Sprintf("clientId %q is platform-reserved and cannot be managed by a KeycloakClient", kclient.Spec.ClientID))
	}

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

	// Ensure the OIDC client exists (create when absent, converge managed fields
	// when present), returning its UUID and whether this call created it.
	clientUUID, created, err := r.ensureClient(ctx, kc, kclient)
	if err != nil {
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

	reason, message := ReasonReconciled, fmt.Sprintf("reconciled Keycloak client %q", kclient.Spec.ClientID)
	if created {
		reason = ReasonCreated
		message = fmt.Sprintf("created Keycloak client %q", kclient.Spec.ClientID)
	}
	return r.succeed(ctx, logger, kclient, reason, message)
}

// ensureClient creates the OIDC client when absent or converges its managed
// fields when present, returning the client UUID and whether this call created
// it. The create tolerates a 409 (a concurrent actor created the same clientId)
// by re-resolving and converging. Unlike the user/group claim model, the platform
// owns its project clients by URL-named identity and a reserved-name guard, so an
// existing client is converged in place rather than gated on spec.adopt.
func (r *ClientReconciler) ensureClient(ctx context.Context, kc ClientClient, kclient *keycloakv1alpha1.KeycloakClient) (string, bool, error) {
	existing, err := kc.FindClientByClientID(ctx, kclient.Spec.ClientID)
	recordKeycloakAPI(opFindClientByClientID, err)
	if err != nil {
		return "", false, fmt.Errorf("finding Keycloak client %q: %w", kclient.Spec.ClientID, err)
	}
	if existing == nil {
		uuid, createErr := kc.CreateClient(ctx, r.desiredClient(kclient))
		recordKeycloakAPI(opCreateClient, ignoreConflict(createErr))
		if keycloak.IsConflict(createErr) {
			// Lost the create race: re-resolve and converge the existing client.
			existing, err = kc.FindClientByClientID(ctx, kclient.Spec.ClientID)
			recordKeycloakAPI(opFindClientByClientID, err)
			if err != nil {
				return "", false, fmt.Errorf("re-resolving Keycloak client after create conflict for %q: %w", kclient.Spec.ClientID, err)
			}
			if existing == nil {
				return "", false, fmt.Errorf("conflict creating Keycloak client %q but no such client is present", kclient.Spec.ClientID)
			}
			if err := r.updateClient(ctx, kc, kclient, existing.ID); err != nil {
				return "", false, err
			}
			return existing.ID, false, nil
		}
		if createErr != nil {
			return "", false, fmt.Errorf("creating Keycloak client %q: %w", kclient.Spec.ClientID, createErr)
		}
		return uuid, true, nil
	}

	// Existing client: converge the managed fields in place.
	if err := r.updateClient(ctx, kc, kclient, existing.ID); err != nil {
		return "", false, err
	}
	return existing.ID, false, nil
}

// desiredClient builds the OIDC client representation to create from the spec.
func (r *ClientReconciler) desiredClient(kclient *keycloakv1alpha1.KeycloakClient) keycloak.OIDCClient {
	return keycloak.OIDCClient{
		ClientID:     kclient.Spec.ClientID,
		Name:         kclient.Name,
		Enabled:      true,
		PublicClient: kclient.Spec.Type == keycloakv1alpha1.KeycloakClientTypePublic,
		RedirectURIs: kclient.Spec.RedirectURIs,
		WebOrigins:   kclient.Spec.WebOrigins,
	}
}

// updateClient converges the managed fields (type, redirect URIs, web origins,
// enabled) of an existing client losslessly via UpdateClientFields, which
// preserves every unmanaged ClientRepresentation key (protocol, PKCE attributes,
// service-account flags, default scopes).
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

	// Generate-once: if the delivered Secret already carries the key, leave it.
	key := types.NamespacedName{Namespace: kclient.Namespace, Name: ref.Name}
	existing := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, key, existing); err == nil {
		if v, ok := existing.Data[ref.Key]; ok && len(v) > 0 {
			return nil
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking delivered client-secret Secret %s/%s: %w", key.Namespace, key.Name, err)
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
		if apierrors.IsAlreadyExists(err) {
			// Created concurrently between the get and the create; treat as delivered.
			return nil
		}
		return fmt.Errorf("delivering client secret to Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// reconcileDelete runs the finalizer. The Keycloak client is deleted only when
// this CR created it (status reason Created persisted via the Programmed
// condition); a never-provisioned CR (rejected, blocked, missing credential)
// drops the finalizer with no Keycloak cleanup. The delivered client-secret
// Secret is garbage-collected by its owner reference, so it needs no explicit
// cleanup here.
func (r *ClientReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, kclient *keycloakv1alpha1.KeycloakClient) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(kclient, clientFinalizer) {
		return ctrl.Result{}, nil
	}

	// Nothing to clean up when the client was never programmed into Keycloak (no
	// Ready=True/Programmed=True ever reached). Drop the finalizer immediately
	// rather than resolving an instance + credential that may never resolve.
	if !clientProgrammed(kclient) {
		return r.removeFinalizer(ctx, kclient)
	}

	// A reserved clientId is never managed, so it is never deleted on CR removal.
	if reservedClientIDs[kclient.Spec.ClientID] {
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

	delErr := kc.DeleteClientByClientIDIfExists(ctx, kclient.Spec.ClientID)
	recordKeycloakAPI(opDeleteClient, delErr)
	if delErr != nil {
		r.Recorder.Event(kclient, corev1.EventTypeWarning, ReasonKeycloakError,
			fmt.Sprintf("deleting Keycloak client %q: %v", kclient.Spec.ClientID, delErr))
		return ctrl.Result{}, fmt.Errorf("deleting Keycloak client %q: %w", kclient.Spec.ClientID, delErr)
	}

	r.Recorder.Event(kclient, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("deleted Keycloak client %q", kclient.Spec.ClientID))
	return r.removeFinalizer(ctx, kclient)
}

// clientProgrammed reports whether the client has ever been programmed into
// Keycloak (its Programmed condition is True), the signal that there is a
// Keycloak-side client to delete on finalization.
func clientProgrammed(kclient *keycloakv1alpha1.KeycloakClient) bool {
	for _, c := range kclient.Status.Conditions {
		if c.Type == ConditionProgrammed {
			return c.Status == "True"
		}
	}
	return false
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

// reject records a terminal-rejection condition for a spec the controller refuses
// to act on (a reserved clientId or a denied cross-namespace reference) and emits
// a Warning, writing status only on a change. It returns a zero result with no
// error: the spec must change to recover.
func (r *ClientReconciler) reject(ctx context.Context, kclient *keycloakv1alpha1.KeycloakClient, reason, message string) (ctrl.Result, error) {
	changed := markRejected(&kclient.Status.Conditions, reason, message, kclient.Generation)
	changed = changed || kclient.Status.ObservedGeneration != kclient.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(kclient, corev1.EventTypeWarning, reason, message)
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

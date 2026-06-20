package keycloak

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	"github.com/holos-run/holos-paas/internal/keycloak"
)

// InstanceClient is the seam the KeycloakInstance reconciler drives Keycloak
// through. It is the subset of internal/keycloak.Client the reconciler needs —
// just the realm-reachability probe — named as an interface so tests inject a
// fake without HTTP. The concrete *keycloak.Client satisfies it.
type InstanceClient interface {
	// GetRealm fetches the target realm's representation. A successful call proves
	// both that the admin credential authenticated and that the realm is reachable;
	// a missing realm returns an error for which keycloak.IsNotFound reports true.
	GetRealm(ctx context.Context) (*keycloak.Realm, error)
}

// InstanceClientFactory builds an InstanceClient from a resolved Keycloak
// credential, the instance URL/realm, and the CA bundle the instance spec
// carries. The default factory (NewKeycloakInstanceClient) returns a real
// *keycloak.Client trusting that bundle; tests substitute a factory that returns
// a fake, which is how the reconciler is exercised without a live Keycloak or
// HTTP. The caBundle comes from the spec (not the credential Secret), so it is a
// separate argument and is never folded into keycloakCredential.
type InstanceClientFactory func(cred *keycloakCredential, url, realm string, caBundle []byte) InstanceClient

// newKeycloakClient builds a real internal/keycloak client from the credential's
// client_credentials material and the instance's url/realm, trusting the
// PEM-encoded caBundle (the in-cluster Keycloak's local-CA chain) in addition to
// the system store. An empty caBundle yields system trust only — unchanged
// behavior. A caBundle that contains no parseable certificate falls back to a
// system-trust client so a misconfigured bundle surfaces as the original x509
// trust error on the Keycloak call rather than a silent nil client; the parse
// failure is logged. It is shared by both the instance and group factories.
func newKeycloakClient(cred *keycloakCredential, url, realm string, caBundle []byte) *keycloak.Client {
	creds := keycloak.Credentials{
		ClientID:     cred.clientID,
		ClientSecret: cred.clientSecret,
		TokenURL:     cred.tokenURL,
	}
	c, err := keycloak.NewClientWithCABundle(url, realm, creds, caBundle)
	if err != nil {
		log.Log.Error(err, "building Keycloak client with caBundle; falling back to system trust")
		return keycloak.NewClient(url, realm, creds, nil)
	}
	return c
}

// NewKeycloakInstanceClient is the production InstanceClientFactory.
func NewKeycloakInstanceClient(cred *keycloakCredential, url, realm string, caBundle []byte) InstanceClient {
	return newKeycloakClient(cred, url, realm, caBundle)
}

// Compile-time assertion that the real Keycloak client satisfies the reconciler's
// seam, so a signature drift in internal/keycloak is caught at build time.
var _ InstanceClient = (*keycloak.Client)(nil)

// InstanceReconciler reconciles a keycloak.holos.run KeycloakInstance: it
// resolves the admin credential and caBundle, builds the internal/keycloak
// client, verifies the target realm is reachable, and reports Ready. The
// KeycloakInstance models only a connection reference — there is no Keycloak-side
// object it owns — so it needs no finalizer (Keycloak cleanup is the job of the
// resource Kinds that create groups/users/clients). Status follows the
// Gateway-API convention (see conditions.go) and meaningful transitions emit
// Events.
type InstanceReconciler struct {
	// Client is the manager's cached client for the KeycloakInstance CR and status.
	client.Client
	// APIReader is the manager's non-caching reader, used to Get the credential
	// Secret without a cluster-wide Secret cache (the controller holds only get on
	// Secrets, never list/watch).
	APIReader client.Reader
	// Recorder emits Kubernetes Events for ready/failed transitions.
	Recorder record.EventRecorder
	// Namespace is the controller's own namespace, where credential Secrets are
	// resolved. Defaults to DefaultControllerNamespace via controllerNamespace().
	Namespace string
	// NewClient builds the Keycloak client from a resolved credential. Defaults to
	// NewKeycloakInstanceClient; tests override it with a fake factory.
	NewClient InstanceClientFactory
}

// Reconcile drives a KeycloakInstance toward its desired state: fetch CR →
// resolve credential → validate caBundle → build client → GetRealm reachability
// probe → mark Ready with observedGeneration → Status().Update. Credential and
// Keycloak errors map to a False condition with an actionable reason and a
// Warning event, and return an error so the request requeues with backoff.
func (r *InstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	defer func() { recordReconcile(kindInstance, retErr) }()

	instance := &keycloakv1alpha1.KeycloakInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		// Not found: the CR was deleted. Nothing to do; do not requeue.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion is a no-op: no finalizer, no Keycloak-side object to clean up.
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, instance, err)
	}

	// Reject an invalid spec.caBundle up front so a malformed bundle surfaces as a
	// failed reconcile (Ready=False) rather than silently falling back to system
	// trust and possibly reporting Ready=True for an unhonored spec.
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		return r.fail(ctx, instance, err)
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)

	// Reachability probe: a successful GetRealm proves the credential authenticated
	// and the realm exists over the (optionally caBundle-trusted) connection.
	_, getErr := kc.GetRealm(ctx)
	recordKeycloakAPI(opAuthenticate, getErr)
	if getErr != nil {
		return r.fail(ctx, instance, fmt.Errorf("verifying Keycloak realm %q at %q is reachable: %w", instance.Spec.Realm, instance.Spec.URL, getErr))
	}

	return r.succeed(ctx, instance, ReasonReconciled,
		fmt.Sprintf("Keycloak realm %q at %q is reachable", instance.Spec.Realm, instance.Spec.URL))
}

// succeed stamps Ready/Programmed/Accepted true with the given reason+message,
// emits a Normal event, and writes status only when something actually changed (a
// condition flipped or observedGeneration advanced), so a steady-state reconcile
// does not spin a status/update/event loop.
func (r *InstanceReconciler) succeed(ctx context.Context, instance *keycloakv1alpha1.KeycloakInstance, reason, message string) (ctrl.Result, error) {
	changed := markReady(&instance.Status.Conditions, reason, message, instance.Generation)
	changed = changed || instance.Status.ObservedGeneration != instance.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(instance, corev1.EventTypeNormal, reason, message)
	log.FromContext(ctx).Info("reconciled KeycloakInstance", "realm", instance.Spec.Realm, "url", instance.Spec.URL)
	if err := r.updateStatus(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// handleCredentialError maps a credential-resolution error to a reconcile result.
// A missing Secret/key is an expected, recoverable state: it sets a
// CredentialsNotFound condition (writing status + emitting a Warning only when the
// condition changed, to avoid churn) and requeues with the error so the reconcile
// retries once the operator provides the Secret. A transient API error reading the
// Secret requeues with backoff without stamping a misleading reason.
func (r *InstanceReconciler) handleCredentialError(ctx context.Context, instance *keycloakv1alpha1.KeycloakInstance, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&instance.Status.Conditions, ReasonCredentialsNotFound, err.Error(), instance.Generation); changed {
		r.Recorder.Event(instance, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, instance); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

// fail records a Keycloak error as a False condition + Warning event and returns
// the error so the request requeues with backoff. The status write and event are
// emitted only when the condition actually changed, so a persistently failing
// reconcile does not re-emit identical events on every backoff retry — the
// returned error already drives the requeue.
func (r *InstanceReconciler) fail(ctx context.Context, instance *keycloakv1alpha1.KeycloakInstance, err error) (ctrl.Result, error) {
	if changed := markNotReady(&instance.Status.Conditions, ReasonKeycloakError, err.Error(), instance.Generation); changed {
		r.Recorder.Event(instance, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		if statusErr := r.updateStatus(ctx, instance); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after Keycloak error")
		}
	}
	return ctrl.Result{}, err
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict. On conflict it refetches the latest object and re-applies
// the computed status onto it before retrying. A NotFound (the CR was deleted
// concurrently) is ignored — there is nothing left to update.
func (r *InstanceReconciler) updateStatus(ctx context.Context, instance *keycloakv1alpha1.KeycloakInstance) error {
	instance.Status.ObservedGeneration = instance.Generation
	desired := instance.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(instance), instance); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&instance.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating KeycloakInstance status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager: it watches
// KeycloakInstance resources, defaults the namespace and client factory if unset,
// and obtains an event recorder.
func (r *InstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keycloakinstance-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewKeycloakInstanceClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakInstance{}).
		Complete(r)
}

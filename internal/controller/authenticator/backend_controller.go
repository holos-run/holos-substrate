package authenticator

import (
	"context"
	"fmt"
	"net/url"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
	"github.com/holos-run/holos-paas/internal/authenticator"
)

// BackendReconciler reconciles an authenticator.holos.run Backend: it performs
// OIDC issuer discovery, compiles the group-mapping CEL expression, validates the
// spec, sets Gateway-API status, and maintains the host-keyed in-memory Store of
// ready backends the gRPC Check path consumes.
//
// Unlike the quay controllers it programs no external system and so needs no
// finalizer: a deleted Backend leaves nothing behind in a remote system, only an
// entry in the in-memory Store, which the reconcile removes on the not-found
// path. "Programmed" here means OIDC discovery and CEL compilation succeeded and
// the entry was loaded into the Store.
type BackendReconciler struct {
	// Client is the manager's cached client for the Backend CR and its status.
	client.Client
	// APIReader is the manager's non-caching reader. It is wired for parity with
	// the quay controllers (credential Secret reads happen in the Check path,
	// HOL-1388); this phase does not read Secrets.
	APIReader client.Reader
	// Recorder emits Kubernetes Events for ready/failed/rejected transitions.
	Recorder record.EventRecorder
	// Store is the shared host-keyed registry of ready backends. The reconciler is
	// its sole writer; the gRPC Check Runnable reads it. Injected from main so the
	// reconciler and the gRPC server share one instance.
	Store *authenticator.Store
	// Discover performs OIDC discovery and returns a token verifier. Defaults to
	// authenticator.DiscoverVerifier; tests override it with a fake that returns a
	// stub verifier without contacting a live issuer.
	Discover authenticator.DiscoverFunc
}

// +kubebuilder:rbac:groups=authenticator.holos.run,resources=backends,verbs=get;list;watch
// +kubebuilder:rbac:groups=authenticator.holos.run,resources=backends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=authenticator.holos.run,resources=backends/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//
// NOTE: this reconciler intentionally requests NO Secret access. This phase
// records spec.credentialsSecretRef in the store but never reads the Secret —
// the privileged impersonator credential is resolved on the Check path
// (HOL-1388), which must do so via a namespace-scoped Role/RoleBinding rather
// than the cluster-wide `secrets get` a ClusterRole grant would imply (the
// confused-deputy hazard the main.go wiring calls out). Adding `secrets get`
// here would grant cluster-wide Secret reads for a capability this phase does
// not exercise.

// Reconcile drives a Backend toward its desired state. Loop shape: fetch CR
// (not-found ⇒ drop from the Store) → compile the group-mapping CEL (a malformed
// expression rejects the spec, Accepted=False) → perform OIDC discovery (a
// failure is a transient operational error, Programmed=False) → register the
// resolved entry in the Store and mark Ready.
func (r *BackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	backend := &authenticatorv1alpha1.Backend{}
	if err := r.Get(ctx, req.NamespacedName, backend); err != nil {
		if apierrors.IsNotFound(err) {
			// The Backend was deleted. Remove its Store entry by object key — the
			// Store's reverse index resolves the host the not-found Get cannot read.
			r.Store.DeleteByKey(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// A Backend pending deletion: remove it from the Store and let the API server
	// finish the delete. No finalizer is registered, so there is no remote cleanup
	// to gate on.
	if !backend.DeletionTimestamp.IsZero() {
		r.Store.DeleteByKey(req.String())
		return ctrl.Result{}, nil
	}

	return r.reconcileNormal(ctx, logger, backend)
}

// reconcileNormal compiles the CEL expression, performs OIDC discovery, builds
// the Store entry, and marks the Backend Ready.
func (r *BackendReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, backend *authenticatorv1alpha1.Backend) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(backend).String()

	// Compile the group-mapping expression first: it is a pure-spec operation, so
	// a malformed expression is a spec rejection (Accepted=False) independent of
	// any external system. Default an empty expression to the groups-claim mapping.
	expr := backend.Spec.GroupMapping.CELExpression
	if expr == "" {
		expr = authenticator.DefaultGroupExpression(backend.Spec.OIDC.GroupsClaim)
	}
	mapper, err := authenticator.NewGroupMapper(expr)
	if err != nil {
		// A bad CEL expression is an invalid spec: the backend can never become
		// ready until it is corrected, so remove any stale Store entry and reject.
		r.Store.DeleteByKey(key)
		return r.reject(ctx, backend, ReasonInvalidSpec,
			fmt.Sprintf("compiling group-mapping CEL expression: %v", err))
	}

	// Validate the upstream API server URL. The CRD enforces only MinLength, so a
	// non-URL string ("not a url") or an unsupported scheme passes admission; a
	// Backend whose status claims the upstream is valid must not be marked Ready
	// with a URL the Check path could never dial. Reject as an invalid spec.
	if err := validateServerURL(backend.Spec.Server.URL); err != nil {
		r.Store.DeleteByKey(key)
		return r.reject(ctx, backend, ReasonInvalidSpec,
			fmt.Sprintf("invalid spec.server.url: %v", err))
	}

	// The spec parsed and the CEL expression compiled: the resource is Accepted
	// (Gateway-API "the spec was understood"). Set it before discovery so a backend
	// whose discovery then fails still reports Accepted=True — discovery is a
	// programming concern, not a spec-validity one. markReady on the success path
	// re-asserts Accepted=True idempotently.
	markAccepted(&backend.Status.Conditions, ReasonReconciled, "spec accepted and group-mapping expression compiled", backend.Generation)

	// Perform OIDC discovery. A failure is a transient operational error (the
	// issuer may be temporarily unreachable), so it is Programmed=False (not a spec
	// rejection) and requeues with backoff via the returned error.
	verifier, err := r.Discover(ctx, backend.Spec.OIDC.IssuerURL, backend.Spec.OIDC.ClientID, backend.Spec.OIDC.CABundle)
	if err != nil {
		// Discovery failed: the backend is not usable. Drop it from the Store so the
		// Check path stops serving a now-unready backend, mark NotReady, and requeue.
		r.Store.DeleteByKey(key)
		return r.fail(ctx, backend, ReasonDiscoveryFailed,
			fmt.Sprintf("OIDC discovery for issuer %q: %v", backend.Spec.OIDC.IssuerURL, err))
	}

	// Reject a host collision deterministically: if a different Backend already
	// owns this spec.host in the store, do not silently overwrite the data-path
	// routing for that host (which would make the active issuer/upstream depend on
	// reconcile/delete ordering). First claimant wins; the loser reports
	// Ready=False/HostConflict until the host is freed or its own host changes.
	if owner, ok := r.Store.Owner(backend.Spec.Host); ok && owner != key {
		r.Store.DeleteByKey(key)
		return r.fail(ctx, backend, ReasonHostConflict,
			fmt.Sprintf("host %q is already claimed by another Backend (%s)", backend.Spec.Host, owner))
	}

	// Build the resolved entry and register it in the Store keyed by spec.host
	// (the Store records the key→host reverse index so a later delete removes it).
	entry := &authenticator.Entry{
		Host:                 backend.Spec.Host,
		Authenticator:        authenticator.NewAuthenticator(verifier, mapper, backend.Spec.OIDC.UsernameClaim),
		UsernameClaim:        backend.Spec.OIDC.UsernameClaim,
		ServerURL:            backend.Spec.Server.URL,
		ServerCABundle:       backend.Spec.Server.CABundle,
		CredentialsSecretRef: backend.Spec.CredentialsSecretRef,
	}
	r.Store.Set(key, entry)

	return r.succeed(ctx, logger, backend)
}

// succeed marks Accepted/Programmed/Ready true and writes status only on a
// change, mirroring the quay controller's churn-free success path.
func (r *BackendReconciler) succeed(ctx context.Context, logger logr.Logger, backend *authenticatorv1alpha1.Backend) (ctrl.Result, error) {
	message := fmt.Sprintf("backend %q is ready (issuer %q)", backend.Spec.Host, backend.Spec.OIDC.IssuerURL)
	changed := markReady(&backend.Status.Conditions, ReasonReconciled, message, backend.Generation)
	changed = changed || backend.Status.ObservedGeneration != backend.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(backend, corev1.EventTypeNormal, ReasonReconciled, message)
	logger.Info("reconciled Backend", "host", backend.Spec.Host, "issuer", backend.Spec.OIDC.IssuerURL)
	if err := r.updateStatus(ctx, backend); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reject records an invalid-spec rejection: Accepted/Programmed/Ready all False.
// The spec must change to recover, so it does not return an error (no requeue
// storm); a spec edit re-triggers the watch. Status and event are written only on
// a change.
func (r *BackendReconciler) reject(ctx context.Context, backend *authenticatorv1alpha1.Backend, reason, message string) (ctrl.Result, error) {
	changed := markRejected(&backend.Status.Conditions, reason, message, backend.Generation)
	changed = changed || backend.Status.ObservedGeneration != backend.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(backend, corev1.EventTypeWarning, reason, message)
	if err := r.updateStatus(ctx, backend); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// fail records a transient operational failure (Programmed/Ready False, Accepted
// untouched) and returns an error so the request requeues with backoff. Status
// and event are emitted only on a change so a persistently failing reconcile does
// not re-emit identical events on every retry — the returned error drives the
// requeue.
func (r *BackendReconciler) fail(ctx context.Context, backend *authenticatorv1alpha1.Backend, reason, message string) (ctrl.Result, error) {
	if changed := markNotReady(&backend.Status.Conditions, reason, message, backend.Generation); changed {
		r.Recorder.Event(backend, corev1.EventTypeWarning, reason, message)
		if err := r.updateStatus(ctx, backend); err != nil {
			log.FromContext(ctx).Error(err, "updating status after discovery failure")
		}
	}
	return ctrl.Result{}, fmt.Errorf("%s", message)
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict and re-applying the computed status onto the refetched
// object. A concurrent delete (NotFound) is ignored.
func (r *BackendReconciler) updateStatus(ctx context.Context, backend *authenticatorv1alpha1.Backend) error {
	backend.Status.ObservedGeneration = backend.Generation
	desired := backend.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, backend); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(backend), backend); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&backend.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating Backend status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager: it watches Backend
// resources and defaults the recorder, discovery func, and Store if unset.
func (r *BackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("backend-controller")
	}
	if r.Discover == nil {
		r.Discover = authenticator.DiscoverVerifier
	}
	if r.Store == nil {
		r.Store = authenticator.NewStore()
	}
	// Run on every replica, not only the elected leader. The reconciler's job is
	// to populate the process-local Store the always-on ext_authz gRPC server
	// (NeedLeaderElection=false) reads; if it ran only on the leader, non-leader
	// replicas would serve Envoy from an empty store and deny every request. Each
	// replica independently reconciles the same Backends into its own store — there
	// is no external system to coordinate, so concurrent reconcilers are safe (Codex
	// round 2).
	runOnEveryReplica := false
	return ctrl.NewControllerManagedBy(mgr).
		For(&authenticatorv1alpha1.Backend{}).
		WithOptions(controller.Options{NeedLeaderElection: &runOnEveryReplica}).
		Complete(r)
}

// validateServerURL checks that the upstream API server URL is a well-formed
// absolute http(s) URL with a host. The CRD enforces only MinLength, so this
// guards against a Backend being marked Ready with a URL the Check path could
// never dial (a relative path, a missing host, or a non-http scheme).
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https (url %q)", u.Scheme, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in %q", raw)
	}
	return nil
}

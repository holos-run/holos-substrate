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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	reconcilepkg "sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// reconcileNormal compiles the CEL expression, resolves a token verifier (an
// offline static-JWKS verifier when spec.oidc.jwks is set, else OIDC discovery),
// builds the Store entry, and marks the Backend Ready.
func (r *BackendReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, backend *authenticatorv1alpha1.Backend) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(backend).String()

	// Compile the group-mapping expression first: it is a pure-spec operation, so
	// a malformed expression is a spec rejection (Accepted=False) independent of
	// any external system. Default an empty expression to the groups-claim mapping.
	expr := backend.Spec.GroupMapping.CELExpression
	if expr == "" {
		expr = authenticator.DefaultGroupExpression(backend.Spec.OIDC.GroupsClaim, backend.Spec.OIDC.GroupsPrefix)
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

	// Validate the extra-mapping keys: each is emitted verbatim as the suffix of an
	// Impersonate-Extra-<key> header, so it must be a valid HTTP header token. A bad
	// key is an invalid spec (Accepted=False) — like a malformed CEL expression or a
	// bad URL — fixed only by editing the spec, so reject without requeue. Key
	// uniqueness is already enforced by the CRD's listMapKey.
	for _, m := range backend.Spec.OIDC.Extra {
		if err := authenticator.ValidateExtraKey(m.Key); err != nil {
			r.Store.DeleteByKey(key)
			return r.reject(ctx, backend, ReasonInvalidSpec,
				fmt.Sprintf("invalid spec.oidc.extra key: %v", err))
		}
	}

	// The spec parsed and the CEL expression compiled: the resource is Accepted
	// (Gateway-API "the spec was understood"). Set it before discovery so a backend
	// whose discovery then fails still reports Accepted=True — discovery is a
	// programming concern, not a spec-validity one. markReady on the success path
	// re-asserts Accepted=True idempotently.
	markAccepted(&backend.Status.Conditions, ReasonReconciled, "spec accepted and group-mapping expression compiled", backend.Generation)

	// Select the verifier source by spec. A static JWKS is pure spec data validated
	// entirely offline (no external system), so a parse/usable-key failure is an
	// invalid-spec rejection (Accepted=False), not a transient DiscoveryFailed.
	// Otherwise discovery is a transient operational concern (the issuer may be
	// temporarily unreachable): Programmed=False, requeued with backoff.
	var verifier authenticator.TokenVerifier
	if len(backend.Spec.OIDC.JWKS) > 0 {
		verifier, err = authenticator.StaticVerifier(backend.Spec.OIDC.IssuerURL, backend.Spec.OIDC.ClientID, backend.Spec.OIDC.JWKS)
		if err != nil {
			// The static JWKS could not yield a verifier: the backend can never become
			// ready until the spec is corrected, so remove any stale Store entry and
			// reject (no requeue storm — a spec edit re-triggers the watch).
			r.Store.DeleteByKey(key)
			return r.reject(ctx, backend, ReasonInvalidSpec,
				fmt.Sprintf("invalid spec.oidc.jwks: %v", err))
		}
	} else {
		verifier, err = r.Discover(ctx, backend.Spec.OIDC.IssuerURL, backend.Spec.OIDC.ClientID, backend.Spec.OIDC.CABundle)
		if err != nil {
			// Discovery failed: the backend is not usable. Drop it from the Store so the
			// Check path stops serving a now-unready backend, mark NotReady, and requeue.
			r.Store.DeleteByKey(key)
			return r.fail(ctx, backend, ReasonDiscoveryFailed,
				fmt.Sprintf("OIDC discovery for issuer %q: %v", backend.Spec.OIDC.IssuerURL, err))
		}
	}

	// Build the resolved entry and attempt to register it keyed by spec.host. The
	// Store resolves a host collision deterministically (the lexicographically
	// smallest object key wins), so every replica converges on the same owner
	// regardless of the order it observes the colliding Backends. Set returns false
	// when this key lost the tie-break: report Ready=False/HostConflict (and requeue
	// so it recovers if the winner is later deleted) rather than silently
	// overwriting the data-path routing for that host.
	// Resolve the credential ref to a value. credentialsSecretRef is a pointer so
	// the CRD CEL rule can distinguish an omitted ref from a set one; a nil ref
	// means "use the default Secret", which resolveImpersonatorToken already
	// produces from a zero SecretReference (its Name defaults to
	// DefaultCredentialsSecretName). Keep Entry's value-typed field unchanged.
	var credRef authenticatorv1alpha1.SecretReference
	if backend.Spec.CredentialsSecretRef != nil {
		credRef = *backend.Spec.CredentialsSecretRef
	}
	// Record the ServiceAccount credential source when set, normalized with its CRD
	// defaults so the Check path need not re-apply them — the CRD defaults populate
	// for an API-server-validated CR, but normalizing here keeps the Check path's
	// minting deterministic even if an Entry were ever built from a CR that bypassed
	// defaulting (HOL-1400). serviceAccountRef and credentialsSecretRef are mutually
	// exclusive (the CRD CEL rule); the Check path additionally prefers the SA path
	// when both are somehow present, so recording the SA ref is sufficient.
	saRef := normalizeServiceAccountRef(backend.Spec.ServiceAccountRef)
	entry := &authenticator.Entry{
		Host:                 backend.Spec.Host,
		Authenticator:        authenticator.NewAuthenticator(verifier, mapper, backend.Spec.OIDC.UsernameClaim, backend.Spec.OIDC.UsernamePrefix, backend.Spec.OIDC.UIDClaim, backend.Spec.OIDC.Extra),
		UsernameClaim:        backend.Spec.OIDC.UsernameClaim,
		ServerURL:            backend.Spec.Server.URL,
		ServerCABundle:       backend.Spec.Server.CABundle,
		CredentialsSecretRef: credRef,
		ServiceAccountRef:    saRef,
	}
	if !r.Store.Set(key, entry) {
		owner, _ := r.Store.Owner(backend.Spec.Host)
		return r.fail(ctx, backend, ReasonHostConflict,
			fmt.Sprintf("host %q is already claimed by another Backend (%s); the lexicographically-smallest Backend key owns a shared host", backend.Spec.Host, owner))
	}

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
		// Re-enqueue every other Backend that shares a changed Backend's spec.host
		// so host-collision losers/winners re-converge promptly. Without this, a
		// Backend that was Ready and then loses its host to a newly-created smaller
		// key (or that should reclaim a host whose owner was just deleted) would keep
		// a stale Ready/HostConflict status until some unrelated reconcile. The map
		// func lists Backends and returns the requests for those matching the host;
		// the event source's own object is excluded because For() already enqueues it.
		Watches(&authenticatorv1alpha1.Backend{}, handler.EnqueueRequestsFromMapFunc(r.backendsSharingHost)).
		WithOptions(controller.Options{NeedLeaderElection: &runOnEveryReplica}).
		Complete(r)
}

// backendsSharingHost returns reconcile requests for every Backend that declares
// the same spec.host as the changed object (excluding the object itself, which
// For() already enqueues). It is the watch map func that keeps host-collision
// status convergent: when one Backend claiming a shared host changes, its
// co-claimants re-reconcile and re-evaluate ownership. A List error yields no
// requests (best effort) — the periodic informer resync remains a backstop.
func (r *BackendReconciler) backendsSharingHost(ctx context.Context, obj client.Object) []reconcilepkg.Request {
	changed, ok := obj.(*authenticatorv1alpha1.Backend)
	if !ok {
		return nil
	}
	var list authenticatorv1alpha1.BackendList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing Backends for host-sharing requeue")
		return nil
	}
	var reqs []reconcilepkg.Request
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.Host != changed.Spec.Host {
			continue
		}
		if b.Namespace == changed.Namespace && b.Name == changed.Name {
			continue // For() already enqueues the changed object itself
		}
		reqs = append(reqs, reconcilepkg.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	}
	return reqs
}

// normalizeServiceAccountRef returns a copy of ref with its CRD defaults applied
// (Name=DefaultImpersonatorServiceAccountName when empty,
// ExpirationSeconds=DefaultTokenExpirationSeconds when nil or non-positive), or nil
// when ref is nil. The CRD defaults populate these for any API-server-validated
// Backend, but normalizing here makes the Check path's TokenManager.Token call
// deterministic regardless — the Entry carries a fully-resolved ref, so the Check
// path applies no further defaulting. The returned ref is a fresh value so the
// Entry never aliases the live spec's pointer.
func normalizeServiceAccountRef(ref *authenticatorv1alpha1.ServiceAccountReference) *authenticatorv1alpha1.ServiceAccountReference {
	if ref == nil {
		return nil
	}
	out := *ref
	if out.Name == "" {
		out.Name = authenticatorv1alpha1.DefaultImpersonatorServiceAccountName
	}
	if out.ExpirationSeconds == nil || *out.ExpirationSeconds <= 0 {
		def := authenticator.DefaultTokenExpirationSeconds
		out.ExpirationSeconds = &def
	}
	return &out
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

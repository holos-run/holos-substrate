package quay

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// organizationFinalizer guards Quay-side cleanup: while it is present, deleting
// the Organization CR runs the finalizer (which deletes the Quay org) before the
// CR is removed from the API server. Its value is the resource's qualified name
// so it is unambiguous among any other finalizers.
const organizationFinalizer = "organization.quay.holos.run/finalizer"

// OrgClient is the seam the Organization reconciler drives Quay through. It is
// the subset of internal/quay.Client's organization operations the reconciler
// needs, named as an interface so tests inject a fake without HTTP. The concrete
// *quay.Client satisfies it.
type OrgClient interface {
	// GetOrganization fetches a Quay organization; a missing org returns an
	// error for which quay.IsNotFound reports true.
	GetOrganization(ctx context.Context, name string) (*quay.Organization, error)
	// CreateOrganization creates the org, returning an error for which
	// quay.IsConflict reports true when the org already exists — the reconciler
	// branches on that to avoid falsely claiming ownership of a create race.
	CreateOrganization(ctx context.Context, name, email string) error
	// DeleteOrganizationIfExists deletes the org, treating an already-absent
	// response as success (idempotent).
	DeleteOrganizationIfExists(ctx context.Context, name string) error
}

// ClientFactory builds an OrgClient from a resolved Quay credential. The default
// factory (NewQuayClient) returns a real *quay.Client; tests substitute a factory
// that returns a fake, which is how the reconciler is exercised without a live
// Quay or HTTP.
type ClientFactory func(cred *quayCredential) OrgClient

// NewQuayClient is the production ClientFactory: it builds a real internal/quay
// client from the credential's url and token.
func NewQuayClient(cred *quayCredential) OrgClient {
	return quay.NewClient(cred.url, cred.token, nil)
}

// Compile-time assertion that the real Quay client satisfies the reconciler's
// seam, so a signature drift in internal/quay is caught at build time.
var _ OrgClient = (*quay.Client)(nil)

// OrganizationReconciler reconciles a quay.holos.run Organization against the
// in-cluster Quay registry: it creates or adopts the named Quay organization and,
// on delete, runs a finalizer that deletes it. Status follows the Gateway-API
// convention (see conditions.go) and meaningful transitions emit Events.
type OrganizationReconciler struct {
	// Client is the manager's cached client for the Organization CR and status.
	client.Client
	// APIReader is the manager's non-caching reader, used to Get the credential
	// Secret without a cluster-wide Secret cache (the controller holds only get
	// on Secrets, never list/watch).
	APIReader client.Reader
	// Recorder emits Kubernetes Events for created/adopted/failed/deleted
	// transitions (AC #2).
	Recorder record.EventRecorder
	// Namespace is the controller's own namespace, where credential Secrets are
	// resolved. Defaults to DefaultControllerNamespace via controllerNamespace().
	Namespace string
	// NewClient builds the Quay client from a resolved credential. Defaults to
	// NewQuayClient; tests override it with a fake factory.
	NewClient ClientFactory
}

// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=quay.holos.run,resources=organizations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives an Organization toward its desired state. Loop shape:
// fetch CR → ensure finalizer → on delete run Quay delete then remove finalizer →
// else resolve credential → GetOrganization (404 ⇒ create, else adopt) → mark
// Ready with observedGeneration → Status().Update. Credential and Quay errors map
// to a False condition with an actionable reason and a Warning event, and return
// an error so the request requeues with backoff.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	org := &quayv1alpha1.Organization{}
	if err := r.Get(ctx, req.NamespacedName, org); err != nil {
		// Not found: the CR was deleted and its finalizer already ran. Nothing
		// to do; do not requeue.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: run the finalizer (delete the Quay org) then drop it so the
	// API server can remove the CR.
	if !org.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, org)
	}

	// Ensure the finalizer is present before doing any Quay work, so a delete
	// that races in still triggers Quay-side cleanup.
	if controllerutil.AddFinalizer(org, organizationFinalizer) {
		if err := r.Update(ctx, org); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// The update changes the resourceVersion; requeue to reconcile the
		// fresh object rather than continuing with a stale one.
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileNormal(ctx, logger, org)
}

// reconcileNormal resolves the credential, creates or adopts the Quay org, and
// updates status.
func (r *OrganizationReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, org.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, org, err)
	}

	qc := r.NewClient(cred)

	// ADR-19 claim model. Quay orgs are a single global namespace and the
	// controller credential carries FEATURE_SUPERUSERS_FULL_ACCESS, so a naive
	// "adopt any existing org" rule would let a namespaced CR seize another
	// tenant's org. The durable owner record is status.Created: true means this
	// CR created the org (and the finalizer may delete it); false means it
	// adopted a pre-existing one (released, never deleted, on removal).
	//
	// GET the org and branch on the claim-model cases.
	_, getErr := qc.GetOrganization(ctx, org.Spec.Name)
	switch {
	case quay.IsNotFound(getErr):
		// Org does not appear to exist → try to create it. Use CreateOrganization
		// (not the IfNotExists wrapper) so a conflict is observable: if another
		// actor created the org between the GET and this POST, a swallowed
		// conflict would let us falsely stamp ownership and later delete an org
		// we did not create. On conflict, fall through to the exists-path claim
		// decision (adopt or Conflict) rather than claiming ownership.
		err := qc.CreateOrganization(ctx, org.Spec.Name, org.Spec.Email)
		switch {
		case err == nil:
			// Clean create: this CR is the owner.
			org.Status.Created = true
			return r.succeed(ctx, logger, org, ReasonCreated,
				fmt.Sprintf("created Quay organization %q", org.Spec.Name))
		case quay.IsConflict(err):
			// Lost the create race: the org now exists but this CR did not
			// create it. Treat it exactly like the exists-unowned path.
			return r.reconcileExistingUnowned(ctx, logger, org)
		default:
			return r.fail(ctx, org, fmt.Errorf("creating Quay organization %q: %w", org.Spec.Name, err))
		}

	case getErr == nil && org.Status.Created:
		// Org exists and this CR created it (owner marker present) → steady
		// state. Reconcile idempotently; never error on an org we own.
		return r.succeed(ctx, logger, org, ReasonCreated,
			fmt.Sprintf("reconciled Quay organization %q", org.Spec.Name))

	case getErr == nil:
		// Org exists and this CR did not create it → adopt (if opted in) or
		// Conflict.
		return r.reconcileExistingUnowned(ctx, logger, org)

	default:
		// Any other Quay error (auth, server error): fail and requeue.
		return r.fail(ctx, org, fmt.Errorf("getting Quay organization %q: %w", org.Spec.Name, getErr))
	}
}

// reconcileExistingUnowned handles a Quay org that exists but was not created by
// this CR: adopt it when spec.adopt is set (status.Created stays false so the
// finalizer releases rather than deletes), otherwise refuse to write and set a
// terminal Conflict condition (no requeue storm — a spec change re-triggers).
func (r *OrganizationReconciler) reconcileExistingUnowned(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	if org.Spec.Adopt {
		org.Status.Created = false
		return r.succeed(ctx, logger, org, ReasonAdopted,
			fmt.Sprintf("adopted existing Quay organization %q", org.Spec.Name))
	}

	message := fmt.Sprintf("Quay organization %q already exists and was not created by this resource; set spec.adopt to claim it", org.Spec.Name)
	changed := setConflict(&org.Status.Conditions, message, org.Generation)
	changed = changed || org.Status.ObservedGeneration != org.Generation
	if !changed {
		// Terminal conflict already recorded; do not rewrite identical status
		// (which would re-enqueue the object and spin a watch-triggered loop).
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(org, corev1.EventTypeWarning, ReasonConflict, message)
	logger.Info("Organization conflict", "name", org.Spec.Name)
	if err := r.updateStatus(ctx, org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// succeed stamps Ready/Programmed/Accepted true with the given reason+message.
// It emits a Normal event and writes status only when something actually changed
// (a condition flipped or observedGeneration advanced), so a steady-state
// reconcile of an unchanged resource does not write status — which would
// otherwise re-enqueue the object and spin a reconcile/update/event loop.
func (r *OrganizationReconciler) succeed(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization, reason, message string) (ctrl.Result, error) {
	changed := markReady(&org.Status.Conditions, reason, message, org.Generation)
	changed = changed || org.Status.ObservedGeneration != org.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(org, corev1.EventTypeNormal, reason, message)
	logger.Info("reconciled Organization", "name", org.Spec.Name, "reason", reason)
	if err := r.updateStatus(ctx, org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileDelete runs the finalizer. Per ADR-19's claim model the Quay org is
// deleted only when this CR created it (status.Created); an adopted org is
// released (the finalizer drops without deleting), so removing a CR that merely
// claimed a pre-existing org never destroys it. After cleanup the finalizer is
// removed so the CR is deleted. A Quay error during delete fails the reconcile
// and requeues, so the finalizer is not removed until cleanup succeeds.
func (r *OrganizationReconciler) reconcileDelete(ctx context.Context, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(org, organizationFinalizer) {
		// Already finalized; nothing to do.
		return ctrl.Result{}, nil
	}

	// Adopted org (or one never created by this CR) → release, do not delete.
	// No credential is needed: the controller does not touch Quay, it only
	// relinquishes its claim.
	if !org.Status.Created {
		r.Recorder.Event(org, corev1.EventTypeNormal, ReasonReleased,
			fmt.Sprintf("released Quay organization %q without deleting (adopted, not created by this resource)", org.Spec.Name))
		return r.removeFinalizer(ctx, org)
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, org.Spec.CredentialsSecretRef)
	if err != nil {
		// Without the credential the Quay org cannot be deleted; do not strand
		// the CR by dropping the finalizer when cleanup could not run. Surface
		// the condition and requeue.
		return r.handleCredentialError(ctx, org, err)
	}

	qc := r.NewClient(cred)
	if err := qc.DeleteOrganizationIfExists(ctx, org.Spec.Name); err != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, ReasonQuayError,
			fmt.Sprintf("deleting Quay organization %q: %v", org.Spec.Name, err))
		return ctrl.Result{}, fmt.Errorf("deleting Quay organization %q: %w", org.Spec.Name, err)
	}

	r.Recorder.Event(org, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("deleted Quay organization %q", org.Spec.Name))

	return r.removeFinalizer(ctx, org)
}

// removeFinalizer drops the organization finalizer and persists the change so
// the API server can delete the CR.
func (r *OrganizationReconciler) removeFinalizer(ctx context.Context, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(org, organizationFinalizer)
	if err := r.Update(ctx, org); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleCredentialError maps a credential-resolution error to a reconcile
// result. A missing Secret/key is an expected, recoverable state: it sets a
// CredentialsNotFound condition (writing status + emitting a Warning only when
// the condition changed, to avoid churn) and requeues with the error so the
// reconcile retries once the operator provides the Secret. A transient API error
// reading the Secret requeues with backoff without stamping a misleading reason.
func (r *OrganizationReconciler) handleCredentialError(ctx context.Context, org *quayv1alpha1.Organization, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&org.Status.Conditions, ReasonCredentialsNotFound, err.Error(), org.Generation); changed {
		r.Recorder.Event(org, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, org); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

// fail records a Quay error as a False condition + Warning event and returns the
// error so the request requeues with backoff. The status write and event are
// emitted only when the condition actually changed, so a persistently failing
// reconcile does not re-emit identical events on every backoff retry — the
// returned error already drives the requeue.
func (r *OrganizationReconciler) fail(ctx context.Context, org *quayv1alpha1.Organization, err error) (ctrl.Result, error) {
	if changed := markNotReady(&org.Status.Conditions, ReasonQuayError, err.Error(), org.Generation); changed {
		r.Recorder.Event(org, corev1.EventTypeWarning, ReasonQuayError, err.Error())
		if statusErr := r.updateStatus(ctx, org); statusErr != nil {
			// Prefer surfacing the original Quay error; log the status failure.
			log.FromContext(ctx).Error(statusErr, "updating status after Quay error")
		}
	}
	return ctrl.Result{}, err
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict. The retry is load-bearing for the ownership marker
// (status.Created): a create side effect already happened in Quay, so the marker
// MUST persist — silently dropping a conflicting write would lose it and let the
// next reconcile mistake the org for foreign (and release rather than delete it).
// On conflict it refetches the latest object and re-applies the computed status
// (conditions, Created, observedGeneration) onto it before retrying. A NotFound
// (the CR was deleted concurrently) is ignored — there is nothing left to update.
func (r *OrganizationReconciler) updateStatus(ctx context.Context, org *quayv1alpha1.Organization) error {
	org.Status.ObservedGeneration = org.Generation
	desired := org.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, org); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				// Refetch and re-apply the desired status onto the fresh object,
				// then let RetryOnConflict try the update again.
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(org), org); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&org.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating Organization status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager: it watches
// Organization resources, defaults the namespace and client factory if unset, and
// obtains an event recorder.
func (r *OrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("organization-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewQuayClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&quayv1alpha1.Organization{}).
		Complete(r)
}

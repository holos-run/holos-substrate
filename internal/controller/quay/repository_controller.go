package quay

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// repositoryFinalizer guards Quay-side cleanup of the repository (and its
// repo_push notification): while it is present, deleting the Repository CR runs
// the finalizer before the CR is removed from the API server.
const repositoryFinalizer = "repository.quay.holos.run/finalizer"

// webhookTitle labels the repo_push notification this controller manages so a
// human reading the Quay UI can tell it apart from manually-created webhooks, and
// so the reconciler only deletes notifications it owns.
const webhookTitle = "holos-controller repo_push"

// webhookSecretResyncInterval is how often a Repository whose webhook URL comes
// from a urlSecretRef is re-reconciled, so a change to the referenced Secret's
// value is eventually picked up. The controller cannot watch Secrets (it holds
// get, not list/watch, and uses a non-caching reader), so this periodic requeue
// stands in for a Secret watch (AC #8). The interval mirrors the Quay team-resync
// cadence used elsewhere in the platform.
const webhookSecretResyncInterval = 30 * time.Minute

// RepoClient is the seam the Repository reconciler drives Quay through: the
// subset of internal/quay.Client's organization-existence, repository, and
// notification operations the reconciler needs. Naming it an interface lets
// tests inject a fake without HTTP; the concrete *quay.Client satisfies it.
type RepoClient interface {
	// GetRepository fetches ns/repo; a missing repository returns an error for
	// which quay.IsNotFound reports true.
	GetRepository(ctx context.Context, ns, repo string) (*quay.Repository, error)
	// CreateRepository creates an image repository with the given visibility and
	// description.
	CreateRepository(ctx context.Context, ns, repo, visibility, description string) error
	// UpdateRepositoryVisibility sets the repository visibility.
	UpdateRepositoryVisibility(ctx context.Context, ns, repo, visibility string) error
	// UpdateRepositoryDescription sets the repository description.
	UpdateRepositoryDescription(ctx context.Context, ns, repo, description string) error
	// DeleteRepositoryIfExists deletes the repository, treating an already-absent
	// response as success (idempotent).
	DeleteRepositoryIfExists(ctx context.Context, ns, repo string) error

	// ListNotifications returns the repository's notifications so the reconciler
	// can find an existing repo_push webhook before creating one.
	ListNotifications(ctx context.Context, ns, repo string) ([]quay.Notification, error)
	// CreateNotification creates a repo_push webhook notification delivering to
	// url, labeled title.
	CreateNotification(ctx context.Context, ns, repo, url, title string) (*quay.Notification, error)
	// DeleteNotificationIfExists deletes the notification by uuid, treating an
	// already-absent response as success (idempotent).
	DeleteNotificationIfExists(ctx context.Context, ns, repo, uuid string) error
}

// RepoClientFactory builds a RepoClient from a resolved Quay credential. The
// default factory (NewQuayRepoClient) returns a real *quay.Client; tests
// substitute a factory that returns a fake.
type RepoClientFactory func(cred *quayCredential) RepoClient

// NewQuayRepoClient is the production RepoClientFactory: it builds a real
// internal/quay client from the credential's url and token.
func NewQuayRepoClient(cred *quayCredential) RepoClient {
	return quay.NewClient(cred.url, cred.token, nil)
}

// Compile-time assertion that the real Quay client satisfies the Repository
// reconciler's seam, so a signature drift in internal/quay is caught at build
// time.
var _ RepoClient = (*quay.Client)(nil)

// RepositoryReconciler reconciles a quay.holos.run Repository against the
// in-cluster Quay registry: it creates or updates the named repository inside an
// existing Organization, configures a repo_push webhook from spec.webhook (an
// inline url or a urlSecretRef), and on delete runs a finalizer that deletes the
// repository. Repository creation/configuration happens solely here (AC #9) —
// the Organization reconciler never touches repositories. Status follows the same
// Gateway-API convention as Organization (see conditions.go) and meaningful
// transitions emit Events.
type RepositoryReconciler struct {
	// Client is the manager's cached client for the Repository CR and status.
	client.Client
	// APIReader is the manager's non-caching reader, used to Get the credential
	// Secret (in the controller namespace) and the webhook URL Secret (in the
	// Repository's namespace) without a cluster-wide Secret cache.
	APIReader client.Reader
	// Recorder emits Kubernetes Events for created/updated/failed/deleted
	// transitions (AC #5).
	Recorder record.EventRecorder
	// Namespace is the controller's own namespace, where credential Secrets are
	// resolved. Defaults to DefaultControllerNamespace via controllerNamespace().
	Namespace string
	// NewClient builds the Quay client from a resolved credential. Defaults to
	// NewQuayRepoClient; tests override it with a fake factory.
	NewClient RepoClientFactory
}

// +kubebuilder:rbac:groups=quay.holos.run,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quay.holos.run,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=quay.holos.run,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a Repository toward its desired state. Loop shape:
// fetch CR → ensure finalizer → on delete run Quay delete then remove finalizer →
// else resolve credential → confirm the owning Quay org exists (never create it,
// AC #9) → GetRepository (404 ⇒ create, else reconcile visibility/description) →
// resolve and reconcile the repo_push webhook → mark Ready/WebhookConfigured with
// observedGeneration → Status().Update. Credential, org-not-ready, webhook, and
// Quay errors map to a False condition with an actionable reason; recoverable
// ones (missing credential/webhook Secret, org not yet ready) requeue.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	repo := &quayv1alpha1.Repository{}
	if err := r.Get(ctx, req.NamespacedName, repo); err != nil {
		// Not found: the CR was deleted and its finalizer already ran.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: run the finalizer (delete the Quay repo) then drop it.
	if !repo.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, repo)
	}

	// Ensure the finalizer is present before any Quay work so a racing delete
	// still triggers cleanup.
	if controllerutil.AddFinalizer(repo, repositoryFinalizer) {
		if err := r.Update(ctx, repo); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileNormal(ctx, logger, repo)
}

// reconcileNormal resolves the credential, resolves the owning Quay org through
// the Organization CR, creates or updates the Quay repository, reconciles the
// webhook, and updates status.
func (r *RepositoryReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, repo *quayv1alpha1.Repository) (ctrl.Result, error) {
	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, repo.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, repo, err)
	}

	qc := r.NewClient(cred)

	// Resolve the Quay org name through the Organization CR named by
	// spec.organizationRef in this Repository's namespace — never trust the ref as
	// a Quay org name directly. This binds the Repository to a Quay org a
	// same-namespace Organization resource has claimed (ADR-19 claim model), so
	// the controller's superuser credential cannot be used to write into an
	// arbitrary org by string. AC #9: the Repository reconciler never creates the
	// org; it requeues until the Organization CR exists and is Ready.
	quayOrg, result, handled, err := r.resolveQuayOrg(ctx, repo)
	if handled {
		return result, err
	}

	// Create the repository or reconcile its visibility/description drift.
	if err := r.ensureRepository(ctx, qc, repo, quayOrg); err != nil {
		return r.fail(ctx, repo, err)
	}

	// Reconcile the repo_push webhook (if any). A recoverable webhook-URL Secret
	// miss sets WebhookConfigured=False and requeues without failing the whole
	// reconcile; an invalid spec.webhook is a terminal (no-requeue) condition.
	// Either of those is "handled": the webhook step already wrote the
	// terminal/recoverable status, so the success path must not run and overwrite
	// it with Ready=True. webhookChanged tells succeed() whether the
	// WebhookConfigured condition flipped, so a webhook URL change that does not
	// bump the generation still gets persisted.
	handled, webhookChanged, result, err := r.reconcileWebhook(ctx, logger, qc, repo, quayOrg)
	if handled {
		return result, err
	}

	res, err := r.succeed(ctx, logger, repo, webhookChanged)
	if err != nil {
		return res, err
	}

	// The controller resolves only the specific webhook-URL Secret a CR names, via
	// a non-caching get (no cluster-wide Secret informer — the RBAC grants Secrets
	// get only, not list/watch), so it cannot watch Secrets to be woken on a value
	// change. For a urlSecretRef-backed webhook, periodically requeue so a later
	// change to the Secret's value is picked up and re-pushed to Quay (AC #8's
	// "watch the referenced Secret, or requeue on a sane interval"). An inline or
	// absent webhook needs no resync — its desired URL lives entirely in the spec,
	// which already triggers reconciliation on change.
	if usesWebhookSecretRef(repo) {
		res.RequeueAfter = webhookSecretResyncInterval
	}
	return res, nil
}

// usesWebhookSecretRef reports whether the Repository's webhook draws its URL
// from a urlSecretRef (rather than an inline url or no webhook at all).
func usesWebhookSecretRef(repo *quayv1alpha1.Repository) bool {
	return repo.Spec.Webhook != nil && repo.Spec.Webhook.UrlSecretRef != nil
}

// resolveQuayOrg resolves the Quay organization name the Repository writes into
// by fetching the Organization CR named by spec.organizationRef in the
// Repository's own namespace and reading its spec.name. It returns the Quay org
// name when the Organization CR exists and is Ready; otherwise it records an
// OrganizationNotReady condition, requeues, and reports handled=true so the
// caller returns immediately.
//
// Going through the CR (rather than treating organizationRef as a Quay org name)
// is the security boundary: a Repository may only target a Quay org that a
// same-namespace Organization resource has claimed, so the controller's
// superuser credential cannot create/update repos or webhooks in an arbitrary
// org named by an unprivileged string.
func (r *RepositoryReconciler) resolveQuayOrg(ctx context.Context, repo *quayv1alpha1.Repository) (quayOrg string, result ctrl.Result, handled bool, err error) {
	org := &quayv1alpha1.Organization{}
	key := client.ObjectKey{Namespace: repo.Namespace, Name: repo.Spec.OrganizationRef}
	if getErr := r.Get(ctx, key, org); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			res, e := r.handleOrgNotReady(ctx, repo,
				fmt.Sprintf("Organization %q (spec.organizationRef) not found in namespace %q", repo.Spec.OrganizationRef, repo.Namespace))
			return "", res, true, e
		}
		// Transient API error reading the CR: requeue with backoff without
		// stamping a misleading reason.
		return "", ctrl.Result{}, true, fmt.Errorf("reading Organization %q: %w", repo.Spec.OrganizationRef, getErr)
	}

	if !meta.IsStatusConditionTrue(org.Status.Conditions, ConditionReady) {
		res, e := r.handleOrgNotReady(ctx, repo,
			fmt.Sprintf("Organization %q is not Ready yet", repo.Spec.OrganizationRef))
		return "", res, true, e
	}

	return org.Spec.Name, ctrl.Result{}, false, nil
}

// ensureRepository creates the Quay repository when absent, otherwise corrects
// visibility and description drift. quayOrg is the resolved Quay organization
// name (Organization.spec.name). On first create it records the provisioned Quay
// path in status so the finalizer can delete exactly it. Visibility is compared
// against Quay's GET boolean is_public; description is compared against the spec.
func (r *RepositoryReconciler) ensureRepository(ctx context.Context, qc RepoClient, repo *quayv1alpha1.Repository, quayOrg string) error {
	ns := quayOrg
	name := repo.Spec.Name
	visibility := string(repo.Spec.Visibility)

	current, err := qc.GetRepository(ctx, ns, name)
	if quay.IsNotFound(err) {
		if createErr := qc.CreateRepository(ctx, ns, name, visibility, repo.Spec.Description); createErr != nil {
			return fmt.Errorf("creating Quay repository %s/%s: %w", ns, name, createErr)
		}
		// Record the provisioned identity so deletion targets exactly this path.
		repo.Status.QuayRepository = ns + "/" + name
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting Quay repository %s/%s: %w", ns, name, err)
	}
	repo.Status.QuayRepository = ns + "/" + name

	// Repository exists: correct drift. Quay returns visibility as the boolean
	// is_public on GET, so map the desired public/private to that to compare.
	wantPublic := repo.Spec.Visibility == quayv1alpha1.RepositoryVisibilityPublic
	if current.IsPublic != wantPublic {
		if err := qc.UpdateRepositoryVisibility(ctx, ns, name, visibility); err != nil {
			return fmt.Errorf("updating Quay repository %s/%s visibility: %w", ns, name, err)
		}
	}
	if current.Description != repo.Spec.Description {
		if err := qc.UpdateRepositoryDescription(ctx, ns, name, repo.Spec.Description); err != nil {
			return fmt.Errorf("updating Quay repository %s/%s description: %w", ns, name, err)
		}
	}
	return nil
}

// reconcileWebhook ensures the repository has exactly one repo_push notification
// matching the resolved webhook URL. When spec.webhook is unset the repository is
// intentionally webhookless: WebhookConfigured is set False (reason
// WebhookNotConfigured) and existing controller-managed repo_push notifications
// are removed.
//
// It returns handled=true when the webhook step itself owns the reconcile
// outcome — an invalid spec.webhook (terminal condition, no requeue) or a
// recoverable webhook-URL miss (requeue), or a Quay/API error — so the caller
// returns immediately without running the success path that would overwrite the
// status it just wrote. On a clean configuration (webhook programmed, or no
// webhook desired) it returns handled=false, leaving the WebhookConfigured
// condition staged for the caller's single Status().Update via succeed().
//
// webhookChanged reports whether the staged WebhookConfigured condition actually
// changed, so succeed() persists status even when Ready and observedGeneration
// are unchanged — e.g. a urlSecretRef value changed (Quay was updated) without a
// generation bump, which must not leave WebhookConfigured stale.
func (r *RepositoryReconciler) reconcileWebhook(ctx context.Context, logger logr.Logger, qc RepoClient, repo *quayv1alpha1.Repository, quayOrg string) (handled, webhookChanged bool, result ctrl.Result, err error) {
	ns := quayOrg
	name := repo.Spec.Name

	if repo.Spec.Webhook == nil {
		// No webhook desired: drop any controller-managed repo_push notifications
		// (e.g. spec.webhook was removed) and record the intentional absence. This
		// is a clean state — fall through to succeed() so Ready is set.
		if err := r.removeManagedWebhooks(ctx, qc, ns, name); err != nil {
			return true, false, ctrl.Result{}, r.failErr(ctx, repo, err)
		}
		changed := setWebhookCondition(&repo.Status.Conditions, metav1.ConditionFalse,
			ReasonWebhookNotConfigured, "no webhook configured", repo.Generation)
		return false, changed, ctrl.Result{}, nil
	}

	url, resolveErr := resolveWebhookURL(ctx, r.APIReader, repo.Namespace, repo.Spec.Webhook)
	if resolveErr != nil {
		switch {
		case isInvalidWebhook(resolveErr):
			// Terminal: a spec change re-triggers reconciliation. Set a False
			// condition and do not requeue.
			res, e := r.handleWebhookCondition(ctx, repo, ReasonInvalidWebhook, resolveErr.Error(), false)
			return true, false, res, e
		case isWebhookURLNotFound(resolveErr):
			// Recoverable: requeue (error) so a later-created Secret takes effect.
			res, e := r.handleWebhookCondition(ctx, repo, ReasonWebhookURLNotFound, resolveErr.Error(), true)
			return true, false, res, e
		default:
			// Transient API error reading the Secret: requeue with backoff.
			return true, false, ctrl.Result{}, resolveErr
		}
	}

	if err := r.ensureWebhook(ctx, qc, ns, name, url); err != nil {
		// Quay's error body for a webhook call can echo the submitted config,
		// including the target URL. For a secret-backed URL that value is sensitive
		// (the hard-to-guess Kargo receiver URL is held in a Secret precisely so it
		// is not exposed), so redact it before the error reaches the status
		// condition that failErr writes. The returned (unredacted) error still
		// drives requeue/backoff and is logged internally.
		return true, false, ctrl.Result{}, r.failErr(ctx, repo, redactWebhookURL(err, repo, url))
	}

	// Do not put the resolved URL in the status message when it came from a
	// urlSecretRef: that URL is deliberately secret (e.g. Kargo's hard-to-guess
	// receiver URL is held in a Secret precisely so it is not exposed), and status
	// is readable by anyone with get on the Repository. An inline url is already
	// in the spec, so echoing it is no additional disclosure.
	message := "repo_push webhook configured"
	if !usesWebhookSecretRef(repo) {
		message = fmt.Sprintf("repo_push webhook configured to %s", url)
	}
	changed := setWebhookCondition(&repo.Status.Conditions, metav1.ConditionTrue,
		ReasonWebhookConfigured, message, repo.Generation)
	logger.V(1).Info("reconciled webhook", "repository", ns+"/"+name)
	return false, changed, ctrl.Result{}, nil
}

// redactWebhookURL scrubs a secret-backed webhook URL out of an error before it
// is written into a status condition. When the Repository's webhook URL came from
// a urlSecretRef, any occurrence of url in err's message is replaced with
// "[redacted]" so a Quay error body that echoes the submitted config cannot leak
// the hard-to-guess receiver URL into status (which is broadly readable). For an
// inline URL — already present in the spec — the error is returned unchanged.
func redactWebhookURL(err error, repo *quayv1alpha1.Repository, url string) error {
	if err == nil || url == "" || !usesWebhookSecretRef(repo) {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, url) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(msg, url, "[redacted]"))
}

// isManagedWebhook reports whether a notification is one this controller owns:
// a repo_push webhook whose title is the controller's webhookTitle. Ownership is
// the gate for deletion so the reconciler only ever removes notifications it
// created — a manually-created repo_push webhook on the same repository (a
// different title) is left untouched.
func isManagedWebhook(n quay.Notification) bool {
	return n.Event == quay.EventRepoPush && n.Method == quay.MethodWebhook && n.Title == webhookTitle
}

// ensureWebhook makes the controller-managed repo_push webhook notifications
// converge on exactly one delivering to url: it lists the existing notifications,
// leaves a single correct controller-owned one untouched, and creates/replaces
// otherwise. Only controller-owned notifications (matched by webhookTitle) are
// deleted, so a re-run or URL change never piles up duplicates and never removes
// a manually-created webhook on the same repository.
func (r *RepositoryReconciler) ensureWebhook(ctx context.Context, qc RepoClient, ns, name, url string) error {
	notifications, err := qc.ListNotifications(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("listing Quay notifications for %s/%s: %w", ns, name, err)
	}

	matched := false
	for _, n := range notifications {
		if !isManagedWebhook(n) {
			// Not ours (e.g. a manually-created webhook): never touch it.
			continue
		}
		if !matched && n.Config.URL == url {
			// Keep exactly one correct controller-owned notification.
			matched = true
			continue
		}
		// Our notification with the wrong URL, or a duplicate of the one we keep:
		// delete it.
		if err := qc.DeleteNotificationIfExists(ctx, ns, name, n.UUID); err != nil {
			return fmt.Errorf("deleting stale Quay notification %s on %s/%s: %w", n.UUID, ns, name, err)
		}
	}

	if matched {
		return nil
	}
	if _, err := qc.CreateNotification(ctx, ns, name, url, webhookTitle); err != nil {
		return fmt.Errorf("creating Quay repo_push webhook on %s/%s: %w", ns, name, err)
	}
	return nil
}

// removeManagedWebhooks deletes every controller-owned repo_push webhook
// notification on the repository (matched by webhookTitle), leaving any
// manually-created webhooks intact. It is used when spec.webhook is unset.
func (r *RepositoryReconciler) removeManagedWebhooks(ctx context.Context, qc RepoClient, ns, name string) error {
	notifications, err := qc.ListNotifications(ctx, ns, name)
	if err != nil {
		// A repository that no longer exists has no notifications to clean up.
		if quay.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("listing Quay notifications for %s/%s: %w", ns, name, err)
	}
	for _, n := range notifications {
		if !isManagedWebhook(n) {
			continue
		}
		if err := qc.DeleteNotificationIfExists(ctx, ns, name, n.UUID); err != nil {
			return fmt.Errorf("deleting Quay notification %s on %s/%s: %w", n.UUID, ns, name, err)
		}
	}
	return nil
}

// succeed stamps Accepted/Programmed/Ready True (reason Reconciled) and writes
// status when something changed (a Ready condition flipped, observedGeneration
// advanced, or the WebhookConfigured condition changed), so a steady-state
// reconcile does not churn status. webhookChanged is load-bearing: a urlSecretRef
// value change updates Quay and flips WebhookConfigured without bumping the
// generation, and without it that status update would be silently skipped.
func (r *RepositoryReconciler) succeed(ctx context.Context, logger logr.Logger, repo *quayv1alpha1.Repository, webhookChanged bool) (ctrl.Result, error) {
	target := repo.Status.QuayRepository
	if target == "" {
		target = repo.Spec.OrganizationRef + "/" + repo.Spec.Name
	}
	message := fmt.Sprintf("reconciled Quay repository %s", target)
	changed := markReady(&repo.Status.Conditions, ReasonReconciled, message, repo.Generation)
	changed = changed || webhookChanged || repo.Status.ObservedGeneration != repo.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(repo, corev1.EventTypeNormal, ReasonReconciled, message)
	logger.Info("reconciled Repository", "repository", target)
	if err := r.updateStatus(ctx, repo); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileDelete runs the finalizer: it deletes the Quay repository (which
// removes its notifications) then drops the finalizer so the CR is removed.
//
// The repository identity is taken from status.QuayRepository — the resolved
// <org>/<repo> path recorded when the repo was provisioned. When status is empty
// (a crash, or a status-write failure, may have raced between the Quay create and
// the status persist, so an empty status does NOT prove the repo is absent) it
// falls back to re-resolving the identity from the immutable spec.name plus the
// Organization CR's spec.name. Because both inputs are immutable/stable, the
// fallback reconstructs exactly the same path, so the finalizer never leaks a
// repository a crash left behind. Only when neither status nor the Organization
// CR yields an identity (the org CR is gone, so the Quay path is unaddressable)
// does it drop the finalizer — there is nothing it can act on.
//
// A Quay error during delete fails the reconcile and requeues, so the finalizer
// is not removed until cleanup succeeds. A missing credential during delete also
// requeues rather than stranding the CR with the repository still in Quay.
func (r *RepositoryReconciler) reconcileDelete(ctx context.Context, repo *quayv1alpha1.Repository) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(repo, repositoryFinalizer) {
		return ctrl.Result{}, nil
	}

	ns, name, ok := splitQuayRepository(repo.Status.QuayRepository)
	if !ok {
		// Status did not record a provisioned path. Re-resolve from the immutable
		// spec + Organization CR so a crash between create and the status write
		// cannot orphan a real Quay repository.
		resolvedNs, resolvedName, resolveOK, err := r.resolveDeleteIdentity(ctx, repo)
		if err != nil {
			// A transient API error reading the Organization must NOT be mistaken
			// for an unresolvable identity — dropping the finalizer here would leak
			// the Quay repo. Requeue with backoff and retry.
			return ctrl.Result{}, err
		}
		if !resolveOK {
			// The Organization CR is genuinely gone: the Quay path is
			// unaddressable, so there is nothing to delete. Release the CR.
			return r.removeFinalizer(ctx, repo)
		}
		ns, name = resolvedNs, resolvedName
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, repo.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, repo, err)
	}

	qc := r.NewClient(cred)
	if err := qc.DeleteRepositoryIfExists(ctx, ns, name); err != nil {
		r.Recorder.Event(repo, corev1.EventTypeWarning, ReasonQuayError,
			fmt.Sprintf("deleting Quay repository %s/%s: %v", ns, name, err))
		return ctrl.Result{}, fmt.Errorf("deleting Quay repository %s/%s: %w", ns, name, err)
	}

	r.Recorder.Event(repo, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("deleted Quay repository %s/%s", ns, name))

	return r.removeFinalizer(ctx, repo)
}

// resolveDeleteIdentity reconstructs the Quay <org>/<repo> path for a Repository
// whose status.QuayRepository is empty, by resolving the Organization CR named by
// the (immutable) spec.organizationRef and combining its spec.name with the
// (immutable) spec.name.
//
// It distinguishes two non-resolution cases so the caller never leaks external
// state on a transient failure:
//   - ok=false, err=nil: the Organization CR is genuinely gone (NotFound) or
//     carries no spec.name, so the Quay path is permanently unaddressable and the
//     finalizer may be dropped.
//   - err!=nil: a transient API error reading the CR; the caller must requeue and
//     retry rather than assume there is nothing to delete.
func (r *RepositoryReconciler) resolveDeleteIdentity(ctx context.Context, repo *quayv1alpha1.Repository) (ns, name string, ok bool, err error) {
	org := &quayv1alpha1.Organization{}
	key := client.ObjectKey{Namespace: repo.Namespace, Name: repo.Spec.OrganizationRef}
	if getErr := r.Get(ctx, key, org); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("reading Organization %q during finalize: %w", repo.Spec.OrganizationRef, getErr)
	}
	if org.Spec.Name == "" || repo.Spec.Name == "" {
		return "", "", false, nil
	}
	return org.Spec.Name, repo.Spec.Name, true, nil
}

// removeFinalizer drops the repository finalizer and persists the change so the
// API server can delete the CR.
func (r *RepositoryReconciler) removeFinalizer(ctx context.Context, repo *quayv1alpha1.Repository) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(repo, repositoryFinalizer)
	if err := r.Update(ctx, repo); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// splitQuayRepository splits a recorded "org/repo" status.QuayRepository into its
// namespace and name. ok is false when the value is empty or malformed (no single
// slash), signaling there is no provisioned repository to act on.
func splitQuayRepository(qr string) (ns, name string, ok bool) {
	i := strings.IndexByte(qr, '/')
	if i <= 0 || i == len(qr)-1 || strings.IndexByte(qr[i+1:], '/') != -1 {
		return "", "", false
	}
	return qr[:i], qr[i+1:], true
}

// handleCredentialError maps a credential-resolution error to a reconcile
// result. A missing Secret/key sets a CredentialsNotFound condition (writing
// status + emitting a Warning only when changed) and requeues with the error.
// A transient API error reading the Secret requeues with backoff without
// stamping a misleading reason.
func (r *RepositoryReconciler) handleCredentialError(ctx context.Context, repo *quayv1alpha1.Repository, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&repo.Status.Conditions, ReasonCredentialsNotFound, err.Error(), repo.Generation); changed {
		r.Recorder.Event(repo, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, repo); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

// handleOrgNotReady records that the owning Organization (named by
// spec.organizationRef) is not resolvable/Ready and requeues. The Repository
// reconciler never creates the org (AC #9); it waits for the Organization
// reconciler. The status write and event fire only when the condition changed,
// and the returned error drives the requeue with backoff.
func (r *RepositoryReconciler) handleOrgNotReady(ctx context.Context, repo *quayv1alpha1.Repository, message string) (ctrl.Result, error) {
	if changed := markNotReady(&repo.Status.Conditions, ReasonOrganizationNotReady, message, repo.Generation); changed {
		r.Recorder.Event(repo, corev1.EventTypeWarning, ReasonOrganizationNotReady, message)
		if statusErr := r.updateStatus(ctx, repo); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, fmt.Errorf("%s", message)
}

// handleWebhookCondition sets the WebhookConfigured condition False with the
// given reason and message and writes status when it changed. requeue selects
// whether the reconcile retries: a recoverable WebhookURLNotFound requeues (so a
// later-created Secret takes effect) by returning an error; a terminal
// InvalidWebhook does not.
func (r *RepositoryReconciler) handleWebhookCondition(ctx context.Context, repo *quayv1alpha1.Repository, reason, message string, requeue bool) (ctrl.Result, error) {
	if changed := setWebhookCondition(&repo.Status.Conditions, metav1.ConditionFalse, reason, message, repo.Generation); changed {
		// Also reflect the not-ready state on Ready so the headline condition is
		// not stuck True from a prior pass while the webhook is unconfigurable.
		markNotReady(&repo.Status.Conditions, reason, message, repo.Generation)
		r.Recorder.Event(repo, corev1.EventTypeWarning, reason, message)
		if statusErr := r.updateStatus(ctx, repo); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	if requeue {
		return ctrl.Result{}, fmt.Errorf("%s", message)
	}
	return ctrl.Result{}, nil
}

// fail records a Quay error as a False condition + Warning event and returns the
// error so the request requeues with backoff. The status write and event are
// emitted only when the condition changed.
func (r *RepositoryReconciler) fail(ctx context.Context, repo *quayv1alpha1.Repository, err error) (ctrl.Result, error) {
	return ctrl.Result{}, r.failErr(ctx, repo, err)
}

// failErr is the error-returning core of fail: it stamps a QuayError condition
// (when changed) and returns err for the caller to propagate as a requeue.
func (r *RepositoryReconciler) failErr(ctx context.Context, repo *quayv1alpha1.Repository, err error) error {
	if changed := markNotReady(&repo.Status.Conditions, ReasonQuayError, err.Error(), repo.Generation); changed {
		r.Recorder.Event(repo, corev1.EventTypeWarning, ReasonQuayError, err.Error())
		if statusErr := r.updateStatus(ctx, repo); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after Quay error")
		}
	}
	return err
}

// updateStatus stamps observedGeneration and writes the status subresource,
// retrying on conflict by refetching and re-applying the computed status onto the
// fresh object. A NotFound (the CR was deleted concurrently) is ignored.
func (r *RepositoryReconciler) updateStatus(ctx context.Context, repo *quayv1alpha1.Repository) error {
	repo.Status.ObservedGeneration = repo.Generation
	desired := repo.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if updateErr := r.Status().Update(ctx, repo); updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(repo), repo); getErr != nil {
					return getErr
				}
				desired.DeepCopyInto(&repo.Status)
			}
			return updateErr
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating Repository status: %w", err)
	}
	return nil
}

// SetupWithManager wires the reconciler into the manager: it watches Repository
// resources, defaults the namespace and client factory if unset, and obtains an
// event recorder.
//
// It deliberately does not watch Secrets: the controller resolves only the
// specific credential/webhook-URL Secrets a CR names, via a non-caching get (the
// RBAC grants Secrets get, not list/watch), so a Secret informer is neither
// available nor desired. A urlSecretRef-backed Repository instead requeues on
// webhookSecretResyncInterval (set on the reconcile result) so a later change to
// the referenced Secret's value is eventually re-pushed to Quay (AC #8).
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("repository-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewQuayRepoClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&quayv1alpha1.Repository{}).
		Complete(r)
}

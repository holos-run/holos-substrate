package quay

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// organizationFinalizer guards Quay-side cleanup: while it is present, deleting
// the Organization CR runs the finalizer (which deletes the Quay org) before the
// CR is removed from the API server. Its value is the resource's qualified name
// so it is unambiguous among any other finalizers.
const organizationFinalizer = "organization.quay.holos.run/finalizer"

// ownerRobotShortname is the short name of the dedicated robot account the
// controller creates on every org it owns as a durable, server-side ownership
// marker (ADR-19, "Ownership and the claim model"). The robot's full username is
// "<org>+holos-owner"; its description carries the opaque ownership token. Keying
// create/adopt/delete on this server-side marker (not solely on the CR's
// status.created) closes the two races HOL-1311 deferred: a create whose
// status-write failed is no longer mis-released, and a delete that races a
// foreign recreate of the same global org name no longer destroys the recreated
// org.
const ownerRobotShortname = "holos-owner"

func statusCreated(org *quayv1alpha1.Organization) bool {
	return org.Status.Created != nil && *org.Status.Created
}

func setStatusCreated(org *quayv1alpha1.Organization, created bool) {
	org.Status.Created = k8sptr.To(created)
}

// ownerToken returns the opaque, controller-managed ownership token stamped into
// the marker robot's description for org. It is the Organization CR's UID: stable
// for the CR's lifetime, unique across CRs, and never reused, so a foreign org
// (or a same-named org recreated by another actor) cannot accidentally match it.
func ownerToken(org *quayv1alpha1.Organization) string {
	return string(org.UID)
}

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
	// UpdateOrganization applies mutable org fields (the contact email) to an
	// existing org. The reconciler calls it only on drift from the desired email.
	UpdateOrganization(ctx context.Context, name, email string) error
	// DeleteOrganizationIfExists deletes the org, treating an already-absent
	// response as success (idempotent).
	DeleteOrganizationIfExists(ctx context.Context, name string) error
	// GetOrganizationRobot reads the ownership-marker robot; a missing robot
	// returns an error for which quay.IsNotFound reports true.
	GetOrganizationRobot(ctx context.Context, org, shortname string) (*quay.OrganizationRobot, error)
	// CreateOrganizationRobot stamps the ownership-marker robot with the opaque
	// token. An already-present robot returns an error for which quay.IsConflict
	// reports true (Quay's create-robot endpoint is not idempotent).
	CreateOrganizationRobot(ctx context.Context, org, shortname, description string) error
	// DeleteOrganizationRobotIfExists deletes the ownership-marker robot,
	// treating an already-absent response as success (idempotent).
	DeleteOrganizationRobotIfExists(ctx context.Context, org, shortname string) error

	// ListTeams returns the org's teams keyed by name (derived from the org
	// payload's teams map), giving the reconciler each team's current role and
	// existence in a single request.
	ListTeams(ctx context.Context, org string) (map[string]quay.Team, error)
	// UpsertTeam creates or updates the org team with the given role and
	// description (PUT is create-or-update, so it is idempotent).
	UpsertTeam(ctx context.Context, org, team, role, description string) error
	// DeleteTeamIfExists deletes the org team, treating an already-absent team as
	// success (idempotent).
	DeleteTeamIfExists(ctx context.Context, org, team string) error
	// GetTeamMembers fetches the team-members payload, whose sync binding (the
	// bound OIDC group via quay.TeamMembers.GroupName) the reconciler reads to
	// detect drift and ownership.
	GetTeamMembers(ctx context.Context, org, team string) (*quay.TeamMembers, error)
	// EnableTeamSyncIfNotSynced binds the team's membership to oidcGroup, no-oping
	// when it is already bound to that group (idempotent).
	EnableTeamSyncIfNotSynced(ctx context.Context, org, team, oidcGroup string) error
	// DisableTeamSyncIfSynced removes the team's sync binding, treating an
	// already-unsynced team as success (idempotent).
	DisableTeamSyncIfSynced(ctx context.Context, org, team string) error

	// ListPrototypes returns the org's default-permission prototypes so the
	// reconciler can find the one delegating to a given team.
	ListPrototypes(ctx context.Context, org string) ([]quay.Prototype, error)
	// CreatePrototype creates an org default-permission prototype delegating role
	// to delegateTeam, returning the created prototype (with its assigned id).
	CreatePrototype(ctx context.Context, org, role, delegateTeam string) (*quay.Prototype, error)
	// UpdatePrototype sets the role on an existing prototype (the delegate is fixed
	// at creation).
	UpdatePrototype(ctx context.Context, org, prototypeID, role string) error
	// DeletePrototypeIfExists deletes the prototype, treating an already-absent
	// prototype as success (idempotent).
	DeletePrototypeIfExists(ctx context.Context, org, prototypeID string) error
}

// ClientFactory builds an OrgClient from a resolved Quay credential and the
// CA bundle the Organization spec carries. The default factory (NewQuayClient)
// returns a real *quay.Client trusting that bundle; tests substitute a factory
// that returns a fake, which is how the reconciler is exercised without a live
// Quay or HTTP. The caBundle comes from the spec (not the credential Secret), so
// it is a separate argument and is never folded into quayCredential.
type ClientFactory func(cred *quayCredential, caBundle []byte) OrgClient

// NewQuayClient is the production ClientFactory: it builds a real internal/quay
// client from the credential's url and token, trusting the PEM-encoded caBundle
// (the in-cluster Quay registry's local-CA chain) in addition to the system
// store. An empty caBundle yields system trust only — unchanged behavior. A
// caBundle that contains no parseable certificate falls back to a system-trust
// client so a misconfigured bundle surfaces as the original x509 trust error on
// the Quay call rather than a silent nil client; the parse failure is logged.
func NewQuayClient(cred *quayCredential, caBundle []byte) OrgClient {
	c, err := quay.NewClientWithCABundle(cred.url, cred.token, caBundle)
	if err != nil {
		log.Log.Error(err, "building Quay client with caBundle; falling back to system trust")
		return quay.NewClient(cred.url, cred.token, nil)
	}
	return c
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
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Record the reconcile outcome (success/error) per kind for the custom
	// holos_controller_reconcile_total metric (AC #4), alongside
	// controller-runtime's built-in reconcile metrics. retErr is the named
	// return, so the deferred record observes whatever error the reconcile
	// ultimately returns regardless of which path produced it.
	defer func() { recordReconcile(kindOrganization, retErr) }()

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
		// fresh object rather than continuing with a stale one. RequeueAfter a
		// negligible delay (not the deprecated Result.Requeue) re-enqueues
		// promptly without staticcheck SA1019.
		return ctrl.Result{RequeueAfter: requeueImmediately}, nil
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

	// Reject an invalid spec.caBundle up front so a malformed bundle surfaces as
	// a failed reconcile (Ready=False) rather than silently falling back to
	// system trust and possibly reporting Ready=True for an unhonored spec.
	if err := quay.ValidateCABundle(org.Spec.CABundle); err != nil {
		return r.fail(ctx, org, err)
	}

	qc := r.NewClient(cred, org.Spec.CABundle)

	// ADR-19 claim model. Quay orgs are a single global namespace and the
	// controller credential carries FEATURE_SUPERUSERS_FULL_ACCESS, so a naive
	// "adopt any existing org" rule would let a namespaced CR seize another
	// tenant's org. Ownership is recorded by a durable, server-side marker — a
	// dedicated "<org>+holos-owner" robot whose description is this CR's UID
	// (ownerToken) — backed by the CR's status.Created. The marker is the
	// authority for the delete decision (so a delete that races a foreign
	// recreate cannot destroy the recreated org) and heals status.Created when a
	// prior create's status-write was lost.
	//
	// GET the org and branch on the claim-model cases. A NotFound is an expected
	// branch (create path), not a Quay-API failure, so it records as success.
	actual, getErr := qc.GetOrganization(ctx, org.Spec.Name)
	recordQuayAPI(opGetOrganization, ignoreNotFound(getErr))
	switch {
	case quay.IsNotFound(getErr):
		// Org does not appear to exist → try to create it. Use CreateOrganization
		// (not the IfNotExists wrapper) so a conflict is observable: if another
		// actor created the org between the GET and this POST, a swallowed
		// conflict would let us falsely stamp ownership and later delete an org
		// we did not create. On conflict, fall through to the exists-path claim
		// decision (adopt or Conflict) rather than claiming ownership.
		err := qc.CreateOrganization(ctx, org.Spec.Name, org.Spec.Email)
		// A conflict is an expected claim-model branch (the org was created by a
		// racing actor), not a Quay-API failure, so record only non-conflict
		// errors as failures.
		recordQuayAPI(opCreateOrganization, ignoreConflict(err))
		switch {
		case err == nil:
			// Clean create: this CR is the owner. Record ownership in
			// status.Created *before* stamping the server-side marker, so that a
			// marker-stamp failure cannot strand the just-created org: the next
			// reconcile sees an existing org with status.Created=true and no marker
			// and takes the heal path (re-stamp), rather than mistaking its own
			// org for a foreign one and releasing it (Codex round 1).
			setStatusCreated(org, true)
			if markerErr := r.ensureOwnerMarker(ctx, qc, org); markerErr != nil {
				// fail() persists status (carrying Created=true) and requeues, so
				// the heal path recovers the marker on the next reconcile.
				return r.fail(ctx, org, markerErr)
			}
			return r.reconcileTeamsThenSucceed(ctx, logger, qc, org, true, ReasonCreated,
				fmt.Sprintf("created Quay organization %q", org.Spec.Name))
		case quay.IsConflict(err):
			// Lost the create race: the org now exists but this CR did not
			// create it. Re-evaluate it against the claim model (owned via the
			// marker, adopt, or Conflict).
			return r.reconcileExisting(ctx, logger, qc, org, nil)
		default:
			return r.fail(ctx, org, fmt.Errorf("creating Quay organization %q: %w", org.Spec.Name, err))
		}

	case getErr == nil:
		// Org exists → resolve ownership from the durable marker and branch.
		return r.reconcileExisting(ctx, logger, qc, org, actual)

	default:
		// Any other Quay error (auth, server error): fail and requeue.
		return r.fail(ctx, org, fmt.Errorf("getting Quay organization %q: %w", org.Spec.Name, getErr))
	}
}

// reconcileExisting handles a Quay org that already exists. It resolves
// ownership from the durable server-side marker (the holos-owner robot) and
// branches per the ADR-19 claim model:
//
//   - marker present and matching this CR's token → owned: heal status.Created,
//     apply mutable spec drift, mark Ready (Created).
//   - marker absent but status.Created already true → this CR created the org and
//     the marker write was previously lost; re-stamp it, then treat as owned.
//   - marker absent and not recorded as created → unowned: adopt (spec.adopt) or
//     Conflict.
//   - marker present but holding a different token → owned by another actor:
//     Conflict, never seized — even with spec.adopt.
//
// actual is the GET-org result used for email-drift detection; it may be nil
// (e.g. on the create-race path), in which case the org is re-fetched only if a
// drift comparison is needed.
func (r *OrganizationReconciler) reconcileExisting(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization, actual *quay.Organization) (ctrl.Result, error) {
	robot, markerErr := qc.GetOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opGetOrganizationRobot, ignoreNotFound(markerErr))
	switch {
	case markerErr == nil:
		// A marker exists. Owned only when its token matches this CR.
		if robot.Description == ownerToken(org) {
			return r.reconcileOwned(ctx, logger, qc, org, actual)
		}
		// Marker holds a foreign token → another owner. Never seize, even with
		// spec.adopt: an actively-marked org belongs to a different claim.
		return r.conflict(ctx, logger, org)

	case quay.IsNotFound(markerErr):
		// No marker. If this CR is already recorded as the creator, the marker
		// write was lost after a successful create (HOL-1311 race a) — re-stamp
		// it and continue as owned rather than mis-releasing the org.
		if statusCreated(org) {
			if err := r.ensureOwnerMarker(ctx, qc, org); err != nil {
				return r.fail(ctx, org, err)
			}
			return r.reconcileOwned(ctx, logger, qc, org, actual, true)
		}
		// Unowned, externally-created org → adopt (if opted in) or Conflict.
		return r.reconcileExistingUnowned(ctx, logger, qc, org)

	default:
		// Any other Quay error reading the marker: fail and requeue.
		return r.fail(ctx, org, fmt.Errorf("reading ownership marker for Quay organization %q: %w", org.Spec.Name, markerErr))
	}
}

// reconcileOwned reconciles an org this CR owns (the marker matches): it heals
// status.Created, pushes any mutable spec drift (the contact email) to Quay, then
// marks Ready. It never errors on an org we own beyond a genuine Quay-API failure.
func (r *OrganizationReconciler) reconcileOwned(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization, actual *quay.Organization, markerMutated ...bool) (ctrl.Result, error) {
	mutated := len(markerMutated) > 0 && markerMutated[0]
	setStatusCreated(org, true)

	// Apply mutable spec drift before marking Ready. Quay 3.17.3 organizations
	// expose only the contact email as a mutable, programmable field on this
	// path.
	if actual == nil {
		fetched, err := qc.GetOrganization(ctx, org.Spec.Name)
		recordQuayAPI(opGetOrganization, err)
		if err != nil {
			return r.fail(ctx, org, fmt.Errorf("getting Quay organization %q: %w", org.Spec.Name, err))
		}
		actual = fetched
	}
	if org.Spec.Email != "" && actual.Email != org.Spec.Email {
		updateErr := qc.UpdateOrganization(ctx, org.Spec.Name, org.Spec.Email)
		recordQuayAPI(opUpdateOrganization, updateErr)
		if updateErr != nil {
			return r.fail(ctx, org, fmt.Errorf("updating Quay organization %q: %w", org.Spec.Name, updateErr))
		}
		mutated = true
		logger.Info("applied Organization email drift", "name", org.Spec.Name)
	}

	return r.reconcileTeamsThenSucceed(ctx, logger, qc, org, mutated, ReasonCreated,
		fmt.Sprintf("reconciled Quay organization %q", org.Spec.Name))
}

// ensureOwnerMarker stamps the durable ownership marker (the holos-owner robot)
// with this CR's token. An already-present marker is tolerated: because Quay's
// create-robot endpoint cannot update an existing robot's description, the
// reconciler verifies-on-conflict that the existing marker already holds this
// CR's token and treats a match as success; a conflicting marker that holds a
// foreign token is a genuine error so the reconcile does not falsely claim
// ownership.
func (r *OrganizationReconciler) ensureOwnerMarker(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) error {
	err := qc.CreateOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname, ownerToken(org))
	recordQuayAPI(opCreateOrganizationRobot, ignoreConflict(err))
	if err == nil {
		return nil
	}
	if !quay.IsConflict(err) {
		return fmt.Errorf("stamping ownership marker on Quay organization %q: %w", org.Spec.Name, err)
	}
	// The marker robot already exists. Confirm it holds this CR's token; if so the
	// marker is already correct (idempotent), otherwise ownership is contested.
	robot, getErr := qc.GetOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opGetOrganizationRobot, getErr)
	if getErr != nil {
		return fmt.Errorf("verifying ownership marker on Quay organization %q: %w", org.Spec.Name, getErr)
	}
	if robot.Description != ownerToken(org) {
		return fmt.Errorf("ownership marker on Quay organization %q holds a foreign token; refusing to claim", org.Spec.Name)
	}
	return nil
}

// conflict records a terminal Conflict condition for an org owned by another
// actor (the marker holds a foreign token) and emits a Warning, writing status
// only on a change so an already-recorded conflict does not spin a watch loop.
func (r *OrganizationReconciler) conflict(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	message := fmt.Sprintf("Quay organization %q is owned by another resource (ownership marker mismatch); refusing to claim it", org.Spec.Name)
	changed := setConflict(&org.Status.Conditions, message, org.Generation)
	changed = changed || org.Status.ObservedGeneration != org.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(org, corev1.EventTypeWarning, ReasonConflict, message)
	logger.Info("Organization conflict", "name", org.Spec.Name)
	if err := r.updateStatus(ctx, org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileExistingUnowned handles a Quay org that exists but was not created by
// this CR: adopt it when spec.adopt is set (status.Created stays false so the
// finalizer releases rather than deletes), otherwise refuse to write and set a
// terminal Conflict condition (no requeue storm — a spec change re-triggers).
func (r *OrganizationReconciler) reconcileExistingUnowned(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	if org.Spec.Adopt {
		setStatusCreated(org, false)
		return r.reconcileTeamsThenSucceed(ctx, logger, qc, org, false, ReasonAdopted,
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

// reconcileTeamsThenSucceed reconciles spec.syncedTeams into the now-owned (or
// adopted) Quay org, then marks the resource Ready. It is the single funnel every
// success path runs so teams are reconciled only after the org itself is
// provisioned (AC #1). A team conflict (a spec team pre-exists and was not created
// by this resource) is surfaced as a TeamConflict condition (a non-Ready outcome)
// rather than a generic Quay error; any other Quay error fails the reconcile and
// requeues. The status write that succeed()/teamConflict() performs also persists
// the status.managedTeams set reconcileSyncedTeams computed.
func (r *OrganizationReconciler) reconcileTeamsThenSucceed(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization, mutated bool, reason, message string) (ctrl.Result, error) {
	// Snapshot the managed-team set so a change to status.managedTeams (e.g. a team
	// added, removed, or healed in) is persisted even when Ready and
	// observedGeneration are unchanged — without it, succeed() would skip the write
	// on a steady-state reconcile and lose the updated ownership record.
	before := append([]string(nil), org.Status.ManagedTeams...)
	teamMutated, err := r.reconcileSyncedTeams(ctx, qc, org)
	mutated = mutated || teamMutated
	// reconcileSyncedTeams may rewrite status.managedTeams even when it returns a
	// (conflict) error — it persists the progress made before the conflict — so
	// compute the change for both the success and the conflict status write.
	managedChanged := !equalStrings(before, org.Status.ManagedTeams)
	if err != nil {
		if isTeamConflict(err) {
			return r.teamConflict(ctx, logger, org, err.Error(), managedChanged)
		}
		return r.fail(ctx, org, err)
	}
	if mutated {
		r.stampMutation(org)
	}
	now := metav1.Now()
	org.Status.LastValidatedTime = &now
	return r.succeed(ctx, logger, org, reason, message, managedChanged)
}

// equalStrings reports whether two string slices are element-wise equal. Both
// managed-team slices are kept sorted (writeManagedTeams), so a positional
// comparison is sufficient to detect a change.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// teamConflict records a TeamConflict condition (Ready/Programmed False) for a
// spec.syncedTeams entry that names a pre-existing Quay team this resource did not
// create. It writes status and emits a Warning only when something actually
// changed — the condition flipped, observedGeneration advanced, or the
// managed-team set changed (managedChanged) — so a persistent, unchanged team
// conflict does not rewrite identical status on every reconcile (which would
// re-enqueue the object and spin a watch-triggered loop), mirroring the org-level
// conflict path. managedChanged keeps the status.managedTeams progress made before
// the conflict durable even when the condition itself is unchanged.
func (r *OrganizationReconciler) teamConflict(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization, message string, managedChanged bool) (ctrl.Result, error) {
	changed := setTeamConflict(&org.Status.Conditions, message, org.Generation)
	changed = changed || managedChanged || org.Status.ObservedGeneration != org.Generation
	if !changed {
		return ctrl.Result{}, nil
	}
	r.Recorder.Event(org, corev1.EventTypeWarning, ReasonTeamConflict, message)
	logger.Info("Organization team conflict", "name", org.Spec.Name)
	if err := r.updateStatus(ctx, org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// succeed stamps Ready/Programmed/Accepted true with the given reason+message.
// It emits a Normal event and writes status only when something actually changed
// (a condition flipped, observedGeneration advanced, or extraChanged is set), so a
// steady-state reconcile of an unchanged resource does not write status — which
// would otherwise re-enqueue the object and spin a reconcile/update/event loop.
// extraChanged is load-bearing for status.managedTeams: a team set that changes
// without flipping Ready or bumping the generation must still be persisted.
func (r *OrganizationReconciler) succeed(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization, reason, message string, extraChanged bool) (ctrl.Result, error) {
	validationChanged := true
	changed := markReady(&org.Status.Conditions, reason, message, org.Generation)
	changed = changed || extraChanged || validationChanged || org.Status.ObservedGeneration != org.Generation
	if !changed {
		return ctrl.Result{RequeueAfter: quayExternalResourceResync}, nil
	}
	r.Recorder.Event(org, corev1.EventTypeNormal, reason, message)
	logger.Info("reconciled Organization", "name", org.Spec.Name, "reason", reason)
	if err := r.updateStatus(ctx, org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: quayExternalResourceResync}, nil
}

func (r *OrganizationReconciler) stampMutation(org *quayv1alpha1.Organization) {
	now := metav1.Now()
	reason := quayv1alpha1.MutationReasonDriftRemediation
	if org.Status.ObservedGeneration != org.Generation || !organizationReady(org) {
		reason = quayv1alpha1.MutationReasonSpecChange
	}
	org.Status.LastMutatedTime = &now
	org.Status.LastMutationReason = reason
	if reason == quayv1alpha1.MutationReasonDriftRemediation {
		org.Status.LastDriftTime = &now
	}
}

func organizationReady(org *quayv1alpha1.Organization) bool {
	for _, c := range org.Status.Conditions {
		if c.Type == ConditionReady {
			return c.Status == metav1.ConditionTrue && c.ObservedGeneration == org.Generation
		}
	}
	return false
}

// reconcileDelete runs the finalizer. Per ADR-19's claim model the Quay org is
// deleted only when this CR created it (status.Created); an adopted org is
// released (the finalizer drops without deleting), so removing a CR that merely
// claimed a pre-existing org never destroys it. After cleanup the finalizer is
// removed so the CR is deleted. A Quay error during delete fails the reconcile
// and requeues, so the finalizer is not removed until cleanup succeeds.
//
// Synced teams need no separate finalizer (AC #7): deleting the Quay org cascades
// its teams (and their default-permission prototypes), so the existing org delete
// is sufficient cleanup for a created org. Adopted-org edge case: when the org is
// released rather than deleted (status.Created false), the synced teams this
// controller created inside it are intentionally NOT individually deleted — the
// platform did not create the org and non-destructive release is the contract
// (mirroring the org-level claim model: an adopted org is never mutated on
// release), so the teams remain on the surviving org. This is a deliberate
// tradeoff mandated by AC #7: it can leave controller-created teams (OIDC-synced,
// possibly with default repository permissions) behind on an adopted org, i.e.
// stale access grants an operator must clean up out of band. The alternative —
// deprovisioning status.managedTeams before releasing — would mutate an org the
// platform does not own, which AC #7 deliberately forbids. (Dropping a team from
// spec.syncedTeams while the CR lives still de-provisions that team via
// reconcileSyncedTeams; this note is only about the whole-CR delete of an adopted
// org.)
func (r *OrganizationReconciler) reconcileDelete(ctx context.Context, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(org, organizationFinalizer) {
		// Already finalized; nothing to do.
		return ctrl.Result{}, nil
	}

	// Adopted org (or one never created by this CR) → release, do not delete.
	// No credential is needed: the controller does not touch Quay, it only
	// relinquishes its claim.
	if !statusCreated(org) {
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

	// A malformed spec.caBundle must not be silently ignored on the delete path
	// either: fail (requeue) rather than build a system-trust client that does
	// not honor the spec, which keeps the finalizer in place until the bundle is
	// corrected.
	if err := quay.ValidateCABundle(org.Spec.CABundle); err != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, ReasonQuayError, err.Error())
		return ctrl.Result{}, err
	}

	qc := r.NewClient(cred, org.Spec.CABundle)

	// Verify the durable server-side marker still names this CR before deleting.
	// This closes HOL-1311 race (b): if a prior delete succeeded but finalizer
	// removal failed, and another actor recreated the same global org name in the
	// gap, status.Created is still true but the recreated org carries no (or a
	// foreign) marker — deleting it would destroy a stranger's org. When the
	// marker no longer matches, release instead of delete.
	owns, err := r.ownsViaMarker(ctx, qc, org)
	if err != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, ReasonQuayError,
			fmt.Sprintf("verifying ownership marker for Quay organization %q: %v", org.Spec.Name, err))
		return ctrl.Result{}, fmt.Errorf("verifying ownership marker for Quay organization %q: %w", org.Spec.Name, err)
	}
	if !owns {
		r.Recorder.Event(org, corev1.EventTypeNormal, ReasonReleased,
			fmt.Sprintf("released Quay organization %q without deleting (ownership marker absent or foreign; org was recreated by another actor)", org.Spec.Name))
		return r.removeFinalizer(ctx, org)
	}

	// Delete the org BEFORE the marker. Ordering matters for retry safety
	// (Codex round 1): if the marker were removed first and the org delete then
	// failed, the next retry would see no marker, classify the org as no longer
	// ours, and release the finalizer — leaking the platform-created org. By
	// deleting the org first, a failed org delete leaves the marker intact so the
	// retry still verifies ownership and re-attempts the delete. Deleting the org
	// also removes its robots, so the explicit marker cleanup below is a
	// best-effort tidy-up of an already-gone robot.
	delErr := qc.DeleteOrganizationIfExists(ctx, org.Spec.Name)
	recordQuayAPI(opDeleteOrganization, delErr)
	if err := delErr; err != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, ReasonQuayError,
			fmt.Sprintf("deleting Quay organization %q: %v", org.Spec.Name, err))
		return ctrl.Result{}, fmt.Errorf("deleting Quay organization %q: %w", org.Spec.Name, err)
	}

	// The marker robot is removed with the org; this idempotent delete tolerates
	// its absence and only tidies a stranded marker (e.g. if the org was already
	// gone). A failure here must not block finalizer removal — the org is deleted.
	markerDelErr := qc.DeleteOrganizationRobotIfExists(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opDeleteOrganizationRobot, markerDelErr)
	if markerDelErr != nil {
		log.FromContext(ctx).Error(markerDelErr, "tidying ownership marker after org delete", "name", org.Spec.Name)
	}

	r.Recorder.Event(org, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("deleted Quay organization %q", org.Spec.Name))

	return r.removeFinalizer(ctx, org)
}

// ownsViaMarker reports whether the durable server-side ownership marker on the
// Quay org still names this CR (its UID). A NotFound marker means the org carries
// no marker — it is not the org this CR created (either never marked or recreated
// by another actor after a prior delete), so it must not be deleted. A non-marker
// Quay error is returned so the caller requeues rather than guessing.
func (r *OrganizationReconciler) ownsViaMarker(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) (bool, error) {
	robot, err := qc.GetOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opGetOrganizationRobot, ignoreNotFound(err))
	if quay.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return robot.Description == ownerToken(org), nil
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
	org.Status.ManagedTeams = normalizeManagedTeams(org.Status.ManagedTeams)
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
		For(&quayv1alpha1.Organization{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

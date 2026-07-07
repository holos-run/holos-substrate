package quay

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	ctrlshared "github.com/holos-run/holos-paas/internal/controller/shared"
	"github.com/holos-run/holos-paas/internal/quay"
)

// organizationFinalizer guards Quay-side cleanup: while it is present, deleting
// the Organization CR runs the finalizer (which deletes the Quay org) before the
// CR is removed from the API server. Its value is the resource's qualified name
// so it is unambiguous among any other finalizers.
const organizationFinalizer = "organization.quay.holos.run/finalizer"

// ownerRobotShortname is the short name of the dedicated robot account the
// controller creates on every org it owns as a durable, server-side ownership
// marker. The robot's full username is "<org>+holos-owner"; its description
// carries the opaque ownership token. Keying create/adopt/delete on this
// server-side marker (not solely on the CR's status.created) prevents a create
// whose status write failed from being mis-released and prevents a delete racing a
// foreign recreate of the same global org name from destroying the recreated org.
const ownerRobotShortname = "holos-owner"

const (
	organizationOwnerKindCreated = "created"
	organizationOwnerKindAdopted = "adopted"
)

func statusCreated(org *quayv1alpha1.Organization) bool {
	return org.Status.Created != nil && *org.Status.Created
}

func setStatusCreated(org *quayv1alpha1.Organization, created bool) {
	org.Status.Created = k8sptr.To(created)
}

// ownerToken returns the opaque, controller-managed ownership token used by
// organization child markers. It is the Organization CR's UID: stable for the
// CR's lifetime, unique across CRs, and never reused, so a foreign org (or a
// same-named org recreated by another actor) cannot accidentally match it.
func ownerToken(org *quayv1alpha1.Organization) string {
	return string(org.UID)
}

// organizationOwnerToken returns the marker robot description recording which CR
// owns the org and whether ownership came from create or adopt.
func organizationOwnerToken(org *quayv1alpha1.Organization, created bool) string {
	kind := organizationOwnerKindAdopted
	if created {
		kind = organizationOwnerKindCreated
	}
	return kind + ":" + ownerToken(org)
}

func parseOrganizationOwnerToken(description string) (uid string, created bool) {
	if rest, ok := strings.CutPrefix(description, organizationOwnerKindAdopted+":"); ok {
		return rest, false
	}
	if rest, ok := strings.CutPrefix(description, organizationOwnerKindCreated+":"); ok {
		return rest, true
	}
	return description, true
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
	// transitions.
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
// Ready with observedGeneration → Status().Patch. Credential and Quay errors map
// to a False condition with an actionable reason and a Warning event, and return
// an error so the request requeues with backoff.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	// Record the reconcile outcome (success/error) per kind for the custom
	// holos_controller_reconcile_total metric, alongside controller-runtime's
	// built-in reconcile metrics. retErr is the named return, so the deferred record
	// observes whatever error the reconcile ultimately returns regardless of which
	// path produced it.
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
		res, failErr := r.fail(ctx, org, err, false)
		if failErr != nil {
			return res, ctrlreconcile.TerminalError(failErr)
		}
		return res, nil
	}

	qc := r.NewClient(cred, org.Spec.CABundle)

	// Claim model. Quay orgs are a single global namespace and the controller
	// credential carries FEATURE_SUPERUSERS_FULL_ACCESS, so a naive "adopt any
	// existing org" rule would let a namespaced CR seize another tenant's org.
	// Ownership is recorded by a durable, server-side marker — a dedicated
	// "<org>+holos-owner" robot whose description is "created:<uid>" or
	// "adopted:<uid>" — backed by the CR's status.Created. The marker is the
	// authority for the delete decision (so a delete that races a foreign recreate
	// cannot destroy the recreated org) and heals status.Created when a prior
	// status-write was lost. Legacy bare-UID marker descriptions are accepted as
	// created markers and healed to the prefixed form.
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
			// org for a foreign one and releasing it.
			setStatusCreated(org, true)
			if _, markerErr := r.ensureOwnerMarker(ctx, qc, org, organizationOwnerToken(org, true)); markerErr != nil {
				r.stampMutation(org, false)
				// fail() persists status (carrying Created=true) and requeues, so
				// the heal path recovers the marker on the next reconcile.
				return r.fail(ctx, org, markerErr, true)
			}
			return r.reconcileTeamsThenSucceed(ctx, logger, qc, org, quayMutation{Mutated: true}, quayv1alpha1.ReasonCreated,
				fmt.Sprintf("created Quay organization %q", org.Spec.Name))
		case quay.IsConflict(err):
			// Lost the create race: the org now exists but this CR did not
			// create it. Re-evaluate it against the claim model (owned via the
			// marker, adopt, or Conflict).
			return r.reconcileExisting(ctx, logger, qc, org, nil)
		default:
			return r.fail(ctx, org, fmt.Errorf("creating Quay organization %q: %w", org.Spec.Name, err), false)
		}

	case getErr == nil:
		// Org exists → resolve ownership from the durable marker and branch.
		return r.reconcileExisting(ctx, logger, qc, org, actual)

	default:
		// Any other Quay error (auth, server error): fail and requeue.
		return r.fail(ctx, org, fmt.Errorf("getting Quay organization %q: %w", org.Spec.Name, getErr), false)
	}
}

// reconcileExisting handles a Quay org that already exists. It resolves
// ownership from the durable server-side marker (the holos-owner robot) and
// branches per the claim model:
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
		uid, created := parseOrganizationOwnerToken(robot.Description)
		if uid == ownerToken(org) {
			markerMutated := false
			desired := organizationOwnerToken(org, created)
			if robot.Description != desired {
				mutated, err := r.replaceOwnerMarker(ctx, qc, org, robot.Description, desired)
				if err != nil {
					if mutated {
						r.stampMutation(org, false)
					}
					return r.fail(ctx, org, err, mutated)
				}
				markerMutated = mutated
			}
			return r.reconcileOwned(ctx, logger, qc, org, actual, created, markerMutated)
		}
		// Marker holds a foreign token → another owner. Never seize, even with
		// spec.adopt: an actively-marked org belongs to a different claim.
		return r.conflict(ctx, logger, org)

	case quay.IsNotFound(markerErr):
		// No marker. If this CR is already recorded as the creator, the marker
		// write was lost after a successful create — re-stamp
		// it and continue as owned rather than mis-releasing the org.
		if statusCreated(org) {
			markerMutated, err := r.ensureOwnerMarker(ctx, qc, org, organizationOwnerToken(org, true))
			if err != nil {
				return r.fail(ctx, org, err, false)
			}
			return r.reconcileOwned(ctx, logger, qc, org, actual, true, markerMutated)
		}
		// Unowned, externally-created org → adopt (if opted in) or Conflict.
		return r.reconcileExistingUnowned(ctx, logger, qc, org)

	default:
		// Any other Quay error reading the marker: fail and requeue.
		return r.fail(ctx, org, fmt.Errorf("reading ownership marker for Quay organization %q: %w", org.Spec.Name, markerErr), false)
	}
}

// reconcileOwned reconciles an org this CR owns (the marker matches): it heals
// status.Created, pushes any mutable spec drift (the contact email) to Quay, then
// marks Ready. It never errors on an org we own beyond a genuine Quay-API failure.
func (r *OrganizationReconciler) reconcileOwned(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization, actual *quay.Organization, created, markerMutated bool) (ctrl.Result, error) {
	mutation := quayMutation{Mutated: markerMutated}
	canMarkDrift := organizationReady(org)
	setStatusCreated(org, created)

	// Apply mutable spec drift before marking Ready. Quay 3.17.3 organizations
	// expose only the contact email as a mutable, programmable field on this
	// path.
	if actual == nil {
		fetched, err := qc.GetOrganization(ctx, org.Spec.Name)
		recordQuayAPI(opGetOrganization, err)
		if err != nil {
			if mutation.Mutated {
				r.stampMutation(org, mutation.HealedDrift)
			}
			return r.fail(ctx, org, fmt.Errorf("getting Quay organization %q: %w", org.Spec.Name, err), mutation.Mutated)
		}
		actual = fetched
	}
	if org.Spec.Email != "" && actual.Email != org.Spec.Email {
		updateErr := qc.UpdateOrganization(ctx, org.Spec.Name, org.Spec.Email)
		recordQuayAPI(opUpdateOrganization, updateErr)
		if updateErr != nil {
			if mutation.Mutated {
				r.stampMutation(org, mutation.HealedDrift)
			}
			return r.fail(ctx, org, fmt.Errorf("updating Quay organization %q: %w", org.Spec.Name, updateErr), mutation.Mutated)
		}
		mutation = mutation.or(quayMutation{Mutated: true, HealedDrift: canMarkDrift})
		logger.Info("applied Organization email drift", "name", org.Spec.Name)
	}

	return r.reconcileTeamsThenSucceed(ctx, logger, qc, org, mutation, quayv1alpha1.ReasonReconciled,
		fmt.Sprintf("reconciled Quay organization %q", org.Spec.Name))
}

// ensureOwnerMarker stamps the durable ownership marker (the holos-owner robot)
// with this CR's token. An already-present marker is tolerated: because Quay's
// create-robot endpoint cannot update an existing robot's description, the
// reconciler verifies-on-conflict that the existing marker already holds this
// CR's token and treats a match as success; a conflicting marker that holds a
// foreign token is a genuine error so the reconcile does not falsely claim
// ownership.
func (r *OrganizationReconciler) ensureOwnerMarker(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization, token string) (bool, error) {
	err := qc.CreateOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname, token)
	recordQuayAPI(opCreateOrganizationRobot, ignoreConflict(err))
	if err == nil {
		return true, nil
	}
	if !quay.IsConflict(err) {
		return false, fmt.Errorf("stamping ownership marker on Quay organization %q: %w", org.Spec.Name, err)
	}
	// The marker robot already exists. Confirm it holds this CR's token; if so the
	// marker is already correct (idempotent), otherwise ownership is contested.
	robot, getErr := qc.GetOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opGetOrganizationRobot, getErr)
	if getErr != nil {
		return false, fmt.Errorf("verifying ownership marker on Quay organization %q: %w", org.Spec.Name, getErr)
	}
	if robot.Description == token {
		return false, nil
	}
	uid, _ := parseOrganizationOwnerToken(robot.Description)
	if uid != ownerToken(org) {
		return false, fmt.Errorf("ownership marker on Quay organization %q holds a foreign token; refusing to claim", org.Spec.Name)
	}
	return r.replaceOwnerMarker(ctx, qc, org, robot.Description, token)
}

func (r *OrganizationReconciler) replaceOwnerMarker(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization, oldToken, newToken string) (bool, error) {
	delErr := qc.DeleteOrganizationRobotIfExists(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opDeleteOrganizationRobot, delErr)
	if delErr != nil {
		return false, fmt.Errorf("replacing ownership marker on Quay organization %q: deleting old marker: %w", org.Spec.Name, delErr)
	}
	createErr := qc.CreateOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname, newToken)
	recordQuayAPI(opCreateOrganizationRobot, ignoreConflict(createErr))
	if createErr != nil {
		restoreErr := qc.CreateOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname, oldToken)
		recordQuayAPI(opCreateOrganizationRobot, ignoreConflict(restoreErr))
		if restoreErr != nil {
			return true, fmt.Errorf("replacing ownership marker on Quay organization %q: creating new marker: %w; restoring old marker: %v", org.Spec.Name, createErr, restoreErr)
		}
		return true, fmt.Errorf("replacing ownership marker on Quay organization %q: creating new marker: %w", org.Spec.Name, createErr)
	}
	return true, nil
}

// conflict records a terminal Conflict condition for an org owned by another
// actor (the marker holds a foreign token) and emits a Warning, writing status
// only on a change so an already-recorded conflict does not spin a watch loop.
func (r *OrganizationReconciler) conflict(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	message := fmt.Sprintf("Quay organization %q is owned by another resource (ownership marker mismatch); refusing to claim it", org.Spec.Name)
	return r.recordConflict(ctx, logger, org, quayv1alpha1.ReasonConflict, message, false)
}

// reconcileExistingUnowned handles a Quay org that exists but was not created by
// this CR: adopt it when spec.adopt is set (status.Created stays false so the
// finalizer releases rather than deletes), otherwise refuse to write and set a
// terminal Conflict condition (no requeue storm — a spec change re-triggers).
func (r *OrganizationReconciler) reconcileExistingUnowned(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	if org.Spec.Adopt {
		reason := quayv1alpha1.ReasonReconciled
		if org.Status.Created == nil {
			reason = quayv1alpha1.ReasonAdopted
		}
		setStatusCreated(org, false)
		markerMutated, err := r.ensureOwnerMarker(ctx, qc, org, organizationOwnerToken(org, false))
		if err != nil {
			return r.fail(ctx, org, err, true)
		}
		return r.reconcileTeamsThenSucceed(ctx, logger, qc, org, quayMutation{Mutated: markerMutated}, reason,
			fmt.Sprintf("adopted existing Quay organization %q", org.Spec.Name))
	}

	message := fmt.Sprintf("Quay organization %q already exists and was not created by this resource; set spec.adopt to claim it", org.Spec.Name)
	return r.recordConflict(ctx, logger, org, quayv1alpha1.ReasonConflict, message, false)
}

// reconcileTeamsThenSucceed reconciles spec.syncedTeams into the now-owned (or
// adopted) Quay org, then marks the resource Ready. It is the single funnel every
// success path runs so teams are reconciled only after the org itself is
// provisioned. A team conflict (a spec team pre-exists and was not created by this
// resource) is surfaced as a TeamConflict condition (a non-Ready outcome) rather
// than a generic Quay error; any other Quay error fails the reconcile and requeues.
// The status write that succeed()/teamConflict() performs also persists the
// status.managedTeams set reconcileSyncedTeams computed.
func (r *OrganizationReconciler) reconcileTeamsThenSucceed(ctx context.Context, logger logr.Logger, qc OrgClient, org *quayv1alpha1.Organization, mutation quayMutation, reason, message string) (ctrl.Result, error) {
	// Snapshot the managed-team set so a change to status.managedTeams (e.g. a team
	// added, removed, or healed in) is persisted even when Ready and
	// observedGeneration are unchanged — without it, succeed() would skip the write
	// on a steady-state reconcile and lose the updated ownership record.
	before := append([]string(nil), org.Status.ManagedTeams...)
	teamMutation, err := r.reconcileSyncedTeams(ctx, qc, org)
	mutation = mutation.or(teamMutation)
	// reconcileSyncedTeams may rewrite status.managedTeams even when it returns a
	// (conflict) error — it persists the progress made before the conflict — so
	// compute the change for both the success and the conflict status write.
	managedChanged := !equalStrings(before, org.Status.ManagedTeams)
	if err != nil {
		if mutation.Mutated {
			r.stampMutation(org, mutation.HealedDrift)
		}
		if isTeamConflict(err) {
			return r.teamConflict(ctx, logger, org, err.Error(), managedChanged || mutation.Mutated)
		}
		return r.fail(ctx, org, err, managedChanged || mutation.Mutated)
	}
	if mutation.Mutated {
		r.stampMutation(org, mutation.HealedDrift)
	}
	now := metav1.Now()
	org.Status.LastValidatedTime = &now
	return r.succeed(ctx, logger, org, reason, message)
}

// equalStrings reports whether two string slices are element-wise equal. Both
// managed-team slices are kept sorted (writeManagedTeams), so a positional
// comparison is sufficient to detect a change.
func equalStrings(a, b []string) bool {
	return ctrlshared.StringSlicesEqual(a, b)
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
	return r.recordConflict(ctx, logger, org, quayv1alpha1.ReasonTeamConflict, message, managedChanged)
}

func (r *OrganizationReconciler) recordConflict(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization, reason, message string, extraChanged bool) (ctrl.Result, error) {
	conditionChanged := setConflict
	logMessage := "Organization conflict"
	if reason == quayv1alpha1.ReasonTeamConflict {
		conditionChanged = setTeamConflict
		logMessage = "Organization team conflict"
	}
	changed := conditionChanged(&org.Status.Conditions, message, org.Generation)
	changed = changed || extraChanged || org.Status.ObservedGeneration != org.Generation
	return ctrlshared.RecordConflict(ctx, r.Recorder, logger, org, reason, message, logMessage, changed, func(ctx context.Context) error {
		return r.updateStatus(ctx, org)
	}, metav1.Duration{Duration: quayExternalResourceResync})
}

// succeed stages Ready/Programmed/Accepted true and writes status on every
// successful validation so lastValidatedTime is persisted. Normal events and Info
// logs are emitted only when a condition's status, reason, or observedGeneration
// transitions.
func (r *OrganizationReconciler) succeed(ctx context.Context, logger logr.Logger, org *quayv1alpha1.Organization, reason, message string) (ctrl.Result, error) {
	beforeConditions := append([]metav1.Condition(nil), org.Status.Conditions...)
	markReady(&org.Status.Conditions, reason, message, org.Generation)
	conditionTransitioned := conditionsTransitioned(beforeConditions, org.Status.Conditions, quayv1alpha1.ConditionAccepted, quayv1alpha1.ConditionProgrammed, quayv1alpha1.ConditionReady)
	if conditionTransitioned {
		r.Recorder.Event(org, corev1.EventTypeNormal, reason, message)
		logger.Info("reconciled Organization", "name", org.Spec.Name, "reason", reason)
	}
	if err := r.updateStatus(ctx, org); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: quayExternalResourceResync}, nil
}

func (r *OrganizationReconciler) stampMutation(org *quayv1alpha1.Organization, healedDrift bool) {
	now, reason, drift := ctrlshared.MutationStamp(org.Status.ObservedGeneration, org.Generation, organizationReady(org), healedDrift)
	org.Status.LastMutatedTime = &now
	org.Status.LastMutationReason = quayv1alpha1.MutationReason(reason)
	if drift {
		org.Status.LastDriftTime = &now
	}
}

func organizationReady(org *quayv1alpha1.Organization) bool {
	return ctrlshared.GenerationReady(org.Status.Conditions, quayv1alpha1.ConditionReady, org.Generation)
}

// reconcileDelete runs the finalizer. Per the claim model the Quay org is deleted
// only when policy and the server-side marker prove this CR has delete authority;
// otherwise it is released. After cleanup the finalizer is removed so the CR is
// deleted. A Quay error during delete fails the reconcile and requeues, so the
// finalizer is not removed until cleanup succeeds.
//
// Synced teams need no separate finalizer: deleting the Quay org cascades its
// teams (and their default-permission prototypes), so the existing org delete is
// sufficient cleanup for a created org. Adopted-org edge case: when the org is
// released rather than deleted (status.Created false), the finalizer strips this
// CR's org-level marker but does not individually delete synced teams this
// controller created inside the surviving org. The platform did not create the org
// and non-destructive release is the contract. This deliberate tradeoff can leave
// controller-created teams (OIDC-synced, possibly with default repository
// permissions) behind on an adopted org, i.e. stale access grants an operator must
// clean up out of band. The alternative — deprovisioning status.managedTeams before
// releasing — would mutate an org the platform does not own. (Dropping a team from
// spec.syncedTeams while the CR lives still de-provisions that team via
// reconcileSyncedTeams; this note is only about the whole-CR delete of an adopted
// org.)
func (r *OrganizationReconciler) reconcileDelete(ctx context.Context, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(org, organizationFinalizer) {
		// Already finalized; nothing to do.
		return ctrl.Result{}, nil
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, org.Spec.CredentialsSecretRef)
	if err != nil {
		// Without the credential the Quay org cannot be deleted or have its
		// ownership marker stripped; do not strand remote state by dropping the
		// finalizer when cleanup could not run.
		return r.handleCredentialError(ctx, org, err)
	}

	// A malformed spec.caBundle must not be silently ignored on the delete path
	// either: fail (requeue) rather than build a system-trust client that does
	// not honor the spec, which keeps the finalizer in place until the bundle is
	// corrected.
	if err := quay.ValidateCABundle(org.Spec.CABundle); err != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonQuayError, err.Error())
		return ctrl.Result{}, err
	}

	qc := r.NewClient(cred, org.Spec.CABundle)
	policy := org.Spec.DeletionPolicy

	if policy == quayv1alpha1.DeletionPolicyOrphan {
		return r.orphanOrganization(ctx, qc, org)
	}

	// Verify the durable server-side marker still names this CR before deleting.
	// If a prior delete succeeded but finalizer removal failed, and another actor
	// recreated the same global org name in the gap, status.Created is still true
	// but the recreated org carries no (or a foreign) marker — deleting it would
	// destroy a stranger's org. When the marker no longer matches, release instead
	// of delete.
	owns, markerCreated, err := r.ownsViaMarker(ctx, qc, org)
	if err != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonQuayError,
			fmt.Sprintf("verifying ownership marker for Quay organization %q: %v", org.Spec.Name, err))
		return ctrl.Result{}, fmt.Errorf("verifying ownership marker for Quay organization %q: %w", org.Spec.Name, err)
	}
	shouldDelete := owns && (policy == quayv1alpha1.DeletionPolicyDelete || (policy == "" && statusCreated(org) && markerCreated))
	if !owns {
		eventType := corev1.EventTypeNormal
		if policy == quayv1alpha1.DeletionPolicyDelete {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Event(org, eventType, quayv1alpha1.ReasonReleased,
			fmt.Sprintf("released Quay organization %q without deleting (ownership marker absent or foreign)", org.Spec.Name))
		return r.removeFinalizer(ctx, org)
	}
	if !shouldDelete {
		if !markerCreated {
			mutation, err := r.removeOwnerMarkerIfOwned(ctx, qc, org)
			if err != nil {
				if mutation.Mutated {
					r.stampMutation(org, false)
					if statusErr := r.updateStatus(ctx, org); statusErr != nil {
						return ctrl.Result{}, statusErr
					}
				}
				r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonQuayError,
					fmt.Sprintf("releasing ownership marker for Quay organization %q: %v", org.Spec.Name, err))
				return ctrl.Result{}, fmt.Errorf("releasing ownership marker for Quay organization %q: %w", org.Spec.Name, err)
			}
			if mutation.Mutated {
				r.stampMutation(org, false)
				if err := r.updateStatus(ctx, org); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		r.Recorder.Event(org, corev1.EventTypeNormal, quayv1alpha1.ReasonReleased,
			fmt.Sprintf("released Quay organization %q without deleting (adopted, not created by this resource)", org.Spec.Name))
		return r.removeFinalizer(ctx, org)
	}

	// Delete the org BEFORE the marker. Ordering matters for retry safety: if the
	// marker were removed first and the org delete then failed, the next retry would
	// see no marker, classify the org as no longer ours, and release the finalizer
	// — leaking the platform-created org. By deleting the org first, a failed org
	// delete leaves the marker intact so the retry still verifies ownership and
	// re-attempts the delete. Deleting the org also removes its robots, so the
	// explicit marker cleanup below is a best-effort tidy-up of an already-gone
	// robot.
	delErr := qc.DeleteOrganizationIfExists(ctx, org.Spec.Name)
	recordQuayAPI(opDeleteOrganization, delErr)
	if delErr != nil {
		r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonQuayError,
			fmt.Sprintf("deleting Quay organization %q: %v", org.Spec.Name, delErr))
		return ctrl.Result{}, fmt.Errorf("deleting Quay organization %q: %w", org.Spec.Name, delErr)
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
// Quay org still names this CR (its UID), and whether that marker records created
// provenance. A NotFound marker means the org carries no marker — it is not the
// org this CR may delete. A non-marker Quay error is returned so the caller
// requeues rather than guessing.
func (r *OrganizationReconciler) ownsViaMarker(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) (bool, bool, error) {
	robot, err := qc.GetOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opGetOrganizationRobot, ignoreNotFound(err))
	if quay.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	uid, created := parseOrganizationOwnerToken(robot.Description)
	return uid == ownerToken(org), created, nil
}

func (r *OrganizationReconciler) orphanOrganization(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) (ctrl.Result, error) {
	mutation, err := r.removeOwnerMarkerIfOwned(ctx, qc, org)
	if err != nil {
		if mutation.Mutated {
			r.stampMutation(org, false)
			if statusErr := r.updateStatus(ctx, org); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
		}
		r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonQuayError,
			fmt.Sprintf("orphaning Quay organization %q: %v", org.Spec.Name, err))
		return ctrl.Result{}, fmt.Errorf("orphaning Quay organization %q: %w", org.Spec.Name, err)
	}
	if mutation.Mutated {
		r.stampMutation(org, false)
		if err := r.updateStatus(ctx, org); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.Recorder.Event(org, corev1.EventTypeNormal, quayv1alpha1.ReasonReleased,
		fmt.Sprintf("released Quay organization %q without deleting (orphaned by deletionPolicy)", org.Spec.Name))
	return r.removeFinalizer(ctx, org)
}

func (r *OrganizationReconciler) removeOwnerMarkerIfOwned(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) (quayMutation, error) {
	robot, err := qc.GetOrganizationRobot(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opGetOrganizationRobot, ignoreNotFound(err))
	if quay.IsNotFound(err) {
		return quayMutation{}, nil
	}
	if err != nil {
		return quayMutation{}, err
	}
	uid, _ := parseOrganizationOwnerToken(robot.Description)
	if uid != ownerToken(org) {
		return quayMutation{}, nil
	}
	delErr := qc.DeleteOrganizationRobotIfExists(ctx, org.Spec.Name, ownerRobotShortname)
	recordQuayAPI(opDeleteOrganizationRobot, delErr)
	if delErr != nil {
		return quayMutation{}, delErr
	}
	return quayMutation{Mutated: true}, nil
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
	if changed := markNotReady(&org.Status.Conditions, quayv1alpha1.ReasonCredentialsNotFound, err.Error(), org.Generation); changed {
		r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonCredentialsNotFound, err.Error())
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
func (r *OrganizationReconciler) fail(ctx context.Context, org *quayv1alpha1.Organization, err error, extraChanged bool) (ctrl.Result, error) {
	conditionChanged := markNotReady(&org.Status.Conditions, quayv1alpha1.ReasonQuayError, err.Error(), org.Generation)
	if conditionChanged || extraChanged {
		if conditionChanged {
			r.Recorder.Event(org, corev1.EventTypeWarning, quayv1alpha1.ReasonQuayError, err.Error())
		}
		if statusErr := r.updateStatus(ctx, org); statusErr != nil {
			// Prefer surfacing the original Quay error; log the status failure.
			log.FromContext(ctx).Error(statusErr, "updating status after Quay error")
		}
	}
	return ctrl.Result{}, err
}

// updateStatus stamps observedGeneration and patches the status subresource from
// a live merge base. A NotFound (the CR was deleted concurrently) is ignored.
func (r *OrganizationReconciler) updateStatus(ctx context.Context, org *quayv1alpha1.Organization) error {
	base := org.DeepCopy()
	org.Status.ObservedGeneration = org.Generation
	org.Status.ManagedTeams = normalizeManagedTeams(org.Status.ManagedTeams)
	return ctrlshared.PatchStatus(ctx, r.Client, base, org, "Organization")
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

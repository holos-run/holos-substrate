package quay

import (
	"context"
	"errors"
	"fmt"
	"sort"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// managedTeamDescription labels every team this controller creates so a human
// reading the Quay UI can tell controller-managed synced teams apart from
// manually-created ones. It is the team-level analog of the Repository
// reconciler's webhookTitle ownership marker, and (with status.managedTeams) lets
// the reconciler heal a team back into ownership after a lost status write
// (see reconcileSyncedTeams' heal rule).
const managedTeamDescription = "managed by holos-controller"

// teamConflictError reports that a spec.syncedTeams entry names a pre-existing
// Quay team this resource did not create. It is a sentinel the caller branches on
// to set the TeamConflict condition (a non-Ready outcome) rather than a generic
// Quay error: adoption of a pre-existing team is deliberately a reconcile-time
// error, never a silent takeover (AC #6, mirroring the org-level claim model).
//
// Adoption is an error in the reconciler only — no API field forbids it. A future
// optional per-team `adopt bool` on SyncedTeam can flip this conflict path to
// adoption without an API break (AC #6 forward-compatibility).
type teamConflictError struct {
	team string
}

func (e *teamConflictError) Error() string {
	return fmt.Sprintf("Quay team %q already exists and was not created by this resource; refusing to adopt it (set a future per-team adopt to claim it)", e.team)
}

// isTeamConflict reports whether err is (or wraps) a teamConflictError.
func isTeamConflict(err error) bool {
	var tc *teamConflictError
	return errors.As(err, &tc)
}

// reconcileSyncedTeams drives spec.syncedTeams into the Quay org org owns. It is
// called only after the org itself is provisioned (Created or Adopted), so teams
// are never touched on an org this CR does not own (AC #1).
//
// Management is non-exclusive (AC #5): the controller manages exactly the teams it
// created — those recorded in status.managedTeams plus those it creates this pass
// — and ignores every other team in the org. A team that is neither in the spec
// nor in status.managedTeams is left untouched.
//
// Ownership and the heal rule (AC #4): a desired team is owned when it is recorded
// in status.managedTeams. To survive a lost status write after a create (the
// team-level analog of the org marker's heal), a Quay team whose sync binding is
// already bound to exactly the spec entry's oidcGroup is also treated as
// controller-managed and healed back into status.managedTeams — the durable
// server-side sync binding stands in for the lost owner record, mirroring ADR-19's
// ownership-marker philosophy. A team that exists but is neither recorded nor
// bound to the desired group is a conflict (teamConflictError), never adopted.
//
// On success it rewrites org.Status.ManagedTeams to exactly the set of teams the
// controller now manages (sorted for a stable status). On the first conflict it
// returns a teamConflictError so the caller sets the TeamConflict condition; the
// managed set computed up to that point is still persisted so progress is durable.
func (r *OrganizationReconciler) reconcileSyncedTeams(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) error {
	orgName := org.Spec.Name

	// The set of teams this CR currently records as managed (its durable owner
	// record). Map for O(1) membership, mutated as teams are added/removed.
	managed := map[string]bool{}
	for _, name := range org.Status.ManagedTeams {
		managed[name] = true
	}

	// List the org's current teams once so each desired team's existence and role
	// are known without a per-team round trip, mirroring the Repository
	// reconciler's list-then-diff shape.
	teams, err := qc.ListTeams(ctx, orgName)
	recordQuayAPI(opListTeams, err)
	if err != nil {
		return fmt.Errorf("listing Quay teams for organization %q: %w", orgName, err)
	}

	// Index desired teams by name for the de-provision diff below.
	desired := map[string]*quayv1alpha1.SyncedTeam{}
	for i := range org.Spec.SyncedTeams {
		t := &org.Spec.SyncedTeams[i]
		desired[t.Name] = t
	}

	// De-provision: a team recorded as managed but dropped from the spec is fully
	// removed (disable sync, delete its default-permission prototype, delete the
	// team) and dropped from the managed set (AC #3). Teams that are neither
	// desired nor managed are ignored entirely (non-exclusive, AC #5).
	for name := range managed {
		if _, stillDesired := desired[name]; stillDesired {
			continue
		}
		if err := r.deprovisionTeam(ctx, qc, orgName, name); err != nil {
			return err
		}
		delete(managed, name)
	}

	// Reconcile each desired team. Process in spec order so the first conflict is
	// deterministic; record managed-set progress before returning a conflict so it
	// persists.
	for i := range org.Spec.SyncedTeams {
		t := &org.Spec.SyncedTeams[i]
		existing, present := teams[t.Name]
		owned := managed[t.Name]

		if present && !owned {
			// The team exists but is not recorded as ours. Heal it into ownership
			// only when its durable sync binding already names this entry's
			// oidcGroup (a lost status write after our own create, AC #4); otherwise
			// it was created by someone else and must not be adopted (AC #6).
			healed, err := r.teamBoundToGroup(ctx, qc, orgName, t.Name, t.OIDCGroup)
			if err != nil {
				return err
			}
			if !healed {
				// Conflict: persist progress so far, then surface the conflict.
				r.writeManagedTeams(org, managed)
				return &teamConflictError{team: t.Name}
			}
			owned = true
			managed[t.Name] = true
		}

		if err := r.ensureTeam(ctx, qc, orgName, t, existing, present); err != nil {
			return err
		}
		managed[t.Name] = true
	}

	r.writeManagedTeams(org, managed)
	return nil
}

// ensureTeam creates or reconciles a single desired team and its optional
// default-permission prototype. existing/present describe the team as last listed
// from Quay (present=false means it must be created).
func (r *OrganizationReconciler) ensureTeam(ctx context.Context, qc OrgClient, orgName string, t *quayv1alpha1.SyncedTeam, existing quay.Team, present bool) error {
	role := string(t.Role)

	// Upsert the team to the desired role. PUT is create-or-update, so this both
	// creates an absent team and corrects role drift on an existing one in one call.
	if !present || existing.Role != role {
		err := qc.UpsertTeam(ctx, orgName, t.Name, role, managedTeamDescription)
		recordQuayAPI(opUpsertTeam, err)
		if err != nil {
			return fmt.Errorf("upserting Quay team %q in organization %q: %w", t.Name, orgName, err)
		}
	}

	// Bind (or re-bind) the team's membership to the desired OIDC group.
	// EnableTeamSyncIfNotSynced no-ops when already bound to t.OIDCGroup; when bound
	// to a different group it surfaces the drift, which we correct by disabling and
	// re-enabling so the binding always tracks the spec (AC #2 oidcGroup re-bind).
	if err := r.bindTeamSync(ctx, qc, orgName, t.Name, t.OIDCGroup); err != nil {
		return err
	}

	// Reconcile the optional org default-permission prototype delegating a repo
	// role to this team.
	return r.reconcileTeamPrototype(ctx, qc, orgName, t)
}

// bindTeamSync ensures the team's sync binding names oidcGroup, re-binding
// (disable then enable) when it is currently bound to a different group so an
// oidcGroup change in the spec takes effect (AC #2).
func (r *OrganizationReconciler) bindTeamSync(ctx context.Context, qc OrgClient, orgName, team, oidcGroup string) error {
	members, err := qc.GetTeamMembers(ctx, orgName, team)
	recordQuayAPI(opGetTeamMembers, err)
	if err != nil {
		return fmt.Errorf("reading Quay team %q members in organization %q: %w", team, orgName, err)
	}

	if members.Synced != nil && members.GroupName() != oidcGroup {
		// Bound to a different group: drop the stale binding before re-binding, since
		// Quay rejects enabling sync on an already-synced team.
		disableErr := qc.DisableTeamSyncIfSynced(ctx, orgName, team)
		recordQuayAPI(opDisableTeamSync, disableErr)
		if disableErr != nil {
			return fmt.Errorf("disabling stale sync on Quay team %q in organization %q: %w", team, orgName, disableErr)
		}
	}

	enableErr := qc.EnableTeamSyncIfNotSynced(ctx, orgName, team, oidcGroup)
	recordQuayAPI(opEnableTeamSync, enableErr)
	if enableErr != nil {
		return fmt.Errorf("enabling sync on Quay team %q in organization %q to group %q: %w", team, orgName, oidcGroup, enableErr)
	}
	return nil
}

// reconcileTeamPrototype makes the org default-permission prototype delegating to
// team match t.RepositoryPermission: create it when desired and absent, update its
// role on drift, and delete it when no longer desired. It finds the controller's
// prototype for this team by matching the delegate (kind team, name team) — only a
// prototype delegating to this exact team is touched, so peers and manually-created
// prototypes are left alone (non-exclusive, mirroring the webhook diff).
func (r *OrganizationReconciler) reconcileTeamPrototype(ctx context.Context, qc OrgClient, orgName string, t *quayv1alpha1.SyncedTeam) error {
	prototypes, err := qc.ListPrototypes(ctx, orgName)
	recordQuayAPI(opListPrototypes, err)
	if err != nil {
		return fmt.Errorf("listing Quay prototypes for organization %q: %w", orgName, err)
	}

	existing := findTeamPrototype(prototypes, t.Name)

	if t.RepositoryPermission == nil {
		// No default permission desired: delete the controller's prototype for this
		// team if one exists.
		if existing == nil {
			return nil
		}
		delErr := qc.DeletePrototypeIfExists(ctx, orgName, existing.ID)
		recordQuayAPI(opDeletePrototype, delErr)
		if delErr != nil {
			return fmt.Errorf("deleting Quay default permission for team %q in organization %q: %w", t.Name, orgName, delErr)
		}
		return nil
	}

	role := string(*t.RepositoryPermission)
	if existing == nil {
		_, createErr := qc.CreatePrototype(ctx, orgName, role, t.Name)
		recordQuayAPI(opCreatePrototype, createErr)
		if createErr != nil {
			return fmt.Errorf("creating Quay default permission for team %q in organization %q: %w", t.Name, orgName, createErr)
		}
		return nil
	}
	if existing.Role != role {
		updateErr := qc.UpdatePrototype(ctx, orgName, existing.ID, role)
		recordQuayAPI(opUpdatePrototype, updateErr)
		if updateErr != nil {
			return fmt.Errorf("updating Quay default permission for team %q in organization %q: %w", t.Name, orgName, updateErr)
		}
	}
	return nil
}

// deprovisionTeam fully removes a controller-managed team that was dropped from
// the spec: it deletes the team's default-permission prototype (if any), disables
// its sync binding, and deletes the team (AC #3). Each step is idempotent so a
// retry after a partial failure converges.
func (r *OrganizationReconciler) deprovisionTeam(ctx context.Context, qc OrgClient, orgName, team string) error {
	prototypes, err := qc.ListPrototypes(ctx, orgName)
	recordQuayAPI(opListPrototypes, err)
	if err != nil {
		return fmt.Errorf("listing Quay prototypes for organization %q: %w", orgName, err)
	}
	if p := findTeamPrototype(prototypes, team); p != nil {
		delErr := qc.DeletePrototypeIfExists(ctx, orgName, p.ID)
		recordQuayAPI(opDeletePrototype, delErr)
		if delErr != nil {
			return fmt.Errorf("deleting Quay default permission for removed team %q in organization %q: %w", team, orgName, delErr)
		}
	}

	disableErr := qc.DisableTeamSyncIfSynced(ctx, orgName, team)
	recordQuayAPI(opDisableTeamSync, disableErr)
	if disableErr != nil {
		return fmt.Errorf("disabling sync on removed Quay team %q in organization %q: %w", team, orgName, disableErr)
	}

	delErr := qc.DeleteTeamIfExists(ctx, orgName, team)
	recordQuayAPI(opDeleteTeam, delErr)
	if delErr != nil {
		return fmt.Errorf("deleting removed Quay team %q in organization %q: %w", team, orgName, delErr)
	}
	return nil
}

// teamBoundToGroup reports whether the team's current sync binding names oidcGroup.
// It is the heal check (AC #4): a team that exists but is not recorded as ours is
// only adopted-by-healing when its durable server-side binding already matches the
// desired group (proving this controller created it and only the status write was
// lost). A read error surfaces so the reconcile requeues rather than guessing.
func (r *OrganizationReconciler) teamBoundToGroup(ctx context.Context, qc OrgClient, orgName, team, oidcGroup string) (bool, error) {
	members, err := qc.GetTeamMembers(ctx, orgName, team)
	recordQuayAPI(opGetTeamMembers, err)
	if err != nil {
		return false, fmt.Errorf("reading Quay team %q members in organization %q: %w", team, orgName, err)
	}
	return members.Synced != nil && members.GroupName() == oidcGroup, nil
}

// writeManagedTeams sets org.Status.ManagedTeams to the sorted names in the
// managed set, so the status is stable across reconciles (a map's range order is
// nondeterministic). An empty set clears the field to nil.
func (r *OrganizationReconciler) writeManagedTeams(org *quayv1alpha1.Organization, managed map[string]bool) {
	if len(managed) == 0 {
		org.Status.ManagedTeams = nil
		return
	}
	names := make([]string, 0, len(managed))
	for name := range managed {
		names = append(names, name)
	}
	sort.Strings(names)
	org.Status.ManagedTeams = names
}

// findTeamPrototype returns the prototype delegating to the named team (kind team),
// or nil. The controller delegates default permissions only to teams, so a match
// on the delegate name with kind team uniquely identifies the prototype this
// controller manages for that team.
func findTeamPrototype(prototypes []quay.Prototype, team string) *quay.Prototype {
	for i := range prototypes {
		p := &prototypes[i]
		if p.Delegate.Kind == quay.PrototypeDelegateTeam && p.Delegate.Name == team {
			return p
		}
	}
	return nil
}

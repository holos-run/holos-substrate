package quay

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// managedTeamPrefix is the human-readable prefix of the description stamped on
// every team this controller creates, so an operator reading the Quay UI can tell
// controller-managed synced teams apart from manually-created ones. The full
// marker appends this CR's opaque ownership token (managedTeamMarker).
const managedTeamPrefix = "managed by holos-controller "

// managedTeamMarker is the durable, server-side ownership marker stamped into a
// team's description for org — the team-level analog of the org's holos-owner
// robot marker. It embeds ownerToken(org) (the CR's UID), so it is resource-
// specific and unforgeable: a manually-created team cannot accidentally carry
// another CR's marker, which is what lets teamHealable trust the marker as proof
// this exact CR created the team (closing the heal/adoption hole — a static,
// guessable description string is not sufficient proof of ownership).
func managedTeamMarker(org *quayv1alpha1.Organization) string {
	return managedTeamPrefix + ownerToken(org)
}

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
// team-level analog of the org marker's heal), a Quay team whose description
// carries this CR's managedTeamMarker (the controller prefix plus ownerToken — the
// CR UID) is also treated as controller-managed and healed back into
// status.managedTeams. The marker is the durable, server-side owner record that
// stands in for the lost status, mirroring ADR-19's holos-owner robot: because it
// embeds the CR UID it is unforgeable, so a team that exists but lacks this CR's
// marker is a conflict (teamConflictError), never adopted — even if it happens to
// be bound to the desired oidcGroup.
//
// On success it rewrites org.Status.ManagedTeams to exactly the set of teams the
// controller now manages (sorted for a stable status). On the first conflict it
// returns a teamConflictError so the caller sets the TeamConflict condition; the
// managed set computed up to that point is still persisted so progress is durable.
func (r *OrganizationReconciler) reconcileSyncedTeams(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization) (quayMutation, error) {
	mutation := quayMutation{}
	orgName := org.Spec.Name
	canMarkDrift := organizationReady(org)

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
		return mutation, fmt.Errorf("listing Quay teams for organization %q: %w", orgName, err)
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
		_, teamPresent := teams[name]
		teamMutation, err := r.deprovisionTeam(ctx, qc, orgName, name, teamPresent)
		mutation = mutation.or(teamMutation)
		if err != nil {
			r.writeManagedTeams(org, managed)
			return mutation, err
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
		ownedBefore := owned || (present && existing.Description == managedTeamMarker(org))

		if present && !owned {
			// Heal-or-conflict for an existing team this CR does not yet record.
			// The team exists but is not recorded as ours. Heal it into ownership
			// only when it carries this CR's durable, unforgeable server-side
			// ownership marker — managedTeamMarker(org), the team-level analog of the
			// org's holos-owner robot stamped with ownerToken (the CR UID). The
			// marker alone is sufficient proof this exact CR created the team (a lost
			// status write after our own create, AC #4): it cannot be forged, so a
			// hand-created team lacks it and is NOT adopted — it is a conflict,
			// preserving the no-adoption / non-exclusive model (AC #5/#6). Relying on
			// the marker (not the sync binding) also makes recovery robust: a team
			// created last pass whose sync/prototype step then failed still carries
			// the marker, so this pass heals it rather than wedging into a false
			// TeamConflict. Adoption stays a reconcile-time error only; a future
			// per-team adopt can flip this conflict path without an API break.
			if existing.Description != managedTeamMarker(org) {
				// Conflict: persist progress so far, then surface the conflict.
				r.writeManagedTeams(org, managed)
				return mutation, &teamConflictError{team: t.Name}
			}
			managed[t.Name] = true
		}

		// Record ownership immediately — before the sync/prototype steps that follow
		// — so a transient Quay failure mid-team does not strand a controller-created
		// team as "unmanaged" and wedge the next reconcile into a false TeamConflict.
		// The managed set is persisted by the caller's status write on success; on a
		// mid-team error the durable managedTeamMarker UpsertTeam stamps (carrying
		// this CR's UID) is the backstop that lets the next reconcile heal the team
		// back into ownership rather than mistaking it for foreign — independent of
		// whether the sync binding step had a chance to run.
		teamMutation, err := r.ensureTeamUpserted(ctx, qc, org, t, existing, present, owned, canMarkDrift)
		mutation = mutation.or(teamMutation)
		if err != nil {
			r.writeManagedTeams(org, managed)
			return mutation, err
		}
		managed[t.Name] = true

		teamMutation, err = r.ensureTeamSyncAndPrototype(ctx, qc, orgName, t, ownedBefore, canMarkDrift)
		mutation = mutation.or(teamMutation)
		if err != nil {
			r.writeManagedTeams(org, managed)
			return mutation, err
		}
	}

	r.writeManagedTeams(org, managed)
	return mutation, nil
}

// ensureTeamUpserted upserts a single desired team to its role with the
// controller's ownership description, stamping the durable managedTeamMarker (this
// CR's UID). PUT is create-or-update, so this both creates an absent team and
// corrects role drift on an existing one in one call. It is the first,
// ownership-stamping step; bindTeamSync/reconcileTeamPrototype follow in
// ensureTeamSyncAndPrototype. existing/present describe the team as last listed
// from Quay.
//
// The upsert always runs when the team is absent, its role drifted, or its
// description is not yet this CR's marker (so a team created with an older or
// missing marker gets stamped), and is skipped only when the team already matches
// on both role and marker — keeping a steady-state reconcile call-free.
func (r *OrganizationReconciler) ensureTeamUpserted(ctx context.Context, qc OrgClient, org *quayv1alpha1.Organization, t *quayv1alpha1.SyncedTeam, existing quay.Team, present, owned, canMarkDrift bool) (quayMutation, error) {
	role := string(t.Role)
	marker := managedTeamMarker(org)
	if present && existing.Role == role && existing.Description == marker {
		return quayMutation{}, nil
	}
	err := qc.UpsertTeam(ctx, org.Spec.Name, t.Name, role, marker)
	recordQuayAPI(opUpsertTeam, err)
	if err != nil {
		return quayMutation{}, fmt.Errorf("upserting Quay team %q in organization %q: %w", t.Name, org.Spec.Name, err)
	}
	return quayMutation{Mutated: true, HealedDrift: canMarkDrift && present && (owned || existing.Description == marker)}, nil
}

// ensureTeamSyncAndPrototype binds the team's OIDC sync and reconciles its
// optional default-permission prototype. It runs after ensureTeamUpserted has
// stamped ownership, so a failure here leaves a team that the next reconcile heals
// back into ownership via its description marker rather than mistaking for foreign.
func (r *OrganizationReconciler) ensureTeamSyncAndPrototype(ctx context.Context, qc OrgClient, orgName string, t *quayv1alpha1.SyncedTeam, ownedBefore, canMarkDrift bool) (quayMutation, error) {
	// Bind (or re-bind) the team's membership to the desired OIDC group.
	// EnableTeamSyncIfNotSynced no-ops when already bound to t.OIDCGroup; when bound
	// to a different group it surfaces the drift, which we correct by disabling and
	// re-enabling so the binding always tracks the spec (AC #2 oidcGroup re-bind).
	syncMutation, err := r.bindTeamSync(ctx, qc, orgName, t.Name, t.OIDCGroup, ownedBefore, canMarkDrift)
	if err != nil {
		return syncMutation, err
	}

	// Reconcile the optional org default-permission prototype delegating a repo
	// role to this team.
	prototypeMutation, err := r.reconcileTeamPrototype(ctx, qc, orgName, t, ownedBefore, canMarkDrift)
	return syncMutation.or(prototypeMutation), err
}

// bindTeamSync ensures the team's sync binding names oidcGroup, re-binding
// (disable then enable) when it is currently bound to a different group so an
// oidcGroup change in the spec takes effect (AC #2).
func (r *OrganizationReconciler) bindTeamSync(ctx context.Context, qc OrgClient, orgName, team, oidcGroup string, ownedBefore, canMarkDrift bool) (quayMutation, error) {
	mutation := quayMutation{}
	members, err := qc.GetTeamMembers(ctx, orgName, team)
	recordQuayAPI(opGetTeamMembers, err)
	if err != nil {
		return mutation, fmt.Errorf("reading Quay team %q members in organization %q: %w", team, orgName, err)
	}

	if members.Synced != nil && members.GroupName() != oidcGroup {
		// Bound to a different group: drop the stale binding before re-binding, since
		// Quay rejects enabling sync on an already-synced team.
		disableErr := qc.DisableTeamSyncIfSynced(ctx, orgName, team)
		recordQuayAPI(opDisableTeamSync, disableErr)
		if disableErr != nil {
			return mutation, fmt.Errorf("disabling stale sync on Quay team %q in organization %q: %w", team, orgName, disableErr)
		}
		mutation = mutation.or(quayMutation{Mutated: true, HealedDrift: canMarkDrift})
	}

	if members.Synced == nil || members.GroupName() != oidcGroup {
		enableErr := qc.EnableTeamSyncIfNotSynced(ctx, orgName, team, oidcGroup)
		recordQuayAPI(opEnableTeamSync, enableErr)
		if enableErr != nil {
			return mutation, fmt.Errorf("enabling sync on Quay team %q in organization %q to group %q: %w", team, orgName, oidcGroup, enableErr)
		}
		mutation = mutation.or(quayMutation{Mutated: true, HealedDrift: canMarkDrift && ownedBefore})
	}
	return mutation, nil
}

// reconcileTeamPrototype makes the org default-permission prototype delegating to
// team match t.RepositoryPermission: create it when desired and absent, update its
// role on drift, and delete it when no longer desired. It finds the controller's
// prototype for this team by matching the delegate (kind team, name team) — only a
// prototype delegating to this exact team is touched, so peers and manually-created
// prototypes are left alone (non-exclusive, mirroring the webhook diff).
func (r *OrganizationReconciler) reconcileTeamPrototype(ctx context.Context, qc OrgClient, orgName string, t *quayv1alpha1.SyncedTeam, ownedBefore, canMarkDrift bool) (quayMutation, error) {
	prototypes, err := qc.ListPrototypes(ctx, orgName)
	recordQuayAPI(opListPrototypes, err)
	if err != nil {
		return quayMutation{}, fmt.Errorf("listing Quay prototypes for organization %q: %w", orgName, err)
	}

	existing := findTeamPrototype(prototypes, t.Name)

	if t.RepositoryPermission == nil {
		// No default permission desired: delete the controller's prototype for this
		// team if one exists.
		if existing == nil {
			return quayMutation{}, nil
		}
		delErr := qc.DeletePrototypeIfExists(ctx, orgName, existing.ID)
		recordQuayAPI(opDeletePrototype, delErr)
		if delErr != nil {
			return quayMutation{}, fmt.Errorf("deleting Quay default permission for team %q in organization %q: %w", t.Name, orgName, delErr)
		}
		return quayMutation{Mutated: true, HealedDrift: canMarkDrift}, nil
	}

	role := string(*t.RepositoryPermission)
	if existing == nil {
		_, createErr := qc.CreatePrototype(ctx, orgName, role, t.Name)
		recordQuayAPI(opCreatePrototype, createErr)
		if createErr != nil {
			return quayMutation{}, fmt.Errorf("creating Quay default permission for team %q in organization %q: %w", t.Name, orgName, createErr)
		}
		return quayMutation{Mutated: true, HealedDrift: canMarkDrift && ownedBefore}, nil
	}
	if existing.Role != role {
		updateErr := qc.UpdatePrototype(ctx, orgName, existing.ID, role)
		recordQuayAPI(opUpdatePrototype, updateErr)
		if updateErr != nil {
			return quayMutation{}, fmt.Errorf("updating Quay default permission for team %q in organization %q: %w", t.Name, orgName, updateErr)
		}
		return quayMutation{Mutated: true, HealedDrift: canMarkDrift}, nil
	}
	return quayMutation{}, nil
}

// deprovisionTeam fully removes a controller-managed team that was dropped from
// the spec: it deletes the team's default-permission prototype (if any), disables
// its sync binding, and deletes the team (AC #3). Each step is idempotent so a
// retry after a partial failure converges.
func (r *OrganizationReconciler) deprovisionTeam(ctx context.Context, qc OrgClient, orgName, team string, teamPresent bool) (quayMutation, error) {
	mutation := quayMutation{}
	prototypes, err := qc.ListPrototypes(ctx, orgName)
	recordQuayAPI(opListPrototypes, err)
	if err != nil {
		return mutation, fmt.Errorf("listing Quay prototypes for organization %q: %w", orgName, err)
	}
	if p := findTeamPrototype(prototypes, team); p != nil {
		delErr := qc.DeletePrototypeIfExists(ctx, orgName, p.ID)
		recordQuayAPI(opDeletePrototype, delErr)
		if delErr != nil {
			return mutation, fmt.Errorf("deleting Quay default permission for removed team %q in organization %q: %w", team, orgName, delErr)
		}
		mutation = mutation.or(quayMutation{Mutated: true})
	}

	if teamPresent {
		members, err := qc.GetTeamMembers(ctx, orgName, team)
		recordQuayAPI(opGetTeamMembers, ignoreNotFound(err))
		if quay.IsNotFound(err) {
			teamPresent = false
		} else if err != nil {
			return mutation, fmt.Errorf("reading Quay team %q members in organization %q before deprovision: %w", team, orgName, err)
		} else if members.Synced != nil {
			disableErr := qc.DisableTeamSyncIfSynced(ctx, orgName, team)
			recordQuayAPI(opDisableTeamSync, disableErr)
			if disableErr != nil {
				return mutation, fmt.Errorf("disabling sync on removed Quay team %q in organization %q: %w", team, orgName, disableErr)
			}
			mutation = mutation.or(quayMutation{Mutated: true})
		}
	}

	delErr := qc.DeleteTeamIfExists(ctx, orgName, team)
	recordQuayAPI(opDeleteTeam, delErr)
	if delErr != nil {
		return mutation, fmt.Errorf("deleting removed Quay team %q in organization %q: %w", team, orgName, delErr)
	}
	if teamPresent {
		mutation = mutation.or(quayMutation{Mutated: true})
	}
	return mutation, nil
}

// writeManagedTeams sets org.Status.ManagedTeams to the sorted names in the
// managed set, so the status is stable across reconciles (a map's range order is
// nondeterministic). An empty set clears the field to nil.
func (r *OrganizationReconciler) writeManagedTeams(org *quayv1alpha1.Organization, managed map[string]bool) {
	if len(managed) == 0 {
		org.Status.ManagedTeams = nil
		return
	}
	names := slices.Sorted(maps.Keys(managed))
	org.Status.ManagedTeams = normalizeManagedTeams(names)
}

// normalizeManagedTeams returns a sorted set of managed team names. It protects
// status writes from stale duplicate entries that would be rejected by the CRD's
// set semantics.
func normalizeManagedTeams(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		normalized = append(normalized, name)
	}
	if len(normalized) == 0 {
		return nil
	}
	slices.Sort(normalized)
	return normalized
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

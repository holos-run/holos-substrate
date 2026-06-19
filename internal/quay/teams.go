package quay

import (
	"context"
	"net/http"
	"net/url"
)

// Quay organization team role values accepted by the team upsert endpoint.
// Quay's TeamDescription schema enumerates exactly these three.
const (
	// TeamRoleMember can read the org's repositories its team is granted.
	TeamRoleMember = "member"
	// TeamRoleCreator additionally creates new repositories in the org.
	TeamRoleCreator = "creator"
	// TeamRoleAdmin administers the org.
	TeamRoleAdmin = "admin"
)

// Team is the subset of a Quay organization team the reconciler reads back from
// the org payload's teams map (GET /api/v1/organization/{orgname}). Quay returns
// more fields per team; only the ones the controller uses are decoded.
//
// The map is keyed by team name, and Quay also echoes the name inside each
// entry, so callers can rely on Name regardless of how they reached the value.
type Team struct {
	// Name is the team name within the organization.
	Name string `json:"name"`
	// Role is the team's org-wide role: member, creator, or admin.
	Role string `json:"role"`
	// Description is the free-text team description.
	Description string `json:"description,omitempty"`
	// IsSynced reports whether the team's membership is bound to (synced from)
	// an external group — for the OIDC backend, an OIDC groups-claim value. The
	// org payload exposes only this boolean; the bound group name and sync
	// service are read from GetTeamMembers, which returns the full Synced
	// binding.
	IsSynced bool `json:"is_synced"`
}

// TeamMembers is the subset of the team-members payload
// (GET /api/v1/organization/{orgname}/team/{teamname}/members) the reconciler
// reads. It carries the sync binding so the reconciler can detect drift (is the
// team bound to the desired OIDC group?) and ownership (is it bound at all?).
type TeamMembers struct {
	// Name is the team name.
	Name string `json:"name"`
	// CanSync describes the available sync service when the team is not yet
	// synced; it is null/absent once a binding exists. Quay populates it with
	// the service metadata (e.g. the configured authentication service name).
	CanSync map[string]any `json:"can_sync,omitempty"`
	// Synced is the active sync binding, present only when the team is synced.
	// It is nil when the team has no binding.
	Synced *TeamSync `json:"synced,omitempty"`
}

// TeamSync is the active team-sync binding Quay reports inside the team-members
// payload's synced field. For the OIDC backend the bound group name lives under
// Config["group_name"], matching the body EnableTeamSync sends.
type TeamSync struct {
	// Service is the sync service name (the configured authentication service,
	// e.g. the OIDC service).
	Service string `json:"service"`
	// LastUpdated is Quay's human-readable timestamp of the last resync.
	LastUpdated string `json:"last_updated,omitempty"`
	// Config is the opaque sync configuration; for the OIDC backend it carries
	// the bound group under the group_name key (see EnableTeamSync).
	Config map[string]any `json:"config,omitempty"`
}

// GroupName returns the bound OIDC group name from the sync config, or "" when
// the team is not synced or the binding carries no group_name. It is the
// reconciler's drift check against the desired OIDC group.
func (m *TeamMembers) GroupName() string {
	if m == nil || m.Synced == nil {
		return ""
	}
	if g, ok := m.Synced.Config["group_name"].(string); ok {
		return g
	}
	return ""
}

// upsertTeamRequest is the PUT /api/v1/organization/{orgname}/team/{teamname}
// body (Quay's TeamDescription schema): a required role and a description.
//
// The description has no omitempty: Quay's handler only changes the description
// when the JSON key is present, so omitting it on an empty desired value would
// leave a previously-set description in place and the reconciled team
// permanently drifted. Always sending the key lets UpsertTeam clear a
// description by passing "".
type upsertTeamRequest struct {
	Role        string `json:"role"`
	Description string `json:"description"`
}

// enableTeamSyncRequest is the POST
// /api/v1/organization/{orgname}/team/{teamname}/syncing body. Quay passes the
// raw JSON straight to the configured authentication backend's group-lookup
// check; the OIDC backend (data/users/externaloidc.py) reads the bound group
// from the group_name key (sync_group_info.get("group_name")), so for the
// platform's OIDC-backed Quay the body is {"group_name": "<oidcGroup>"}.
type enableTeamSyncRequest struct {
	GroupName string `json:"group_name"`
}

// teamPath builds the /api/v1/organization/{orgname}/team/{teamname} path with
// each segment escaped.
func teamPath(org, team string) string {
	return "/api/v1/organization/" + url.PathEscape(org) + "/team/" + url.PathEscape(team)
}

// ListTeams returns the organization's teams keyed by team name, derived from
// GET /api/v1/organization/{orgname} (the org payload carries a teams map). It
// gives the reconciler each team's current role and whether it exists and is
// synced, in a single request, without a per-team round trip. A missing
// organization is returned as an *APIError reporting IsNotFound.
func (c *Client) ListTeams(ctx context.Context, org string) (map[string]Team, error) {
	var out struct {
		Teams map[string]Team `json:"teams"`
	}
	path := "/api/v1/organization/" + url.PathEscape(org)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	// Backfill each entry's Name from its map key, since a caller that ranges
	// the map by key should not depend on Quay also echoing the name inside.
	for name, team := range out.Teams {
		if team.Name == "" {
			team.Name = name
			out.Teams[name] = team
		}
	}
	return out.Teams, nil
}

// UpsertTeam creates or updates the org team via
// PUT /api/v1/organization/{orgname}/team/{teamname} with the given role
// (member, creator, or admin) and description. Quay's PUT is create-or-update,
// so this is naturally idempotent across reconciler re-runs — no *IfNotExists
// wrapper is needed.
func (c *Client) UpsertTeam(ctx context.Context, org, team, role, description string) error {
	req := upsertTeamRequest{Role: role, Description: description}
	return c.doJSON(ctx, http.MethodPut, teamPath(org, team), req, nil)
}

// GetTeamMembers fetches the team-members payload via
// GET /api/v1/organization/{orgname}/team/{teamname}/members, returning the sync
// binding (the bound OIDC group via TeamMembers.GroupName) the reconciler uses
// to detect drift and ownership. A missing team is returned as an *APIError
// reporting IsNotFound.
func (c *Client) GetTeamMembers(ctx context.Context, org, team string) (*TeamMembers, error) {
	out := &TeamMembers{}
	if err := c.doJSON(ctx, http.MethodGet, teamPath(org, team)+"/members", nil, out); err != nil {
		return nil, err
	}
	if out.Name == "" {
		out.Name = team
	}
	return out, nil
}

// EnableTeamSync binds the org team's membership to the OIDC group oidcGroup via
// POST /api/v1/organization/{orgname}/team/{teamname}/syncing with body
// {"group_name": "<oidcGroup>"}. See enableTeamSyncRequest for why group_name is
// the verified key for the OIDC backend.
//
// Quay rejects enabling sync on a team that is already synced; that
// already-bound response is mapped to an *APIError reporting IsConflict so a
// reconciler can treat a re-run as benign (use EnableTeamSyncIfNotSynced).
func (c *Client) EnableTeamSync(ctx context.Context, org, team, oidcGroup string) error {
	req := enableTeamSyncRequest{GroupName: oidcGroup}
	err := c.doJSON(ctx, http.MethodPost, teamPath(org, team)+"/syncing", req, nil)
	return mapDuplicateToConflict(err)
}

// EnableTeamSyncIfNotSynced enables sync, but no-ops when the team is already
// synced to the requested oidcGroup, so the call is idempotent across reconciler
// re-runs without masking drift.
//
// Quay's syncing endpoint is not idempotent: the TeamSync row has a unique
// constraint on the team, so a second POST for an already-synced team fails with
// a database integrity error rather than a clean conflict, and Quay 3.17.3 maps
// that DataModelException to no well-defined wire status. Rather than depend on
// that ambiguous shape, this checks the current binding via GetTeamMembers
// first: if the team is already synced to oidcGroup the call is a no-op success;
// if it is synced to a different group the EnableTeamSync error surfaces so the
// reconciler corrects the drift (e.g. DisableTeamSync then re-enable); only when
// the team is not yet synced does it POST. A failure reading the current binding
// surfaces rather than risking a duplicate POST.
func (c *Client) EnableTeamSyncIfNotSynced(ctx context.Context, org, team, oidcGroup string) error {
	members, err := c.GetTeamMembers(ctx, org, team)
	if err != nil {
		return err
	}
	if members.Synced != nil {
		if members.GroupName() == oidcGroup {
			return nil
		}
		// Already synced to a different group; surface the conflict from a real
		// enable attempt so the reconciler treats it as drift to correct rather
		// than reconciled.
		return c.EnableTeamSync(ctx, org, team, oidcGroup)
	}
	return c.EnableTeamSync(ctx, org, team, oidcGroup)
}

// DisableTeamSync removes the org team's sync binding via
// DELETE /api/v1/organization/{orgname}/team/{teamname}/syncing. A team that is
// not synced is returned as an *APIError reporting IsNotFound; use
// DisableTeamSyncIfSynced to treat that as success.
func (c *Client) DisableTeamSync(ctx context.Context, org, team string) error {
	return c.doJSON(ctx, http.MethodDelete, teamPath(org, team)+"/syncing", nil, nil)
}

// DisableTeamSyncIfSynced disables sync and returns nil when the team is already
// not synced, so the call is idempotent.
func (c *Client) DisableTeamSyncIfSynced(ctx context.Context, org, team string) error {
	err := c.DisableTeamSync(ctx, org, team)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteTeam deletes the org team via
// DELETE /api/v1/organization/{orgname}/team/{teamname}. A missing team is
// returned as an *APIError reporting IsNotFound; use DeleteTeamIfExists to treat
// that as success.
func (c *Client) DeleteTeam(ctx context.Context, org, team string) error {
	return c.doJSON(ctx, http.MethodDelete, teamPath(org, team), nil, nil)
}

// DeleteTeamIfExists deletes the org team and returns nil when it is already
// absent, so the call is idempotent for cleanup and finalizer logic.
//
// Quay's delete-team path raises InvalidTeamException for a team that does not
// exist, which Quay 3.17.3 surfaces as a DataModelException with no well-defined
// wire status (commonly a 400, not a clean 404). So a plain IsNotFound check is
// not enough: isAbsentTeam additionally recognizes the absent-team 400, and for
// any other non-2xx the method confirms absence via ListTeams and treats a
// missing team as success — so a benign already-deleted team does not fail
// cleanup while a genuine error (or a team that still exists) still surfaces.
func (c *Client) DeleteTeamIfExists(ctx context.Context, org, team string) error {
	err := c.DeleteTeam(ctx, org, team)
	if err == nil || IsNotFound(err) || isAbsentTeam(err) {
		return nil
	}
	// The delete failed for some other reason; if the team is nonetheless absent
	// the failure was a benign already-deleted (a Quay error shape we did not
	// recognize), so succeed. If the existence check itself fails, return the
	// original delete error.
	teams, listErr := c.ListTeams(ctx, org)
	if listErr != nil {
		return err
	}
	if _, present := teams[team]; !present {
		return nil
	}
	return err
}

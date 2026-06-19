package quay

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestUpsertTeam(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme/team/devs", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.UpsertTeam(context.Background(), "acme", "devs", TeamRoleMember, "the dev team"); err != nil {
		t.Fatalf("UpsertTeam: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["role"] != "member" {
		t.Errorf("body role = %v, want member", h.gotBody["role"])
	}
	if h.gotBody["description"] != "the dev team" {
		t.Errorf("body description = %v, want the dev team", h.gotBody["description"])
	}
}

func TestUpsertTeamSendsEmptyDescription(t *testing.T) {
	// Quay only changes the description when the key is present, so an empty
	// desired description must still be sent (as "") to clear a prior value —
	// otherwise a reconciled team stays permanently drifted.
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme/team/devs", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.UpsertTeam(context.Background(), "acme", "devs", TeamRoleAdmin, ""); err != nil {
		t.Fatalf("UpsertTeam: %v", err)
	}
	assertCommonRequest(t, h, true)
	desc, ok := h.gotBody["description"]
	if !ok {
		t.Error("description key must be present even when empty, so Quay clears a prior value")
	}
	if desc != "" {
		t.Errorf("body description = %v, want empty string", desc)
	}
	if h.gotBody["role"] != "admin" {
		t.Errorf("body role = %v, want admin", h.gotBody["role"])
	}
}

func TestListTeams(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme",
		status:   http.StatusOK,
		respBody: `{"name":"acme","teams":{"devs":{"name":"devs","role":"member","description":"d","is_synced":true},"ops":{"role":"admin","is_synced":false}}}`,
	}
	c, _ := newTestClient(t, h)

	teams, err := c.ListTeams(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	assertCommonRequest(t, h, false)
	if len(teams) != 2 {
		t.Fatalf("teams len = %d, want 2: %+v", len(teams), teams)
	}
	devs := teams["devs"]
	if devs.Name != "devs" || devs.Role != "member" || !devs.IsSynced {
		t.Errorf("devs decoded = %+v", devs)
	}
	// Name backfilled from the map key when Quay omits it inside the entry.
	ops := teams["ops"]
	if ops.Name != "ops" || ops.Role != "admin" || ops.IsSynced {
		t.Errorf("ops decoded = %+v (Name should backfill from key)", ops)
	}
}

func TestListTeamsNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/missing",
		status:   http.StatusNotFound,
		respBody: `{"error_message":"Not Found"}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.ListTeams(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestGetTeamMembersSynced(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme/team/devs/members",
		status:   http.StatusOK,
		respBody: `{"name":"devs","synced":{"service":"oidc","last_updated":"now","config":{"group_name":"platform-devs"}}}`,
	}
	c, _ := newTestClient(t, h)

	m, err := c.GetTeamMembers(context.Background(), "acme", "devs")
	if err != nil {
		t.Fatalf("GetTeamMembers: %v", err)
	}
	assertCommonRequest(t, h, false)
	if m.Name != "devs" {
		t.Errorf("name = %q, want devs", m.Name)
	}
	if m.Synced == nil || m.Synced.Service != "oidc" {
		t.Fatalf("synced = %+v, want service oidc", m.Synced)
	}
	if got := m.GroupName(); got != "platform-devs" {
		t.Errorf("GroupName() = %q, want platform-devs", got)
	}
}

func TestGetTeamMembersNotSynced(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme/team/devs/members",
		status:   http.StatusOK,
		respBody: `{"name":"devs","can_sync":{"service":"oidc"}}`,
	}
	c, _ := newTestClient(t, h)

	m, err := c.GetTeamMembers(context.Background(), "acme", "devs")
	if err != nil {
		t.Fatalf("GetTeamMembers: %v", err)
	}
	if m.Synced != nil {
		t.Errorf("synced should be nil for an unsynced team, got %+v", m.Synced)
	}
	if got := m.GroupName(); got != "" {
		t.Errorf("GroupName() = %q, want empty for an unsynced team", got)
	}
	if m.CanSync["service"] != "oidc" {
		t.Errorf("can_sync = %+v, want service oidc", m.CanSync)
	}
}

func TestGetTeamMembersBackfillsName(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme/team/devs/members",
		status:   http.StatusOK,
		respBody: `{}`,
	}
	c, _ := newTestClient(t, h)

	m, err := c.GetTeamMembers(context.Background(), "acme", "devs")
	if err != nil {
		t.Fatalf("GetTeamMembers: %v", err)
	}
	if m.Name != "devs" {
		t.Errorf("name = %q, want backfilled devs", m.Name)
	}
}

func TestGroupNameNilReceiver(t *testing.T) {
	var m *TeamMembers
	if got := m.GroupName(); got != "" {
		t.Errorf("GroupName() on nil = %q, want empty", got)
	}
}

func TestEnableTeamSync(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/organization/acme/team/devs/syncing", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.EnableTeamSync(context.Background(), "acme", "devs", "platform-devs"); err != nil {
		t.Fatalf("EnableTeamSync: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["group_name"] != "platform-devs" {
		t.Errorf("body group_name = %v, want platform-devs", h.gotBody["group_name"])
	}
}

func TestEnableTeamSyncAlreadySyncedIsConflict(t *testing.T) {
	// Quay rejects enabling sync on a team that is already synced; here as a
	// 400 with a duplicate-style message, which must map to a conflict.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/organization/acme/team/devs/syncing",
		status:   http.StatusBadRequest,
		respBody: `{"message":"Team is already in use"}`,
	}
	c, _ := newTestClient(t, h)

	err := c.EnableTeamSync(context.Background(), "acme", "devs", "platform-devs")
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict for already-synced team, got %v", err)
	}
}

func TestEnableTeamSyncIfNotSyncedSameGroupSucceeds(t *testing.T) {
	// Already synced to the requested group: the conflict is verified via a
	// members GET and swallowed as a benign no-op.
	getHit := false
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/organization/acme/team/devs/syncing": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error_message":"already synced"}`)
		},
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			getHit = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"name":"devs","synced":{"service":"oidc","config":{"group_name":"platform-devs"}}}`)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs"); err != nil {
		t.Fatalf("EnableTeamSyncIfNotSynced should succeed when already synced to the same group, got %v", err)
	}
	if !getHit {
		t.Error("expected a confirming GET members after the conflict")
	}
}

func TestEnableTeamSyncIfNotSyncedWrongGroupSurfacesConflict(t *testing.T) {
	// Already synced to a DIFFERENT group: the conflict must surface so the
	// reconciler corrects the drift, never silently reporting success.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/organization/acme/team/devs/syncing": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error_message":"already synced"}`)
		},
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"name":"devs","synced":{"service":"oidc","config":{"group_name":"some-other-group"}}}`)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs")
	if !IsConflict(err) {
		t.Fatalf("expected the conflict to surface for a wrong-group binding, got %v", err)
	}
}

func TestEnableTeamSyncIfNotSyncedSurfacesConflictWhenMembersUnreadable(t *testing.T) {
	// The conflict cannot be confirmed against the existing binding (members
	// GET fails), so the original conflict must surface rather than be assumed
	// to match.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/organization/acme/team/devs/syncing": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error_message":"already synced"}`)
		},
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error_message":"boom"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs")
	if !IsConflict(err) {
		t.Fatalf("expected the original conflict to surface when members is unreadable, got %v", err)
	}
}

func TestEnableTeamSyncIfNotSyncedFirstEnableSucceeds(t *testing.T) {
	// Not yet synced: enabling succeeds outright, no members GET needed.
	h := &recordingHandler{t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/organization/acme/team/devs/syncing", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs"); err != nil {
		t.Fatalf("EnableTeamSyncIfNotSynced should succeed on a fresh enable, got %v", err)
	}
	if h.gotBody["group_name"] != "platform-devs" {
		t.Errorf("body group_name = %v, want platform-devs", h.gotBody["group_name"])
	}
}

func TestDisableTeamSync(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/devs/syncing", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.DisableTeamSync(context.Background(), "acme", "devs"); err != nil {
		t.Fatalf("DisableTeamSync: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDisableTeamSyncIfSyncedSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/devs/syncing", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DisableTeamSyncIfSynced(context.Background(), "acme", "devs"); err != nil {
		t.Fatalf("DisableTeamSyncIfSynced should swallow 404, got %v", err)
	}
}

func TestDeleteTeam(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/devs", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteTeam(context.Background(), "acme", "devs"); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeleteTeamIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeleteTeamIfExists(context.Background(), "acme", "gone"); err != nil {
		t.Fatalf("DeleteTeamIfExists should swallow 404, got %v", err)
	}
}

func TestTeamPathEscaping(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/a b/team/c d", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteTeam(context.Background(), "a b", "c d"); err != nil {
		t.Fatalf("DeleteTeam with spaces: %v", err)
	}
	if h.gotEscaped != "/api/v1/organization/a%20b/team/c%20d" {
		t.Errorf("escaped path = %q, want both spaces percent-encoded", h.gotEscaped)
	}
}

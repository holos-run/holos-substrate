package quay

import (
	"context"
	"encoding/json"
	"errors"
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

func TestEnableTeamSyncIfNotSyncedSameGroupNoOps(t *testing.T) {
	// Already synced to the requested group: a members GET confirms it and the
	// call is a no-op success — no POST is issued (the syncing endpoint is not
	// idempotent, so re-POSTing would fail the unique TeamSync constraint).
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"name":"devs","synced":{"service":"oidc","config":{"group_name":"platform-devs"}}}`)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs"); err != nil {
		t.Fatalf("EnableTeamSyncIfNotSynced should no-op when already synced to the same group, got %v", err)
	}
}

func TestEnableTeamSyncIfNotSyncedWrongGroupSurfacesError(t *testing.T) {
	// Already synced to a DIFFERENT group: it must attempt a real enable so the
	// resulting error surfaces as drift the reconciler corrects, never silently
	// reporting success.
	postHit := false
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"name":"devs","synced":{"service":"oidc","config":{"group_name":"some-other-group"}}}`)
		},
		"POST /api/v1/organization/acme/team/devs/syncing": func(w http.ResponseWriter, _ *http.Request) {
			postHit = true
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error_message":"already synced"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs")
	if err == nil {
		t.Fatal("expected an error for a wrong-group binding, got nil")
	}
	if !postHit {
		t.Error("expected a real enable attempt for a wrong-group binding")
	}
}

func TestEnableTeamSyncIfNotSyncedSurfacesMembersReadError(t *testing.T) {
	// The current binding cannot be read; the read error surfaces rather than
	// risking a duplicate POST against the unique TeamSync constraint.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error_message":"boom"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs")
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected the members read error to surface, got %v", err)
	}
}

func TestEnableTeamSyncIfNotSyncedFirstEnableSucceeds(t *testing.T) {
	// Not yet synced: a members GET shows no binding, then the enable POST is
	// issued and succeeds.
	postHit := false
	var gotGroup any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/organization/acme/team/devs/members": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"name":"devs","can_sync":{"service":"oidc"}}`)
		},
		"POST /api/v1/organization/acme/team/devs/syncing": func(w http.ResponseWriter, r *http.Request) {
			postHit = true
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			gotGroup = parsed["group_name"]
			w.WriteHeader(http.StatusOK)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnableTeamSyncIfNotSynced(context.Background(), "acme", "devs", "platform-devs"); err != nil {
		t.Fatalf("EnableTeamSyncIfNotSynced should succeed on a fresh enable, got %v", err)
	}
	if !postHit {
		t.Error("expected an enable POST when the team is not yet synced")
	}
	if gotGroup != "platform-devs" {
		t.Errorf("POST body group_name = %v, want platform-devs", gotGroup)
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

func TestDeleteTeamIfExistsSwallowsAbsentTeam400(t *testing.T) {
	// Quay's remove_team raises InvalidTeamException for a missing team, which
	// can surface as a 400 (not 404); the if-exists wrapper must treat it as
	// gone by message, without a confirming GET.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/gone",
		status:   http.StatusBadRequest,
		respBody: `{"error_message":"Team 'gone' is not a team in org 'acme'"}`,
	}
	c, _ := newTestClient(t, h)

	if err := c.DeleteTeamIfExists(context.Background(), "acme", "gone"); err != nil {
		t.Fatalf("DeleteTeamIfExists should swallow an absent-team 400, got %v", err)
	}
}

func TestDeleteTeamIfExistsSurfacesOtherError(t *testing.T) {
	// Only the unambiguous absent signals (404 / absent-team 400) are swallowed;
	// every other error — including a 500 or an auth failure — must surface so a
	// real failure is never silently reported as a successful cleanup.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/devs",
		status:   http.StatusInternalServerError,
		respBody: `{"error_message":"boom"}`,
	}
	c, _ := newTestClient(t, h)

	err := c.DeleteTeamIfExists(context.Background(), "acme", "devs")
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected the 500 delete error to surface, got %v", err)
	}
}

func TestDeleteTeamIfExistsSurfacesUnrelated400(t *testing.T) {
	// A 400 that is not an absent-team signal must still surface.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/team/devs",
		status:   http.StatusBadRequest,
		respBody: `{"error_message":"some other validation failure"}`,
	}
	c, _ := newTestClient(t, h)

	if err := c.DeleteTeamIfExists(context.Background(), "acme", "devs"); err == nil {
		t.Fatal("expected an unrelated 400 to surface, got nil")
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

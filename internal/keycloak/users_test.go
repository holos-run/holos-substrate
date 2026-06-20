package keycloak

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

const usersBase = adminPathPrefix + "/realms/holos/users"

func TestFindUserByEmailFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: usersBase,
		status:   http.StatusOK,
		respBody: `[{"id":"u1","username":"alice","email":"alice@example.com"}]`,
	}
	c, _ := newTestClient(t, h)

	u, err := c.FindUserByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("FindUserByEmail: %v", err)
	}
	if u == nil || u.ID != "u1" || u.Username != "alice" {
		t.Errorf("decoded user = %+v", u)
	}
	if !strings.Contains(h.gotQuery, "exact=true") || !strings.Contains(h.gotQuery, "email=alice") {
		t.Errorf("query = %q, want exact email match", h.gotQuery)
	}
}

func TestFindUserByEmailNotFoundIsNilNotError(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodGet, wantPath: usersBase, status: http.StatusOK, respBody: `[]`}
	c, _ := newTestClient(t, h)

	u, err := c.FindUserByEmail(context.Background(), "nobody@example.com")
	if err != nil {
		t.Fatalf("an empty result must not be an error, got %v", err)
	}
	if u != nil {
		t.Errorf("expected nil user for an empty result, got %+v", u)
	}
}

func TestFindUserByUsername(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: usersBase,
		status:   http.StatusOK,
		respBody: `[{"id":"u2","username":"bob"}]`,
	}
	c, _ := newTestClient(t, h)

	u, err := c.FindUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatalf("FindUserByUsername: %v", err)
	}
	if u == nil || u.ID != "u2" {
		t.Errorf("decoded user = %+v", u)
	}
	if !strings.Contains(h.gotQuery, "username=bob") {
		t.Errorf("query = %q, want username=bob", h.gotQuery)
	}
}

func TestCreateUser(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: usersBase,
		status:   http.StatusCreated,
		location: "https://kc/admin/realms/holos/users/u-new",
	}
	c, _ := newTestClient(t, h)

	id, err := c.CreateUser(context.Background(), User{Email: "carol@example.com", Username: "carol", Enabled: true, EmailVerified: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id != "u-new" {
		t.Errorf("id = %q, want u-new (from Location)", id)
	}
	if h.gotBody["email"] != "carol@example.com" || h.gotBody["username"] != "carol" {
		t.Errorf("body = %+v", h.gotBody)
	}
	if h.gotBody["emailVerified"] != true {
		t.Errorf("emailVerified = %v, want true", h.gotBody["emailVerified"])
	}
}

func TestCreateUserConflict(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: usersBase,
		status:   http.StatusConflict,
		respBody: `{"errorMessage":"User exists with same email"}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.CreateUser(context.Background(), User{Email: "dup@example.com"})
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict for duplicate user, got %v", err)
	}
}

func TestAddUserToGroup(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: usersBase + "/u1/groups/g1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.AddUserToGroup(context.Background(), "u1", "g1"); err != nil {
		t.Fatalf("AddUserToGroup: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestRemoveUserFromGroup(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: usersBase + "/u1/groups/g1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.RemoveUserFromGroup(context.Background(), "u1", "g1"); err != nil {
		t.Fatalf("RemoveUserFromGroup: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestRemoveUserFromGroupIfMemberSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: usersBase + "/u1/groups/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.RemoveUserFromGroupIfMember(context.Background(), "u1", "gone"); err != nil {
		t.Fatalf("RemoveUserFromGroupIfMember should swallow 404, got %v", err)
	}
}

func TestCreateFederatedIdentity(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: usersBase + "/u1/federated-identity/oidc",
		status: http.StatusCreated,
	}
	c, _ := newTestClient(t, h)

	link := FederatedIdentity{IdentityProvider: "oidc", UserID: "sub-123", UserName: "alice"}
	if err := c.CreateFederatedIdentity(context.Background(), "u1", "oidc", link); err != nil {
		t.Fatalf("CreateFederatedIdentity: %v", err)
	}
	if h.gotBody["identityProvider"] != "oidc" || h.gotBody["userId"] != "sub-123" || h.gotBody["userName"] != "alice" {
		t.Errorf("body = %+v", h.gotBody)
	}
}

func TestCreateFederatedIdentityIfNotExistsSwallowsMatchingConflict(t *testing.T) {
	// The 409 is swallowed only after a GET confirms the existing link points at
	// the SAME upstream userId — the desired state already holds.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST " + usersBase + "/u1/federated-identity/oidc": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"errorMessage":"User is already linked"}`)
		},
		"GET " + usersBase + "/u1/federated-identity": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"identityProvider":"oidc","userId":"sub-123","userName":"alice"}]`)
		},
	}}
	c, _ := newTestClient(t, m)

	link := FederatedIdentity{IdentityProvider: "oidc", UserID: "sub-123", UserName: "alice"}
	if err := c.CreateFederatedIdentityIfNotExists(context.Background(), "u1", "oidc", link); err != nil {
		t.Fatalf("CreateFederatedIdentityIfNotExists should swallow a matching 409, got %v", err)
	}
}

func TestCreateFederatedIdentityIfNotExistsSurfacesMismatchConflict(t *testing.T) {
	// The provider is already bound to a DIFFERENT upstream userId: the conflict
	// must surface, not be swallowed, so a mis-link is never reported as success.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST " + usersBase + "/u1/federated-identity/oidc": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"errorMessage":"User is already linked"}`)
		},
		"GET " + usersBase + "/u1/federated-identity": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"identityProvider":"oidc","userId":"DIFFERENT-sub","userName":"alice"}]`)
		},
	}}
	c, _ := newTestClient(t, m)

	link := FederatedIdentity{IdentityProvider: "oidc", UserID: "sub-123", UserName: "alice"}
	err := c.CreateFederatedIdentityIfNotExists(context.Background(), "u1", "oidc", link)
	if !IsConflict(err) {
		t.Fatalf("expected the mismatch conflict to surface, got %v", err)
	}
}

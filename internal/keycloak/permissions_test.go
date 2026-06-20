package keycloak

import (
	"context"
	"net/http"
	"testing"
)

const authzBase = adminPathPrefix + "/realms/holos/clients/perm-uuid/authz/resource-server"

func TestCreateGroupResource(t *testing.T) {
	// The authz resource endpoint returns 201 with the ResourceRepresentation
	// (carrying _id) in the BODY, not a Location header; the helper must read it
	// from the body.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: authzBase + "/resource",
		status:   http.StatusCreated,
		respBody: `{"_id":"res-1","name":"/projects/my-project/roles/owner","type":"Groups"}`,
	}
	c, _ := newTestClient(t, h)

	res := AuthzResource{
		Name:   "/projects/my-project/roles/owner",
		Scopes: []AuthzScope{{Name: ScopeManageMembers}, {Name: ScopeManageMembership}},
	}
	id, err := c.CreateGroupResource(context.Background(), "perm-uuid", res)
	if err != nil {
		t.Fatalf("CreateGroupResource: %v", err)
	}
	if id != "res-1" {
		t.Errorf("id = %q, want res-1 (decoded from the body _id)", id)
	}
	if h.gotBody["type"] != "Groups" {
		t.Errorf("type = %v, want defaulted Groups", h.gotBody["type"])
	}
	if h.gotBody["name"] != "/projects/my-project/roles/owner" {
		t.Errorf("name = %v", h.gotBody["name"])
	}
}

func TestCreateGroupPolicyDefaultsType(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: authzBase + "/policy/group",
		status:   http.StatusCreated,
		respBody: `{"id":"pol-1","name":"custodians-owner","type":"group"}`,
	}
	c, _ := newTestClient(t, h)

	pol := GroupPolicy{Name: "custodians-owner", Groups: []GroupPolicyMember{{ID: "g-cust-owner"}}}
	id, err := c.CreateGroupPolicy(context.Background(), "perm-uuid", pol)
	if err != nil {
		t.Fatalf("CreateGroupPolicy: %v", err)
	}
	if id != "pol-1" {
		t.Errorf("id = %q, want pol-1", id)
	}
	if h.gotBody["type"] != "group" {
		t.Errorf("type = %v, want defaulted group", h.gotBody["type"])
	}
}

func TestCreateScopePermission(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: authzBase + "/permission/scope",
		status:   http.StatusCreated,
		respBody: `{"id":"perm-1","name":"custodian-owner-manages-roles-owner"}`,
	}
	c, _ := newTestClient(t, h)

	perm := ScopePermission{
		Name:      "custodian-owner-manages-roles-owner",
		Resources: []string{"res-1"},
		Scopes:    []string{ScopeManageMembers, ScopeManageMembership},
		Policies:  []string{"pol-1"},
	}
	id, err := c.CreateScopePermission(context.Background(), "perm-uuid", perm)
	if err != nil {
		t.Fatalf("CreateScopePermission: %v", err)
	}
	if id != "perm-1" {
		t.Errorf("id = %q, want perm-1", id)
	}
	if h.gotBody["name"] != "custodian-owner-manages-roles-owner" {
		t.Errorf("name = %v", h.gotBody["name"])
	}
}

func TestCreateScopePermissionConflict(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: authzBase + "/permission/scope",
		status:   http.StatusConflict,
		respBody: `{"error":"Conflicting policy"}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.CreateScopePermission(context.Background(), "perm-uuid", ScopePermission{Name: "dup"})
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict, got %v", err)
	}
}

func TestDeleteScopePermission(t *testing.T) {
	// A scope permission is a policy in Keycloak's Authorization Services, so it
	// is deleted via the generic /policy/{id} endpoint.
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: authzBase + "/policy/perm-1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteScopePermission(context.Background(), "perm-uuid", "perm-1"); err != nil {
		t.Fatalf("DeleteScopePermission: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeleteScopePermissionIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: authzBase + "/policy/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeleteScopePermissionIfExists(context.Background(), "perm-uuid", "gone"); err != nil {
		t.Fatalf("DeleteScopePermissionIfExists should swallow 404, got %v", err)
	}
}

func TestFindPolicyByName(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: authzBase + "/policy",
		status:   http.StatusOK,
		respBody: `[{"id":"pol-1","name":"holos:custodian:a"},{"id":"pol-2","name":"holos:custodian:ab"}]`,
	}
	c, _ := newTestClient(t, h)

	id, err := c.FindPolicyByName(context.Background(), "perm-uuid", "holos:custodian:a")
	if err != nil {
		t.Fatalf("FindPolicyByName: %v", err)
	}
	if id != "pol-1" {
		t.Errorf("id = %q, want pol-1 (exact-name match, not the substring sibling)", id)
	}
	if h.gotQuery != "name=holos%3Acustodian%3Aa" {
		t.Errorf("query = %q, want the name filter", h.gotQuery)
	}
}

func TestFindPermissionByNameNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: authzBase + "/permission",
		status: http.StatusOK, respBody: `[]`,
	}
	c, _ := newTestClient(t, h)

	id, err := c.FindPermissionByName(context.Background(), "perm-uuid", "holos:custodian-perm:x")
	if err != nil {
		t.Fatalf("FindPermissionByName: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty for no match", id)
	}
}

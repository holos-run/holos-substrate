package keycloak

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

const clientsBase = adminPathPrefix + "/realms/holos/clients"

func TestFindClientByClientIDFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: clientsBase,
		status:   http.StatusOK,
		respBody: `[{"id":"uuid-1","clientId":"https://quay.holos.localhost","publicClient":false}]`,
	}
	c, _ := newTestClient(t, h)

	cl, err := c.FindClientByClientID(context.Background(), "https://quay.holos.localhost")
	if err != nil {
		t.Fatalf("FindClientByClientID: %v", err)
	}
	if cl == nil || cl.ID != "uuid-1" {
		t.Errorf("decoded client = %+v", cl)
	}
	if !strings.Contains(h.gotQuery, "clientId=https") {
		t.Errorf("query = %q, want clientId filter", h.gotQuery)
	}
}

func TestFindClientByClientIDNotFoundIsNil(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodGet, wantPath: clientsBase, status: http.StatusOK, respBody: `[]`}
	c, _ := newTestClient(t, h)

	cl, err := c.FindClientByClientID(context.Background(), "https://absent")
	if err != nil {
		t.Fatalf("empty result must not be an error, got %v", err)
	}
	if cl != nil {
		t.Errorf("expected nil for empty result, got %+v", cl)
	}
}

func TestCreateClient(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: clientsBase,
		status:   http.StatusCreated,
		location: "https://kc/admin/realms/holos/clients/uuid-new",
	}
	c, _ := newTestClient(t, h)

	id, err := c.CreateClient(context.Background(), OIDCClient{ClientID: "https://app", PublicClient: true, RedirectURIs: []string{"https://app/cb"}})
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if id != "uuid-new" {
		t.Errorf("id = %q, want uuid-new", id)
	}
	if h.gotBody["clientId"] != "https://app" {
		t.Errorf("body clientId = %v", h.gotBody["clientId"])
	}
}

func TestUpdateClient(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: clientsBase + "/uuid-1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.UpdateClient(context.Background(), "uuid-1", OIDCClient{ClientID: "https://app", Name: "App"}); err != nil {
		t.Fatalf("UpdateClient: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["name"] != "App" {
		t.Errorf("body name = %v, want App", h.gotBody["name"])
	}
}

func TestListProtocolMappers(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: clientsBase + "/uuid-1/protocol-mappers/models",
		status:   http.StatusOK,
		respBody: `[{"id":"m1","name":"quay-client-roles","protocolMapper":"oidc-usermodel-client-role-mapper"}]`,
	}
	c, _ := newTestClient(t, h)

	mappers, err := c.ListProtocolMappers(context.Background(), "uuid-1")
	if err != nil {
		t.Fatalf("ListProtocolMappers: %v", err)
	}
	if len(mappers) != 1 || mappers[0].Name != "quay-client-roles" {
		t.Errorf("decoded mappers = %+v", mappers)
	}
}

func TestEnsureClientRoleMapperNoOpsWhenPresent(t *testing.T) {
	postHit := false
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"id":"m1","name":"project-roles"}]`)
		},
		"POST " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			postHit = true
			w.WriteHeader(http.StatusCreated)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnsureClientRoleMapper(context.Background(), "uuid-1", "project-roles", "https://app", "groups"); err != nil {
		t.Fatalf("EnsureClientRoleMapper: %v", err)
	}
	if postHit {
		t.Error("no POST should be issued when the mapper already exists")
	}
}

func TestEnsureClientRoleMapperCreatesWhenAbsent(t *testing.T) {
	var gotConfig map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[]`)
		},
		"POST " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			gotConfig = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusCreated)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnsureClientRoleMapper(context.Background(), "uuid-1", "project-roles", "https://app", "groups"); err != nil {
		t.Fatalf("EnsureClientRoleMapper: %v", err)
	}
	if gotConfig["protocolMapper"] != ProtocolMapperClientRole {
		t.Errorf("protocolMapper = %v, want %s", gotConfig["protocolMapper"], ProtocolMapperClientRole)
	}
	cfg, ok := gotConfig["config"].(map[string]any)
	if !ok {
		t.Fatalf("config not an object: %v", gotConfig["config"])
	}
	if cfg["usermodel.clientRoleMapping.clientId"] != "https://app" {
		t.Errorf("clientRoleMapping.clientId = %v, want https://app", cfg["usermodel.clientRoleMapping.clientId"])
	}
	if cfg["claim.name"] != "groups" {
		t.Errorf("claim.name = %v, want groups", cfg["claim.name"])
	}
}

func TestEnsureClientRoleMapperToleratesConflict(t *testing.T) {
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[]`)
		},
		"POST " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"errorMessage":"already exists"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnsureClientRoleMapper(context.Background(), "uuid-1", "project-roles", "https://app", "groups"); err != nil {
		t.Fatalf("EnsureClientRoleMapper should tolerate a 409, got %v", err)
	}
}

func TestListClientRoles(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: clientsBase + "/uuid-1/roles",
		status:   http.StatusOK,
		respBody: `[{"id":"r1","name":"my-project-owner"},{"id":"r2","name":"my-project-editor"}]`,
	}
	c, _ := newTestClient(t, h)

	roles, err := c.ListClientRoles(context.Background(), "uuid-1")
	if err != nil {
		t.Fatalf("ListClientRoles: %v", err)
	}
	if len(roles) != 2 || roles[0].Name != "my-project-owner" {
		t.Errorf("decoded roles = %+v", roles)
	}
}

func TestGetClientRole(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: clientsBase + "/uuid-1/roles/my-project-owner",
		status:   http.StatusOK,
		respBody: `{"id":"r1","name":"my-project-owner","clientRole":true}`,
	}
	c, _ := newTestClient(t, h)

	role, err := c.GetClientRole(context.Background(), "uuid-1", "my-project-owner")
	if err != nil {
		t.Fatalf("GetClientRole: %v", err)
	}
	if role.ID != "r1" || !role.ClientRole {
		t.Errorf("decoded role = %+v", role)
	}
}

func TestGetClientRoleNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: clientsBase + "/uuid-1/roles/missing",
		status:   http.StatusNotFound,
		respBody: `{"error":"Could not find role"}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.GetClientRole(context.Background(), "uuid-1", "missing"); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestCreateClientRole(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPost, wantPath: clientsBase + "/uuid-1/roles", status: http.StatusCreated}
	c, _ := newTestClient(t, h)

	if err := c.CreateClientRole(context.Background(), "uuid-1", ClientRole{Name: "my-project-owner", Description: "owner"}); err != nil {
		t.Fatalf("CreateClientRole: %v", err)
	}
	if h.gotBody["name"] != "my-project-owner" {
		t.Errorf("body name = %v, want my-project-owner", h.gotBody["name"])
	}
}

func TestCreateClientRoleIfNotExistsSwallowsConflict(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: clientsBase + "/uuid-1/roles",
		status:   http.StatusConflict,
		respBody: `{"errorMessage":"Role with name my-project-owner already exists"}`,
	}
	c, _ := newTestClient(t, h)

	if err := c.CreateClientRoleIfNotExists(context.Background(), "uuid-1", ClientRole{Name: "my-project-owner"}); err != nil {
		t.Fatalf("CreateClientRoleIfNotExists should swallow 409, got %v", err)
	}
}

func TestAssignClientRoleToGroup(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost,
		wantPath: adminPathPrefix + "/realms/holos/groups/g-owner/role-mappings/clients/uuid-1",
		status:   http.StatusNoContent,
	}
	c, _ := newTestClient(t, h)

	role := ClientRole{ID: "r1", Name: "my-project-owner"}
	if err := c.AssignClientRoleToGroup(context.Background(), "g-owner", "uuid-1", role); err != nil {
		t.Fatalf("AssignClientRoleToGroup: %v", err)
	}
	// Body is a single-element array of role representations.
	if len(h.gotArray) != 1 {
		t.Fatalf("body should be a one-element array, got %v", h.gotArray)
	}
	first, ok := h.gotArray[0].(map[string]any)
	if !ok || first["id"] != "r1" || first["name"] != "my-project-owner" {
		t.Errorf("array[0] = %v, want the role with id/name", h.gotArray[0])
	}
}

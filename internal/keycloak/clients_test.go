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
		respBody: `[{"id":"uuid-1","clientId":"https://quay.holos.internal","publicClient":false}]`,
	}
	c, _ := newTestClient(t, h)

	cl, err := c.FindClientByClientID(context.Background(), "https://quay.holos.internal")
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

func TestUpdateClientFieldsPreservesUnmanagedFields(t *testing.T) {
	// The lossless update path fetches the full representation, overwrites only
	// the set managed fields, and PUTs it back — unmanaged fields (protocol,
	// attributes/PKCE, service-account flags) must survive the round-trip.
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://app","name":"Old","enabled":true,"publicClient":false,"protocol":"openid-connect","clientAuthenticatorType":"client-secret","serviceAccountsEnabled":true,"attributes":{"pkce.code.challenge.method":"S256"}}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	newName := "App"
	if err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{Name: &newName}); err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	if putBody["name"] != "App" {
		t.Errorf("name = %v, want the managed override App", putBody["name"])
	}
	// Unmanaged fields must be preserved verbatim.
	if putBody["protocol"] != "openid-connect" {
		t.Errorf("protocol = %v, want preserved openid-connect", putBody["protocol"])
	}
	if putBody["clientAuthenticatorType"] != "client-secret" {
		t.Errorf("clientAuthenticatorType = %v, want preserved", putBody["clientAuthenticatorType"])
	}
	if putBody["serviceAccountsEnabled"] != true {
		t.Errorf("serviceAccountsEnabled = %v, want preserved true", putBody["serviceAccountsEnabled"])
	}
	attrs, ok := putBody["attributes"].(map[string]any)
	if !ok || attrs["pkce.code.challenge.method"] != "S256" {
		t.Errorf("attributes = %v, want preserved PKCE config", putBody["attributes"])
	}
}

func TestUpdateClientFieldsOnlyTouchesSetFields(t *testing.T) {
	// A nil field must not appear as an override; the fetched value stays.
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://app","enabled":true,"publicClient":true}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	// Only flip publicClient to false (converge public -> confidential); leave
	// enabled untouched.
	confidential := false
	if err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{PublicClient: &confidential}); err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	if putBody["publicClient"] != false {
		t.Errorf("publicClient = %v, want overridden false", putBody["publicClient"])
	}
	if putBody["enabled"] != true {
		t.Errorf("enabled = %v, want the fetched true preserved (not reset)", putBody["enabled"])
	}
}

func TestGetClientRawNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: clientsBase + "/missing",
		status:   http.StatusNotFound,
		respBody: `{"error":"Could not find client"}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.GetClientRaw(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
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

func TestEnsureClientRoleMapperNoOpsWhenAlreadyConverged(t *testing.T) {
	// An existing mapper that already matches the desired type and programmed
	// config is left untouched: no POST and no PUT.
	postHit, putHit := false, false
	converged := `[{"id":"m1","name":"project-roles","protocolMapper":"oidc-usermodel-client-role-mapper","config":{"usermodel.clientRoleMapping.clientId":"https://app","claim.name":"groups","jsonType.label":"String","multivalued":"true","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true"}}]`
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, converged)
		},
		"POST " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			postHit = true
			w.WriteHeader(http.StatusCreated)
		},
		"PUT " + clientsBase + "/uuid-1/protocol-mappers/models/m1": func(w http.ResponseWriter, _ *http.Request) {
			putHit = true
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnsureClientRoleMapper(context.Background(), "uuid-1", "project-roles", "https://app", "groups"); err != nil {
		t.Fatalf("EnsureClientRoleMapper: %v", err)
	}
	if postHit || putHit {
		t.Error("a converged mapper must be left untouched (no POST/PUT)")
	}
}

func TestEnsureClientRoleMapperCorrectsDrift(t *testing.T) {
	// A same-named mapper exists but points at the WRONG clientId: it must be PUT
	// back to the desired definition, not left broken while reporting success.
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"id":"m1","name":"project-roles","protocolMapper":"oidc-usermodel-client-role-mapper","config":{"usermodel.clientRoleMapping.clientId":"https://WRONG","claim.name":"groups"}}]`)
		},
		"PUT " + clientsBase + "/uuid-1/protocol-mappers/models/m1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.EnsureClientRoleMapper(context.Background(), "uuid-1", "project-roles", "https://app", "groups"); err != nil {
		t.Fatalf("EnsureClientRoleMapper: %v", err)
	}
	if putBody == nil {
		t.Fatal("a drifted mapper must be corrected via PUT")
	}
	cfg, ok := putBody["config"].(map[string]any)
	if !ok || cfg["usermodel.clientRoleMapping.clientId"] != "https://app" {
		t.Errorf("PUT config clientId = %v, want corrected to https://app", putBody["config"])
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
	// The create races a concurrent creator and 409s; the re-read then finds the
	// now-present (and converged) mapper, so the call succeeds without a PUT.
	getCalls := 0
	converged := `[{"id":"m1","name":"project-roles","protocolMapper":"oidc-usermodel-client-role-mapper","config":{"usermodel.clientRoleMapping.clientId":"https://app","claim.name":"groups","jsonType.label":"String","multivalued":"true","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true"}}]`
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1/protocol-mappers/models": func(w http.ResponseWriter, _ *http.Request) {
			getCalls++
			w.WriteHeader(http.StatusOK)
			if getCalls == 1 {
				_, _ = io.WriteString(w, `[]`) // first list: not yet present
				return
			}
			_, _ = io.WriteString(w, converged) // re-read after the 409
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

func TestListGroupClientRoles(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet,
		wantPath: adminPathPrefix + "/realms/holos/groups/g-owner/role-mappings/clients/uuid-1",
		status:   http.StatusOK,
		respBody: `[{"id":"r1","name":"my-project-owner"},{"id":"r2","name":"my-project-editor"}]`,
	}
	c, _ := newTestClient(t, h)

	roles, err := c.ListGroupClientRoles(context.Background(), "g-owner", "uuid-1")
	if err != nil {
		t.Fatalf("ListGroupClientRoles: %v", err)
	}
	assertCommonRequest(t, h, false)
	if len(roles) != 2 || roles[0].Name != "my-project-owner" || roles[1].Name != "my-project-editor" {
		t.Errorf("roles = %+v", roles)
	}
}

func TestRemoveClientRoleFromGroup(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete,
		wantPath: adminPathPrefix + "/realms/holos/groups/g-owner/role-mappings/clients/uuid-1",
		status:   http.StatusNoContent,
	}
	c, _ := newTestClient(t, h)

	role := ClientRole{ID: "r1", Name: "my-project-owner"}
	if err := c.RemoveClientRoleFromGroup(context.Background(), "g-owner", "uuid-1", role); err != nil {
		t.Fatalf("RemoveClientRoleFromGroup: %v", err)
	}
	if len(h.gotArray) != 1 {
		t.Fatalf("body should be a one-element array, got %v", h.gotArray)
	}
	first, ok := h.gotArray[0].(map[string]any)
	if !ok || first["id"] != "r1" {
		t.Errorf("array[0] = %v, want the role with id", h.gotArray[0])
	}
}

func TestGetClientSecret(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet,
		wantPath: clientsBase + "/uuid-1/client-secret",
		status:   http.StatusOK,
		respBody: `{"type":"secret","value":"s3cr3t"}`,
	}
	c, _ := newTestClient(t, h)

	secret, err := c.GetClientSecret(context.Background(), "uuid-1")
	if err != nil {
		t.Fatalf("GetClientSecret: %v", err)
	}
	if secret.Value != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t", secret.Value)
	}
}

func TestDeleteClient(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete,
		wantPath: clientsBase + "/uuid-1",
		status:   http.StatusNoContent,
	}
	c, _ := newTestClient(t, h)

	if err := c.DeleteClient(context.Background(), "uuid-1"); err != nil {
		t.Fatalf("DeleteClient: %v", err)
	}
}

func TestDeleteClientByClientIDIfExistsAbsentIsNil(t *testing.T) {
	// FindClientByClientID returns an empty array (no such client); the delete must
	// be a no-op success.
	h := &recordingHandler{t: t, wantMethod: http.MethodGet, wantPath: clientsBase, status: http.StatusOK, respBody: `[]`}
	c, _ := newTestClient(t, h)

	if err := c.DeleteClientByClientIDIfExists(context.Background(), "https://absent"); err != nil {
		t.Fatalf("absent client must be a no-op success, got %v", err)
	}
}

func TestCreateClientSendsPKCEAttribute(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: clientsBase,
		status:   http.StatusCreated,
		location: "https://kc/admin/realms/holos/clients/uuid-pkce",
	}
	c, _ := newTestClient(t, h)

	_, err := c.CreateClient(context.Background(), OIDCClient{
		ClientID:     "https://pub",
		PublicClient: true,
		Attributes:   map[string]string{PKCECodeChallengeMethodAttr: PKCEMethodS256},
	})
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	attrs, ok := h.gotBody["attributes"].(map[string]any)
	if !ok || attrs[PKCECodeChallengeMethodAttr] != "S256" {
		t.Errorf("body attributes = %v, want PKCE S256", h.gotBody["attributes"])
	}
}

func TestCreateClientSendsDescription(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: clientsBase,
		status:   http.StatusCreated,
		location: "https://kc/admin/realms/holos/clients/uuid-desc",
	}
	c, _ := newTestClient(t, h)

	_, err := c.CreateClient(context.Background(), OIDCClient{
		ClientID:    "https://app",
		Description: "the app client",
	})
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if h.gotBody["description"] != "the app client" {
		t.Errorf("body description = %v, want %q", h.gotBody["description"], "the app client")
	}
}

func TestUpdateClientFieldsSetsDescription(t *testing.T) {
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://app"}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	desc := "managed description"
	if err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{Description: &desc}); err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	if putBody["description"] != "managed description" {
		t.Errorf("description = %v, want %q", putBody["description"], "managed description")
	}
}

func TestUpdateClientFieldsClearsDescription(t *testing.T) {
	// A pointer to the empty string actively clears the description.
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://app","description":"old"}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	empty := ""
	if err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{Description: &empty}); err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	got, ok := putBody["description"]
	if !ok {
		t.Fatalf("description key absent; want it written to empty string to clear")
	}
	if got != "" {
		t.Errorf("description = %v, want cleared to empty string", got)
	}
}

func TestUpdateClientFieldsNilDescriptionLeavesItUntouched(t *testing.T) {
	// A nil Description must not appear as an override; the fetched value stays.
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://app","description":"keep me"}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	newName := "App"
	if err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{Name: &newName}); err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	if putBody["description"] != "keep me" {
		t.Errorf("description = %v, want the fetched %q preserved (nil leaves it untouched)", putBody["description"], "keep me")
	}
}

func TestUpdateClientFieldsMergesPKCEAttribute(t *testing.T) {
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://pub","attributes":{"existing":"keep"}}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(req.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{
		Attributes: map[string]string{PKCECodeChallengeMethodAttr: PKCEMethodS256},
	})
	if err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	attrs, ok := putBody["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("PUT body has no attributes map: %v", putBody)
	}
	if attrs["existing"] != "keep" {
		t.Errorf("unmanaged attribute clobbered: %v", attrs)
	}
	if attrs[PKCECodeChallengeMethodAttr] != "S256" {
		t.Errorf("PKCE attribute not merged: %v", attrs)
	}
}

func TestUpdateClientFieldsRemovesAttribute(t *testing.T) {
	var putBody map[string]any
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + clientsBase + "/uuid-1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"uuid-1","clientId":"https://conf","attributes":{"pkce.code.challenge.method":"S256","keep":"yes"}}`)
		},
		"PUT " + clientsBase + "/uuid-1": func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(req.Body)
			putBody = decodeJSONObject(t, body)
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.UpdateClientFields(context.Background(), "uuid-1", ClientFields{
		RemoveAttributes: []string{PKCECodeChallengeMethodAttr},
	})
	if err != nil {
		t.Fatalf("UpdateClientFields: %v", err)
	}
	attrs, ok := putBody["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("PUT body has no attributes map: %v", putBody)
	}
	if _, present := attrs[PKCECodeChallengeMethodAttr]; present {
		t.Errorf("PKCE attribute was not removed: %v", attrs)
	}
	if attrs["keep"] != "yes" {
		t.Errorf("unmanaged attribute clobbered by removal: %v", attrs)
	}
}

package quay

import (
	"net/http"
	"testing"
)

func TestListPrototypes(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme/prototypes",
		status:   http.StatusOK,
		respBody: `{"prototypes":[{"id":"p1","role":"write","delegate":{"name":"devs","kind":"team"}},{"id":"p2","role":"read","delegate":{"name":"ops","kind":"team"}}]}`,
	}
	c, _ := newTestClient(t, h)

	protos, err := c.ListPrototypes(t.Context(), "acme")
	if err != nil {
		t.Fatalf("ListPrototypes: %v", err)
	}
	assertCommonRequest(t, h, false)
	if len(protos) != 2 {
		t.Fatalf("prototypes len = %d, want 2: %+v", len(protos), protos)
	}
	if protos[0].ID != "p1" || protos[0].Role != "write" {
		t.Errorf("protos[0] = %+v", protos[0])
	}
	if protos[0].Delegate.Name != "devs" || protos[0].Delegate.Kind != "team" {
		t.Errorf("protos[0].Delegate = %+v", protos[0].Delegate)
	}
	if protos[1].ID != "p2" || protos[1].Delegate.Name != "ops" {
		t.Errorf("protos[1] = %+v", protos[1])
	}
}

func TestListPrototypesNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/missing/prototypes",
		status:   http.StatusNotFound,
		respBody: `{"error_message":"Not Found"}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.ListPrototypes(t.Context(), "missing"); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestCreatePrototype(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/organization/acme/prototypes",
		status:   http.StatusOK,
		respBody: `{"id":"p1","role":"write","delegate":{"name":"devs","kind":"team","avatar":{}}}`,
	}
	c, _ := newTestClient(t, h)

	p, err := c.CreatePrototype(t.Context(), "acme", PrototypeRoleWrite, "devs")
	if err != nil {
		t.Fatalf("CreatePrototype: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["role"] != "write" {
		t.Errorf("body role = %v, want write", h.gotBody["role"])
	}
	delegate, ok := h.gotBody["delegate"].(map[string]any)
	if !ok {
		t.Fatalf("body delegate = %v, want object", h.gotBody["delegate"])
	}
	if delegate["kind"] != "team" || delegate["name"] != "devs" {
		t.Errorf("body delegate = %+v, want {kind:team,name:devs}", delegate)
	}
	if p.ID != "p1" || p.Role != "write" || p.Delegate.Name != "devs" || p.Delegate.Kind != "team" {
		t.Errorf("decoded prototype = %+v", p)
	}
}

func TestUpdatePrototype(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme/prototypes/p1", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.UpdatePrototype(t.Context(), "acme", "p1", PrototypeRoleAdmin); err != nil {
		t.Fatalf("UpdatePrototype: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["role"] != "admin" {
		t.Errorf("body role = %v, want admin", h.gotBody["role"])
	}
}

func TestUpdatePrototypeNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme/prototypes/gone",
		status:   http.StatusNotFound,
		respBody: `{"error_message":"Not Found"}`,
	}
	c, _ := newTestClient(t, h)

	if err := c.UpdatePrototype(t.Context(), "acme", "gone", PrototypeRoleRead); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestDeletePrototype(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/prototypes/p1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeletePrototype(t.Context(), "acme", "p1"); err != nil {
		t.Fatalf("DeletePrototype: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeletePrototypeIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/prototypes/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeletePrototypeIfExists(t.Context(), "acme", "gone"); err != nil {
		t.Fatalf("DeletePrototypeIfExists should swallow 404, got %v", err)
	}
}

func TestPrototypePathEscaping(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/a b/prototypes/p 1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeletePrototype(t.Context(), "a b", "p 1"); err != nil {
		t.Fatalf("DeletePrototype with spaces: %v", err)
	}
	if h.gotEscaped != "/api/v1/organization/a%20b/prototypes/p%201" {
		t.Errorf("escaped path = %q, want spaces percent-encoded", h.gotEscaped)
	}
}

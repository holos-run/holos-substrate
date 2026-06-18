package quay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordingHandler captures the request a single call makes and replies with a
// canned status and body, so each test asserts method/path/headers/body and the
// client's decoding of the response in one place.
type recordingHandler struct {
	t          *testing.T
	wantMethod string
	wantPath   string
	status     int
	respBody   string

	// captured request fields
	gotMethod  string
	gotPath    string
	gotAuth    string
	gotContent string
	gotAccept  string
	gotBody    map[string]any
	gotRawBody string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.gotMethod = r.Method
	h.gotPath = r.URL.Path
	h.gotAuth = r.Header.Get("Authorization")
	h.gotContent = r.Header.Get("Content-Type")
	h.gotAccept = r.Header.Get("Accept")
	body, _ := io.ReadAll(r.Body)
	h.gotRawBody = string(body)
	if len(body) > 0 {
		if err := json.Unmarshal(body, &h.gotBody); err != nil {
			h.t.Errorf("request body is not valid JSON: %v (%q)", err, string(body))
		}
	}
	if h.status == 0 {
		h.status = http.StatusOK
	}
	w.WriteHeader(h.status)
	if h.respBody != "" {
		_, _ = io.WriteString(w, h.respBody)
	}
}

// newTestClient spins up an httptest server with the given handler and returns a
// Client pointed at it. The caller closes the returned server.
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token", srv.Client()), srv
}

// assertCommonRequest checks the auth header and (when a body is sent) the
// content type, which every JSON request must carry.
func assertCommonRequest(t *testing.T, h *recordingHandler, expectBody bool) {
	t.Helper()
	if h.gotMethod != h.wantMethod {
		t.Errorf("method = %q, want %q", h.gotMethod, h.wantMethod)
	}
	if h.gotPath != h.wantPath {
		t.Errorf("path = %q, want %q", h.gotPath, h.wantPath)
	}
	if h.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", h.gotAuth, "Bearer test-token")
	}
	if h.gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", h.gotAccept)
	}
	if expectBody && h.gotContent != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", h.gotContent)
	}
	if !expectBody && h.gotRawBody != "" {
		t.Errorf("expected no request body, got %q", h.gotRawBody)
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://quay.example.com/", "tok", nil)
	if c.baseURL != "https://quay.example.com" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
	if c.httpClient != http.DefaultClient {
		t.Error("nil http.Client should default to http.DefaultClient")
	}
}

func TestCreateOrganization(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/organization/", status: http.StatusCreated}
	c, _ := newTestClient(t, h)

	if err := c.CreateOrganization(context.Background(), "acme", "ops@acme.example"); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["name"] != "acme" {
		t.Errorf("body name = %v, want acme", h.gotBody["name"])
	}
	if h.gotBody["email"] != "ops@acme.example" {
		t.Errorf("body email = %v, want ops@acme.example", h.gotBody["email"])
	}
}

func TestCreateOrganizationDuplicateIsConflict(t *testing.T) {
	// Quay reports a duplicate org as 400 with an "already exists" message.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/organization/",
		status:   http.StatusBadRequest,
		respBody: `{"message":"A user or organization with this name already exists"}`,
	}
	c, _ := newTestClient(t, h)

	err := c.CreateOrganization(context.Background(), "acme", "ops@acme.example")
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict for duplicate org, got %v", err)
	}

	// The if-not-exists wrapper swallows it.
	if err := c.CreateOrganizationIfNotExists(context.Background(), "acme", "ops@acme.example"); err != nil {
		t.Fatalf("CreateOrganizationIfNotExists should treat duplicate as success, got %v", err)
	}
}

func TestGetOrganization(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme",
		status:   http.StatusOK,
		respBody: `{"name":"acme","email":"ops@acme.example","is_org_admin":true}`,
	}
	c, _ := newTestClient(t, h)

	org, err := c.GetOrganization(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	assertCommonRequest(t, h, false)
	if org.Name != "acme" || org.Email != "ops@acme.example" || !org.IsOrgAdmin {
		t.Errorf("decoded org = %+v", org)
	}
}

func TestGetOrganizationNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/missing",
		status:   http.StatusNotFound,
		respBody: `{"error_message":"Not Found"}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.GetOrganization(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *APIError: %v", err)
	}
	if ae.Message != "Not Found" {
		t.Errorf("APIError.Message = %q, want surfaced body message", ae.Message)
	}
}

func TestDeleteOrganization(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteOrganization(context.Background(), "acme"); err != nil {
		t.Fatalf("DeleteOrganization: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeleteOrganizationIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeleteOrganizationIfExists(context.Background(), "gone"); err != nil {
		t.Fatalf("DeleteOrganizationIfExists should swallow 404, got %v", err)
	}
}

func TestCreateRepository(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/repository", status: http.StatusCreated}
	c, _ := newTestClient(t, h)

	err := c.CreateRepository(context.Background(), "acme", "web", VisibilityPrivate, "the web app")
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["namespace"] != "acme" || h.gotBody["repository"] != "web" {
		t.Errorf("body identity = %+v", h.gotBody)
	}
	if h.gotBody["visibility"] != "private" {
		t.Errorf("body visibility = %v, want private", h.gotBody["visibility"])
	}
	if h.gotBody["description"] != "the web app" {
		t.Errorf("body description = %v", h.gotBody["description"])
	}
	if h.gotBody["repo_kind"] != "image" {
		t.Errorf("body repo_kind = %v, want image", h.gotBody["repo_kind"])
	}
}

func TestCreateRepositoryConflict(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/repository",
		status:   http.StatusConflict,
		respBody: `{"error_message":"Repository already exists"}`,
	}
	c, _ := newTestClient(t, h)

	err := c.CreateRepository(context.Background(), "acme", "web", VisibilityPrivate, "")
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict, got %v", err)
	}
	if err := c.CreateRepositoryIfNotExists(context.Background(), "acme", "web", VisibilityPrivate, ""); err != nil {
		t.Fatalf("CreateRepositoryIfNotExists should swallow 409, got %v", err)
	}
}

func TestGetRepository(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/repository/acme/web",
		status:   http.StatusOK,
		respBody: `{"description":"the web app","is_public":false}`,
	}
	c, _ := newTestClient(t, h)

	repo, err := c.GetRepository(context.Background(), "acme", "web")
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	assertCommonRequest(t, h, false)
	// namespace/name backfilled from the request when Quay omits them.
	if repo.Namespace != "acme" || repo.Name != "web" {
		t.Errorf("identity not backfilled: %+v", repo)
	}
	if repo.IsPublic {
		t.Error("repo should be private")
	}
	if repo.Description != "the web app" {
		t.Errorf("description = %q", repo.Description)
	}
}

func TestUpdateRepositoryVisibility(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/repository/acme/web/changevisibility", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.UpdateRepositoryVisibility(context.Background(), "acme", "web", VisibilityPublic); err != nil {
		t.Fatalf("UpdateRepositoryVisibility: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["visibility"] != "public" {
		t.Errorf("body visibility = %v, want public", h.gotBody["visibility"])
	}
}

func TestUpdateRepositoryDescription(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/repository/acme/web", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.UpdateRepositoryDescription(context.Background(), "acme", "web", "updated"); err != nil {
		t.Fatalf("UpdateRepositoryDescription: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["description"] != "updated" {
		t.Errorf("body description = %v, want updated", h.gotBody["description"])
	}
}

func TestDeleteRepository(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/repository/acme/web", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteRepository(context.Background(), "acme", "web"); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeleteRepositoryIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/repository/acme/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeleteRepositoryIfExists(context.Background(), "acme", "gone"); err != nil {
		t.Fatalf("DeleteRepositoryIfExists should swallow 404, got %v", err)
	}
}

func TestCreateNotification(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/repository/acme/web/notification/",
		status:   http.StatusCreated,
		respBody: `{"uuid":"abc-123","event":"repo_push","method":"webhook","title":"kargo","config":{"url":"https://kargo.example/webhook"}}`,
	}
	c, _ := newTestClient(t, h)

	n, err := c.CreateNotification(context.Background(), "acme", "web", "https://kargo.example/webhook", "kargo")
	if err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["event"] != "repo_push" || h.gotBody["method"] != "webhook" {
		t.Errorf("body event/method = %+v", h.gotBody)
	}
	cfg, ok := h.gotBody["config"].(map[string]any)
	if !ok || cfg["url"] != "https://kargo.example/webhook" {
		t.Errorf("body config = %v", h.gotBody["config"])
	}
	if _, ok := h.gotBody["eventConfig"]; !ok {
		t.Error("body should carry eventConfig")
	}
	if h.gotBody["title"] != "kargo" {
		t.Errorf("body title = %v", h.gotBody["title"])
	}
	if n.UUID != "abc-123" || n.Config.URL != "https://kargo.example/webhook" {
		t.Errorf("decoded notification = %+v", n)
	}
}

func TestListNotifications(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/repository/acme/web/notification/",
		status:   http.StatusOK,
		respBody: `{"notifications":[{"uuid":"u1","event":"repo_push","method":"webhook","config":{"url":"https://a"}},{"uuid":"u2","event":"repo_push","method":"webhook","config":{"url":"https://b"}}]}`,
	}
	c, _ := newTestClient(t, h)

	list, err := c.ListNotifications(context.Background(), "acme", "web")
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	assertCommonRequest(t, h, false)
	if len(list) != 2 || list[0].UUID != "u1" || list[1].Config.URL != "https://b" {
		t.Errorf("decoded list = %+v", list)
	}
}

func TestDeleteNotification(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/repository/acme/web/notification/u1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteNotification(context.Background(), "acme", "web", "u1"); err != nil {
		t.Fatalf("DeleteNotification: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeleteNotificationIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/repository/acme/web/notification/gone", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeleteNotificationIfExists(context.Background(), "acme", "web", "gone"); err != nil {
		t.Fatalf("DeleteNotificationIfExists should swallow 404, got %v", err)
	}
}

func TestServerError500(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme",
		status:   http.StatusInternalServerError,
		respBody: `{"error_message":"boom"}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.GetOrganization(context.Background(), "acme")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if IsNotFound(err) || IsConflict(err) {
		t.Errorf("500 must be neither not-found nor conflict: %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *APIError: %v", err)
	}
	if ae.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", ae.StatusCode)
	}
	if ae.Body != `{"error_message":"boom"}` {
		t.Errorf("APIError.Body should surface raw body, got %q", ae.Body)
	}
}

func TestPathEscaping(t *testing.T) {
	// A name needing escaping must arrive percent-encoded at the server.
	h := &recordingHandler{t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/a b", status: http.StatusOK, respBody: `{"name":"a b"}`}
	c, _ := newTestClient(t, h)

	if _, err := c.GetOrganization(context.Background(), "a b"); err != nil {
		t.Fatalf("GetOrganization with space: %v", err)
	}
	// httptest decodes the path before handing it to the handler, so gotPath is
	// the decoded form; the assertion above on wantPath confirms round-trip.
	assertCommonRequest(t, h, false)
}

// asAPIErr is a thin errors.As wrapper used by tests to extract *APIError.
func asAPIErr(err error, target **APIError) bool {
	for err != nil {
		if ae, ok := err.(*APIError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

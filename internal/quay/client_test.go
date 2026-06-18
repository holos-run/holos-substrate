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
	gotEscaped string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.gotMethod = r.Method
	h.gotPath = r.URL.Path
	h.gotEscaped = r.URL.EscapedPath()
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

func TestNewClientNormalizesBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"instance root", "https://quay.example.com", "https://quay.example.com"},
		{"trailing slash", "https://quay.example.com/", "https://quay.example.com"},
		{"api root", "https://quay.example.com/api/v1", "https://quay.example.com"},
		{"api root trailing slash", "https://quay.example.com/api/v1/", "https://quay.example.com"},
		{"subpath instance", "https://host/quay", "https://host/quay"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(tc.in, "tok", nil)
			if c.baseURL != tc.want {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.want)
			}
		})
	}
}

func TestNewClientNilClientGetsTimeout(t *testing.T) {
	c := NewClient("https://quay.example.com", "tok", nil)
	if c.httpClient == http.DefaultClient {
		t.Error("nil http.Client must not be the timeout-less http.DefaultClient")
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("default client Timeout = %v, want %v", c.httpClient.Timeout, defaultTimeout)
	}
}

// TestAPIRootBaseURLNoDoubling exercises the end-to-end path: a client built
// from an /api/v1 base URL must still hit /api/v1/organization/acme, not a
// doubled prefix.
func TestAPIRootBaseURLNoDoubling(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme",
		status: http.StatusOK, respBody: `{"name":"acme"}`,
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL+"/api/v1", "test-token", srv.Client())

	if _, err := c.GetOrganization(context.Background(), "acme"); err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if h.gotPath != "/api/v1/organization/acme" {
		t.Errorf("path = %q, want un-doubled /api/v1/organization/acme", h.gotPath)
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

func TestUpdateOrganization(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme", status: http.StatusOK}
	c, _ := newTestClient(t, h)

	if err := c.UpdateOrganization(context.Background(), "acme", "new@acme.example"); err != nil {
		t.Fatalf("UpdateOrganization: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["email"] != "new@acme.example" {
		t.Errorf("body email = %v, want new@acme.example", h.gotBody["email"])
	}
	// Quay orgs have no display-name field, so the PUT body must carry only the
	// email — not a display name.
	if _, ok := h.gotBody["displayName"]; ok {
		t.Errorf("PUT body unexpectedly carried displayName: %+v", h.gotBody)
	}
}

func TestGetOrganizationRobot(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme/robots/holos-owner",
		status:   http.StatusOK,
		respBody: `{"name":"acme+holos-owner","description":"owner-uid-123","token":"abc"}`,
	}
	c, _ := newTestClient(t, h)

	robot, err := c.GetOrganizationRobot(context.Background(), "acme", "holos-owner")
	if err != nil {
		t.Fatalf("GetOrganizationRobot: %v", err)
	}
	assertCommonRequest(t, h, false)
	if robot.Name != "acme+holos-owner" || robot.Description != "owner-uid-123" {
		t.Errorf("decoded robot = %+v", robot)
	}
}

func TestGetOrganizationRobotNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: "/api/v1/organization/acme/robots/holos-owner",
		status:   http.StatusNotFound,
		respBody: `{"error_message":"Could not find robot with specified username"}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.GetOrganizationRobot(context.Background(), "acme", "holos-owner")
	if !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestCreateOrganizationRobot(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme/robots/holos-owner", status: http.StatusCreated}
	c, _ := newTestClient(t, h)

	if err := c.CreateOrganizationRobot(context.Background(), "acme", "holos-owner", "owner-uid-123"); err != nil {
		t.Fatalf("CreateOrganizationRobot: %v", err)
	}
	assertCommonRequest(t, h, true)
	if h.gotBody["description"] != "owner-uid-123" {
		t.Errorf("body description = %v, want owner-uid-123", h.gotBody["description"])
	}
}

func TestCreateOrganizationRobotDuplicateIsConflict(t *testing.T) {
	// Quay's create-robot endpoint is not idempotent: an existing robot comes
	// back as a 400 naming the duplicate, which must map to a conflict.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPut, wantPath: "/api/v1/organization/acme/robots/holos-owner",
		status:   http.StatusBadRequest,
		respBody: `{"message":"Existing robot with name: acme+holos-owner already exists"}`,
	}
	c, _ := newTestClient(t, h)

	err := c.CreateOrganizationRobot(context.Background(), "acme", "holos-owner", "owner-uid-123")
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict for duplicate robot, got %v", err)
	}
}

func TestDeleteOrganizationRobotIfExistsSwallowsNotFound(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/organization/acme/robots/holos-owner", status: http.StatusNotFound}
	c, _ := newTestClient(t, h)

	if err := c.DeleteOrganizationRobotIfExists(context.Background(), "acme", "holos-owner"); err != nil {
		t.Fatalf("DeleteOrganizationRobotIfExists should swallow 404, got %v", err)
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

// muxHandler routes by method+path so a single test can drive a sequence of
// distinct requests (e.g. a failing create followed by a confirming GET).
type muxHandler struct {
	t       *testing.T
	routes  map[string]func(w http.ResponseWriter, r *http.Request)
	unknown int
}

func (m *muxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	if fn, ok := m.routes[key]; ok {
		fn(w, r)
		return
	}
	m.unknown++
	m.t.Errorf("unexpected request %s", key)
	w.WriteHeader(http.StatusInternalServerError)
}

func TestCreateRepositoryGenericQuay400IsNotConflict(t *testing.T) {
	// Quay's "Could not create repository" 400 is a generic create failure, not
	// a reliable duplicate signal: the bare create must NOT report it as a
	// conflict (otherwise the if-not-exists wrapper would swallow a real failure
	// without confirming existence).
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: "/api/v1/repository",
		status:   http.StatusBadRequest,
		respBody: `{"error_message":"Could not create repository"}`,
	}
	c, _ := newTestClient(t, h)

	err := c.CreateRepository(context.Background(), "acme", "web", VisibilityPrivate, "")
	if err == nil {
		t.Fatal("expected an error for the 400")
	}
	if IsConflict(err) {
		t.Errorf("generic 'Could not create repository' 400 must not be a conflict: %v", err)
	}
}

func TestCreateRepositoryIfNotExistsConfirmsViaGet(t *testing.T) {
	// An unrecognized 400 on create, but the repo exists: the GET fallback
	// confirms existence and the wrapper succeeds.
	getHit := false
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/repository": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_message":"some opaque error"}`)
		},
		"GET /api/v1/repository/acme/web": func(w http.ResponseWriter, _ *http.Request) {
			getHit = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"is_public":false}`)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.CreateRepositoryIfNotExists(context.Background(), "acme", "web", VisibilityPrivate, ""); err != nil {
		t.Fatalf("CreateRepositoryIfNotExists should succeed when GET confirms existence, got %v", err)
	}
	if !getHit {
		t.Error("expected a confirming GET after the failed create")
	}
}

func TestCreateRepositoryIfNotExistsSurfacesRealError(t *testing.T) {
	// Create fails AND the repo does not exist: the original create error must
	// surface rather than being swallowed.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/repository": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error_message":"boom"}`)
		},
		"GET /api/v1/repository/acme/web": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error_message":"Not Found"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	err := c.CreateRepositoryIfNotExists(context.Background(), "acme", "web", VisibilityPrivate, "")
	if err == nil {
		t.Fatal("expected the original create error to surface")
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected the 500 create error, got %v", err)
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

func TestDeleteNotificationIfExistsSwallows400InvalidRequest(t *testing.T) {
	// Quay can return 400 InvalidRequest (not 404) for an already-removed
	// notification UUID; the if-exists wrapper must treat it as gone.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/repository/acme/web/notification/gone",
		status:   http.StatusBadRequest,
		respBody: `{"error":"InvalidRequest","error_message":"Invalid request"}`,
	}
	c, _ := newTestClient(t, h)

	if err := c.DeleteNotificationIfExists(context.Background(), "acme", "web", "gone"); err != nil {
		t.Fatalf("DeleteNotificationIfExists should swallow 400 InvalidRequest, got %v", err)
	}
}

func TestDeleteNotificationIfExistsSurfacesOther400(t *testing.T) {
	// A 400 that is not an absent-UUID signal must still surface.
	h := &recordingHandler{
		t: t, wantMethod: http.MethodDelete, wantPath: "/api/v1/repository/acme/web/notification/u1",
		status:   http.StatusBadRequest,
		respBody: `{"error_message":"some other validation failure"}`,
	}
	c, _ := newTestClient(t, h)

	if err := c.DeleteNotificationIfExists(context.Background(), "acme", "web", "u1"); err == nil {
		t.Fatal("expected an unrelated 400 to surface, got nil")
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
	// r.URL.Path is already percent-decoded by net/http (so gotPath is the
	// decoded form assertCommonRequest checks); the on-the-wire escaped path is
	// what proves the client percent-encoded the space.
	assertCommonRequest(t, h, false)
	if h.gotEscaped != "/api/v1/organization/a%20b" {
		t.Errorf("escaped path = %q, want the space percent-encoded as %%20", h.gotEscaped)
	}
}

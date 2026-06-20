package keycloak

import (
	"context"
	"io"
	"net/http"
	"testing"
)

const groupsBase = adminPathPrefix + "/realms/holos/groups"

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"projects/p/roles/owner":   "/projects/p/roles/owner",
		"/projects/p/roles/owner":  "/projects/p/roles/owner",
		"/projects/p/roles/owner/": "/projects/p/roles/owner",
		"":                         "",
		"/":                        "",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetGroupByPath(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet,
		wantPath: adminPathPrefix + "/realms/holos/group-by-path/projects/my-project/roles/owner",
		status:   http.StatusOK,
		respBody: `{"id":"g-owner","name":"owner","path":"/projects/my-project/roles/owner"}`,
	}
	c, _ := newTestClient(t, h)

	g, err := c.GetGroupByPath(context.Background(), "projects/my-project/roles/owner")
	if err != nil {
		t.Fatalf("GetGroupByPath: %v", err)
	}
	assertCommonRequest(t, h, false)
	if g.ID != "g-owner" || g.Path != "/projects/my-project/roles/owner" {
		t.Errorf("decoded group = %+v", g)
	}
}

func TestGetGroupByPathNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet,
		wantPath: adminPathPrefix + "/realms/holos/group-by-path/projects/missing",
		status:   http.StatusNotFound,
		respBody: `{"error":"Group path does not exist"}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.GetGroupByPath(context.Background(), "/projects/missing"); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestGetGroup(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: groupsBase + "/g1",
		status:   http.StatusOK,
		respBody: `{"id":"g1","name":"projects","subGroups":[{"id":"g2","name":"my-project"}]}`,
	}
	c, _ := newTestClient(t, h)

	g, err := c.GetGroup(context.Background(), "g1")
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	assertCommonRequest(t, h, false)
	if g.ID != "g1" || len(g.SubGroups) != 1 || g.SubGroups[0].Name != "my-project" {
		t.Errorf("decoded group = %+v", g)
	}
}

func TestCreateTopLevelGroup(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: groupsBase,
		status:   http.StatusCreated,
		location: "https://kc/admin/realms/holos/groups/new-id",
	}
	c, _ := newTestClient(t, h)

	id, err := c.CreateTopLevelGroup(context.Background(), "projects")
	if err != nil {
		t.Fatalf("CreateTopLevelGroup: %v", err)
	}
	if id != "new-id" {
		t.Errorf("id = %q, want new-id (from Location)", id)
	}
	if h.gotBody["name"] != "projects" {
		t.Errorf("body name = %v, want projects", h.gotBody["name"])
	}
}

func TestCreateTopLevelGroupConflict(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: groupsBase,
		status:   http.StatusConflict,
		respBody: `{"errorMessage":"Top level group named 'projects' already exists."}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.CreateTopLevelGroup(context.Background(), "projects")
	if !IsConflict(err) {
		t.Fatalf("expected IsConflict for duplicate group, got %v", err)
	}
}

func TestCreateChildGroup(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodPost, wantPath: groupsBase + "/parent-id/children",
		status:   http.StatusCreated,
		location: "https://kc/admin/realms/holos/groups/child-id",
	}
	c, _ := newTestClient(t, h)

	id, err := c.CreateChildGroup(context.Background(), "parent-id", "roles")
	if err != nil {
		t.Fatalf("CreateChildGroup: %v", err)
	}
	if id != "child-id" {
		t.Errorf("id = %q, want child-id", id)
	}
	if h.gotBody["name"] != "roles" {
		t.Errorf("body name = %v, want roles", h.gotBody["name"])
	}
}

func TestDeleteGroup(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodDelete, wantPath: groupsBase + "/g1", status: http.StatusNoContent}
	c, _ := newTestClient(t, h)

	if err := c.DeleteGroup(context.Background(), "g1"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	assertCommonRequest(t, h, false)
}

func TestDeleteGroupByPathIfExistsSwallowsMissingPath(t *testing.T) {
	// The path lookup 404s: the group is already gone, so the call succeeds
	// without a DELETE.
	deleteHit := false
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/gone": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.DeleteGroupByPathIfExists(context.Background(), "projects/gone"); err != nil {
		t.Fatalf("DeleteGroupByPathIfExists should swallow a missing path, got %v", err)
	}
	if deleteHit {
		t.Error("no DELETE should be issued for an already-absent group")
	}
}

func TestDeleteGroupByPathIfExistsDeletesResolvedID(t *testing.T) {
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p/roles/owner": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"g-owner","path":"/projects/p/roles/owner"}`)
		},
		"DELETE " + groupsBase + "/g-owner": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	}}
	c, _ := newTestClient(t, m)

	if err := c.DeleteGroupByPathIfExists(context.Background(), "/projects/p/roles/owner"); err != nil {
		t.Fatalf("DeleteGroupByPathIfExists: %v", err)
	}
}

func TestEnsureGroupByPathFastPathWhenPresent(t *testing.T) {
	// The whole path already resolves: a single lookup, no creates.
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p/roles/owner": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"g-owner"}`)
		},
	}}
	c, _ := newTestClient(t, m)

	id, err := c.EnsureGroupByPath(context.Background(), "projects/p/roles/owner")
	if err != nil {
		t.Fatalf("EnsureGroupByPath: %v", err)
	}
	if id != "g-owner" {
		t.Errorf("id = %q, want g-owner", id)
	}
}

func TestEnsureGroupByPathCreatesMissingTree(t *testing.T) {
	// Nothing exists: the full path 404s, each prefix 404s, then top-level
	// "projects" is created and each remaining segment is created as a child.
	created := map[string]bool{}
	routes := map[string]func(http.ResponseWriter, *http.Request){}
	// All group-by-path lookups 404 (nothing pre-exists).
	for _, p := range []string{
		"/projects/p/roles/owner", "/projects", "/projects/p", "/projects/p/roles", "/projects/p/roles/owner",
	} {
		routes["GET "+adminPathPrefix+"/realms/holos/group-by-path"+p] = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	}
	routes["POST "+groupsBase] = func(w http.ResponseWriter, _ *http.Request) {
		created["projects"] = true
		w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-projects")
		w.WriteHeader(http.StatusCreated)
	}
	routes["POST "+groupsBase+"/id-projects/children"] = func(w http.ResponseWriter, _ *http.Request) {
		created["p"] = true
		w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-p")
		w.WriteHeader(http.StatusCreated)
	}
	routes["POST "+groupsBase+"/id-p/children"] = func(w http.ResponseWriter, _ *http.Request) {
		created["roles"] = true
		w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-roles")
		w.WriteHeader(http.StatusCreated)
	}
	routes["POST "+groupsBase+"/id-roles/children"] = func(w http.ResponseWriter, _ *http.Request) {
		created["owner"] = true
		w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-owner")
		w.WriteHeader(http.StatusCreated)
	}
	c, _ := newTestClient(t, &muxHandler{t: t, routes: routes})

	id, err := c.EnsureGroupByPath(context.Background(), "projects/p/roles/owner")
	if err != nil {
		t.Fatalf("EnsureGroupByPath: %v", err)
	}
	if id != "id-owner" {
		t.Errorf("leaf id = %q, want id-owner", id)
	}
	for _, seg := range []string{"projects", "p", "roles", "owner"} {
		if !created[seg] {
			t.Errorf("segment %q was not created", seg)
		}
	}
}

func TestEnsureGroupByPathReusesExistingAncestor(t *testing.T) {
	// projects/p exists; only roles and owner need creating.
	createdChildren := 0
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p/roles/owner": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"id-projects"}`)
		},
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"id-p"}`)
		},
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p/roles": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"POST " + groupsBase + "/id-p/children": func(w http.ResponseWriter, _ *http.Request) {
			createdChildren++
			w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-roles")
			w.WriteHeader(http.StatusCreated)
		},
		"POST " + groupsBase + "/id-roles/children": func(w http.ResponseWriter, _ *http.Request) {
			createdChildren++
			w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-owner")
			w.WriteHeader(http.StatusCreated)
		},
	}
	c, _ := newTestClient(t, &muxHandler{t: t, routes: routes})

	id, err := c.EnsureGroupByPath(context.Background(), "projects/p/roles/owner")
	if err != nil {
		t.Fatalf("EnsureGroupByPath: %v", err)
	}
	if id != "id-owner" {
		t.Errorf("leaf id = %q, want id-owner", id)
	}
	if createdChildren != 2 {
		t.Errorf("created %d children, want 2 (roles, owner; projects/p reused)", createdChildren)
	}
}

func TestEnsureGroupByPathToleratesConcurrentConflict(t *testing.T) {
	// A create races a concurrent creator: the POST 409s, and the now-present
	// node is re-resolved so the walk continues.
	getCalls := 0
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects": func(w http.ResponseWriter, _ *http.Request) {
			getCalls++
			if getCalls == 1 {
				// Full-path fast probe (the whole path) -> miss is fine; but this
				// route only matches /projects. First hit: not yet present.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Re-resolve after the 409: now present.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"id-projects"}`)
		},
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"POST " + groupsBase: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"errorMessage":"already exists"}`)
		},
		"POST " + groupsBase + "/id-projects/children": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-p")
			w.WriteHeader(http.StatusCreated)
		},
	}
	c, _ := newTestClient(t, &muxHandler{t: t, routes: routes})

	id, err := c.EnsureGroupByPath(context.Background(), "projects/p")
	if err != nil {
		t.Fatalf("EnsureGroupByPath should tolerate a concurrent 409, got %v", err)
	}
	if id != "id-p" {
		t.Errorf("leaf id = %q, want id-p", id)
	}
}

func TestEnsureGroupByPathReResolvesWhenCreateReturnsNoID(t *testing.T) {
	// A create that succeeds (2xx) but returns no Location header yields an empty
	// id; EnsureGroupByPath must re-resolve the node by path so a follow-up child
	// create never runs with an empty parentID.
	resolveAfterCreate := false
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects/p": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"GET " + adminPathPrefix + "/realms/holos/group-by-path/projects": func(w http.ResponseWriter, _ *http.Request) {
			if !resolveAfterCreate {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"id-projects"}`)
		},
		"POST " + groupsBase: func(w http.ResponseWriter, _ *http.Request) {
			// 201 with NO Location header.
			resolveAfterCreate = true
			w.WriteHeader(http.StatusCreated)
		},
		"POST " + groupsBase + "/id-projects/children": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://kc/admin/realms/holos/groups/id-p")
			w.WriteHeader(http.StatusCreated)
		},
	}
	c, _ := newTestClient(t, &muxHandler{t: t, routes: routes})

	id, err := c.EnsureGroupByPath(context.Background(), "projects/p")
	if err != nil {
		t.Fatalf("EnsureGroupByPath: %v", err)
	}
	if id != "id-p" {
		t.Errorf("leaf id = %q, want id-p (child created under the re-resolved parent)", id)
	}
}

func TestEnsureGroupByPathEmptyPathRejected(t *testing.T) {
	c, _ := newTestClient(t, &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}})
	if _, err := c.EnsureGroupByPath(context.Background(), "/"); err == nil {
		t.Fatal("expected an error for an empty group path")
	}
}

func TestGroupByPathEscaping(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet,
		wantPath: adminPathPrefix + "/realms/holos/group-by-path/a b/c d",
		status:   http.StatusOK, respBody: `{"id":"x"}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.GetGroupByPath(context.Background(), "a b/c d"); err != nil {
		t.Fatalf("GetGroupByPath with spaces: %v", err)
	}
	if h.gotEscaped != adminPathPrefix+"/realms/holos/group-by-path/a%20b/c%20d" {
		t.Errorf("escaped path = %q, want both spaces percent-encoded", h.gotEscaped)
	}
}

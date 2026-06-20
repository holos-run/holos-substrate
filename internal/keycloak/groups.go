package keycloak

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Group is the subset of a Keycloak group representation the reconcilers read
// back. Keycloak returns more fields; only the ones the controller uses are
// decoded.
type Group struct {
	// ID is the group's UUID.
	ID string `json:"id,omitempty"`
	// Name is the group's leaf name (not the full path).
	Name string `json:"name,omitempty"`
	// Path is the group's full path, e.g. /projects/my-project/roles/owner.
	Path string `json:"path,omitempty"`
	// SubGroups are the group's immediate children, populated by GetGroup (the
	// single-group endpoint embeds children) but not by group-by-path.
	SubGroups []Group `json:"subGroups,omitempty"`
}

// createGroupRequest is the body for POST .../groups and
// POST .../groups/{parentId}/children. Keycloak's GroupRepresentation accepts
// more, but only the leaf name is needed to create a node.
type createGroupRequest struct {
	Name string `json:"name"`
}

// normalizePath ensures a group path has exactly one leading slash and no
// trailing slash, so "projects/p/roles/owner", "/projects/p/roles/owner", and
// "/projects/p/roles/owner/" all canonicalize identically. The empty/root path
// canonicalizes to "".
func normalizePath(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return ""
	}
	return "/" + trimmed
}

// groupByPathPath builds the group-by-path lookup path. The Admin API expects
// the path WITHOUT a leading slash after group-by-path/, with each segment
// escaped, e.g. /admin/realms/{realm}/group-by-path/projects/my-project.
func (c *Client) groupByPathPath(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return c.adminPath("/group-by-path/" + strings.Join(segments, "/"))
}

// GetGroupByPath fetches the group at the given full path (e.g.
// projects/my-project/roles/owner) via
// GET /admin/realms/{realm}/group-by-path/{path}. A leading slash is optional.
// A missing group is returned as an *APIError reporting IsNotFound.
func (c *Client) GetGroupByPath(ctx context.Context, path string) (*Group, error) {
	g := &Group{}
	if err := c.doJSON(ctx, http.MethodGet, c.groupByPathPath(path), nil, g); err != nil {
		return nil, err
	}
	return g, nil
}

// GetGroup fetches a single group by its UUID via
// GET /admin/realms/{realm}/groups/{id}; the response embeds the group's
// immediate subGroups. A missing group is returned as an *APIError reporting
// IsNotFound.
func (c *Client) GetGroup(ctx context.Context, id string) (*Group, error) {
	g := &Group{}
	path := c.adminPath("/groups/" + url.PathEscape(id))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, g); err != nil {
		return nil, err
	}
	return g, nil
}

// CreateTopLevelGroup creates a top-level (realm-root) group named name via
// POST /admin/realms/{realm}/groups and returns its UUID (parsed from the
// Location header). When the group already exists Keycloak responds 409;
// CreateTopLevelGroup surfaces that as an *APIError reporting IsConflict so
// callers can treat a re-run as idempotent (use EnsureGroupByPath for the
// idempotent convenience).
func (c *Client) CreateTopLevelGroup(ctx context.Context, name string) (string, error) {
	return c.doCreate(ctx, c.adminPath("/groups"), createGroupRequest{Name: name})
}

// CreateChildGroup creates a child group named name under the parent group
// identified by parentID via
// POST /admin/realms/{realm}/groups/{parentId}/children, returning the child's
// UUID. An already-existing child is surfaced as an *APIError reporting
// IsConflict.
func (c *Client) CreateChildGroup(ctx context.Context, parentID, name string) (string, error) {
	path := c.adminPath("/groups/" + url.PathEscape(parentID) + "/children")
	return c.doCreate(ctx, path, createGroupRequest{Name: name})
}

// DeleteGroup deletes the group identified by id via
// DELETE /admin/realms/{realm}/groups/{id}. A missing group is returned as an
// *APIError reporting IsNotFound; use DeleteGroupByPathIfExists to treat that as
// success.
func (c *Client) DeleteGroup(ctx context.Context, id string) error {
	path := c.adminPath("/groups/" + url.PathEscape(id))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteGroupByPathIfExists deletes the group at the given full path, resolving
// it to a UUID first, and returns nil when the group is already absent — so the
// call is idempotent for cleanup and finalizer logic.
func (c *Client) DeleteGroupByPathIfExists(ctx context.Context, path string) error {
	g, err := c.GetGroupByPath(ctx, path)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	err = c.DeleteGroup(ctx, g.ID)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// EnsureGroupByPath idempotently ensures every node along the full path exists,
// creating any missing ancestor as a top-level or child group as appropriate,
// and returns the leaf group's UUID. It is the primary entry point for
// provisioning the nested tree projects/<p>/roles/* and projects/<p>/custodians/*
// (ADR-20): repeated calls converge without error, and a node another reconcile
// already created is reused rather than recreated.
//
// It resolves the deepest existing ancestor via group-by-path lookups, then
// creates each remaining segment in order. A 409 from a create (a concurrent
// reconcile won the race) is treated as benign: the now-existing node is
// re-resolved and the walk continues.
func (c *Client) EnsureGroupByPath(ctx context.Context, path string) (string, error) {
	normalized := normalizePath(path)
	if normalized == "" {
		return "", &APIError{StatusCode: http.StatusBadRequest, Method: http.MethodPost, Path: "groups", Message: "empty group path"}
	}

	// Fast path: the whole path already exists.
	if g, err := c.GetGroupByPath(ctx, normalized); err == nil {
		return g.ID, nil
	} else if !IsNotFound(err) {
		return "", err
	}

	segments := strings.Split(strings.TrimPrefix(normalized, "/"), "/")
	var parentID string
	var prefix string
	for i, seg := range segments {
		prefix += "/" + seg
		// Is this node already present? Resolve it and descend.
		if g, err := c.GetGroupByPath(ctx, prefix); err == nil {
			parentID = g.ID
			continue
		} else if !IsNotFound(err) {
			return "", err
		}

		// Create the missing node, tolerating a concurrent creator's 409 by
		// re-resolving it.
		id, err := c.createGroupNode(ctx, i, parentID, seg)
		if err != nil {
			if !IsConflict(err) {
				return "", err
			}
			g, gerr := c.GetGroupByPath(ctx, prefix)
			if gerr != nil {
				return "", gerr
			}
			id = g.ID
		}
		parentID = id
	}
	return parentID, nil
}

// createGroupNode creates one segment of a path walk: a top-level group when it
// is the first segment (index 0, no parent), otherwise a child under parentID.
func (c *Client) createGroupNode(ctx context.Context, index int, parentID, name string) (string, error) {
	if index == 0 {
		return c.CreateTopLevelGroup(ctx, name)
	}
	return c.CreateChildGroup(ctx, parentID, name)
}

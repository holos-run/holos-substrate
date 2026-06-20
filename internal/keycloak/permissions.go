package keycloak

import (
	"context"
	"net/http"
	"net/url"
)

// Fine-Grained Admin Permissions v2 (FGAP v2, Keycloak >= 26.2) realizes
// custodian delegation natively: a permission scoped to a role group grants a
// custodian group the manage-members / manage-membership scopes over it, so a
// custodian adds/removes the role group's members without realm-admin rights
// (ADR-20, "Custodian delegation — FGAP v2 group scope").
//
// FGAP v2 is built on Keycloak's Authorization Services hosted by a realm
// management client (the "admin-permissions" client). The reconciler programs
// the delegation as authorization objects on that client: a group resource for
// the role group, a group policy naming the custodian group, and a
// scope-permission binding the manage-members/manage-membership scopes to that
// policy. This file is the thin REST seam over those endpoints; the controller
// (ADR-20 Phase 4) sequences them. It exists because the design uses native
// delegation (custodianDelegation: fgap-v2 is the default); the controller-layer
// alternative needs none of it.

// Group permission scopes FGAP v2 defines for delegating membership management
// over a group.
const (
	// ScopeManageMembers permits adding/removing the group's direct members.
	ScopeManageMembers = "manage-members"
	// ScopeManageMembership permits managing the group's membership relations
	// (e.g. nested membership). The design grants both to a custodian.
	ScopeManageMembership = "manage-membership"
)

// AuthzResource is the subset of an Authorization Services resource
// representation the reconciler uses to register a group as a permission target
// on the admin-permissions client.
type AuthzResource struct {
	// ID is the resource's UUID (read-only).
	ID string `json:"_id,omitempty"`
	// Name is the resource name (the reconciler uses the role group's path).
	Name string `json:"name"`
	// Type is the resource type, "Groups" for a group resource.
	Type string `json:"type,omitempty"`
	// Scopes are the authorization scopes the resource exposes.
	Scopes []AuthzScope `json:"scopes,omitempty"`
}

// AuthzScope is an Authorization Services scope reference (by name).
type AuthzScope struct {
	// Name is the scope name, e.g. manage-members.
	Name string `json:"name"`
}

// GroupPolicy is the subset of an Authorization Services group policy the
// reconciler creates to name the custodian group(s) a permission applies to.
type GroupPolicy struct {
	// ID is the policy's UUID (read-only).
	ID string `json:"id,omitempty"`
	// Name is the policy name.
	Name string `json:"name"`
	// Type is the policy type, always "group" here.
	Type string `json:"type,omitempty"`
	// Groups are the group definitions the policy grants to.
	Groups []GroupPolicyMember `json:"groups,omitempty"`
}

// GroupPolicyMember references a group within a group policy by its UUID.
type GroupPolicyMember struct {
	// ID is the custodian group's UUID.
	ID string `json:"id"`
	// ExtendChildren, when true, also applies to the group's children.
	ExtendChildren bool `json:"extendChildren,omitempty"`
}

// ScopePermission is the subset of an Authorization Services scope-permission
// the reconciler creates to bind the manage-members/manage-membership scopes
// over a group resource to a custodian group policy.
type ScopePermission struct {
	// ID is the permission's UUID (read-only).
	ID string `json:"id,omitempty"`
	// Name is the permission name.
	Name string `json:"name"`
	// Resources are the resource IDs (or names) the permission applies to.
	Resources []string `json:"resources,omitempty"`
	// Scopes are the scope IDs (or names) granted.
	Scopes []string `json:"scopes,omitempty"`
	// Policies are the policy IDs evaluated to grant the permission.
	Policies []string `json:"policies,omitempty"`
}

// authzPath builds a path under the admin-permissions client's authorization
// resource server, /admin/realms/{realm}/clients/{permClientUUID}/authz/resource-server.
// The caller passes the permission (admin-permissions) client's UUID, resolved
// once via FindClientByClientID("admin-permissions").
func (c *Client) authzPath(permClientUUID, suffix string) string {
	return c.adminPath("/clients/" + url.PathEscape(permClientUUID) + "/authz/resource-server" + suffix)
}

// CreateGroupResource registers a group as an FGAP v2 permission resource on the
// admin-permissions client via POST .../authz/resource-server/resource,
// exposing the manage-members/manage-membership scopes, and returns its UUID. An
// already-existing resource is surfaced as an *APIError reporting IsConflict.
func (c *Client) CreateGroupResource(ctx context.Context, permClientUUID string, resource AuthzResource) (string, error) {
	if resource.Type == "" {
		resource.Type = "Groups"
	}
	return c.doCreateReturningBody(ctx, c.authzPath(permClientUUID, "/resource"), resource)
}

// CreateGroupPolicy creates an FGAP v2 group policy naming the custodian
// group(s) via POST .../authz/resource-server/policy/group, and returns its
// UUID. An already-existing policy is surfaced as an *APIError reporting
// IsConflict.
func (c *Client) CreateGroupPolicy(ctx context.Context, permClientUUID string, policy GroupPolicy) (string, error) {
	if policy.Type == "" {
		policy.Type = "group"
	}
	return c.doCreateReturningBody(ctx, c.authzPath(permClientUUID, "/policy/group"), policy)
}

// authzNamed is the minimal shape of an Authorization Services policy or
// permission returned by the search endpoints — just the id and name, enough to
// resolve a UUID by name when a create returned 409 (already exists) without an id.
type authzNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindPolicyByName returns the UUID of the group policy whose name exactly matches
// via GET .../authz/resource-server/policy?name=, or "" when none exists. Keycloak's
// name filter is a substring match, so the exact name is selected from the results.
// It lets the reconciler recover a policy's id after a 409 create so the delegation
// can be pruned later by id.
func (c *Client) FindPolicyByName(ctx context.Context, permClientUUID, name string) (string, error) {
	return c.findAuthzByName(ctx, permClientUUID, "/policy", name)
}

// FindPermissionByName returns the UUID of the scope permission whose name exactly
// matches via GET .../authz/resource-server/permission?name=, or "" when none
// exists. It is the permission analog of FindPolicyByName.
func (c *Client) FindPermissionByName(ctx context.Context, permClientUUID, name string) (string, error) {
	return c.findAuthzByName(ctx, permClientUUID, "/permission", name)
}

// findAuthzByName queries an Authorization Services search endpoint (policy or
// permission) by name and returns the id of the exact-name match, or "" when none
// matches. Keycloak's name filter is a substring match, so it selects the entry
// whose name equals name exactly.
func (c *Client) findAuthzByName(ctx context.Context, permClientUUID, kindSuffix, name string) (string, error) {
	q := url.Values{"name": {name}}
	path := c.authzPath(permClientUUID, kindSuffix+"?"+q.Encode())
	var results []authzNamed
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &results); err != nil {
		return "", err
	}
	for _, r := range results {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return "", nil
}

// CreateScopePermission creates an FGAP v2 scope permission binding the given
// scopes over the group resource to the custodian group policy via
// POST .../authz/resource-server/permission/scope, and returns its UUID. This
// is the object that actually grants a custodian group the
// manage-members/manage-membership scope over a role group (ADR-20). An
// already-existing permission is surfaced as an *APIError reporting IsConflict.
func (c *Client) CreateScopePermission(ctx context.Context, permClientUUID string, permission ScopePermission) (string, error) {
	return c.doCreateReturningBody(ctx, c.authzPath(permClientUUID, "/permission/scope"), permission)
}

// DeleteScopePermission deletes an FGAP v2 scope permission by its UUID via
// DELETE .../authz/resource-server/policy/{id}. In Keycloak's Authorization
// Services a scope permission is a policy, and the generic policy endpoint
// deletes any policy or permission by id (the type-specific
// /permission/scope/{id} delete is not exposed); creation, by contrast, uses
// the type-specific /permission/scope endpoint. A missing permission is
// returned as an *APIError reporting IsNotFound; use
// DeleteScopePermissionIfExists to treat that as success.
func (c *Client) DeleteScopePermission(ctx context.Context, permClientUUID, permissionID string) error {
	path := c.authzPath(permClientUUID, "/policy/"+url.PathEscape(permissionID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteScopePermissionIfExists deletes the scope permission and returns nil
// when it is already absent, so the call is idempotent for cleanup.
func (c *Client) DeleteScopePermissionIfExists(ctx context.Context, permClientUUID, permissionID string) error {
	err := c.DeleteScopePermission(ctx, permClientUUID, permissionID)
	if IsNotFound(err) {
		return nil
	}
	return err
}

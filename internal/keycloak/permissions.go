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
	return c.doCreate(ctx, c.authzPath(permClientUUID, "/resource"), resource)
}

// CreateGroupPolicy creates an FGAP v2 group policy naming the custodian
// group(s) via POST .../authz/resource-server/policy/group, and returns its
// UUID. An already-existing policy is surfaced as an *APIError reporting
// IsConflict.
func (c *Client) CreateGroupPolicy(ctx context.Context, permClientUUID string, policy GroupPolicy) (string, error) {
	if policy.Type == "" {
		policy.Type = "group"
	}
	return c.doCreate(ctx, c.authzPath(permClientUUID, "/policy/group"), policy)
}

// CreateScopePermission creates an FGAP v2 scope permission binding the given
// scopes over the group resource to the custodian group policy via
// POST .../authz/resource-server/permission/scope, and returns its UUID. This
// is the object that actually grants a custodian group the
// manage-members/manage-membership scope over a role group (ADR-20). An
// already-existing permission is surfaced as an *APIError reporting IsConflict.
func (c *Client) CreateScopePermission(ctx context.Context, permClientUUID string, permission ScopePermission) (string, error) {
	return c.doCreate(ctx, c.authzPath(permClientUUID, "/permission/scope"), permission)
}

// DeleteScopePermission deletes an FGAP v2 scope permission by its UUID via
// DELETE .../authz/resource-server/permission/scope/{id}. A missing permission
// is returned as an *APIError reporting IsNotFound; use
// DeleteScopePermissionIfExists to treat that as success.
func (c *Client) DeleteScopePermission(ctx context.Context, permClientUUID, permissionID string) error {
	path := c.authzPath(permClientUUID, "/permission/scope/"+url.PathEscape(permissionID))
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

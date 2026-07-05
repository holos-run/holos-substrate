package quay

import (
	"context"
	"net/http"
	"net/url"
)

// Quay organization prototype (default permission) role values. Quay's
// NewPrototype/PrototypeUpdate schemas enumerate exactly these three — note they
// are repository-permission roles (read/write/admin), distinct from the
// org-team roles in teams.go (member/creator/admin).
const (
	// PrototypeRoleRead grants pull (read) on repositories created in the org.
	PrototypeRoleRead = "read"
	// PrototypeRoleWrite grants push (write) on repositories created in the org.
	PrototypeRoleWrite = "write"
	// PrototypeRoleAdmin grants admin on repositories created in the org.
	PrototypeRoleAdmin = "admin"
)

// PrototypeDelegateTeam is the delegate kind for a team. Quay's delegate object
// accepts kind "team" or "user"; the controller only delegates default
// permissions to teams.
const PrototypeDelegateTeam = "team"

// Prototype is an organization default-permission prototype: a rule granting a
// delegate (here, always a team) a role on every repository subsequently created
// in the org. Only the fields the reconciler reads are decoded.
type Prototype struct {
	// ID is Quay's prototype identifier (a UUID), needed to update or delete it.
	ID string `json:"id"`
	// Role is the granted repository role: read, write, or admin.
	Role string `json:"role"`
	// Delegate is the principal the role is granted to — for this controller,
	// the team the default permission delegates to.
	Delegate PrototypeDelegate `json:"delegate"`
}

// PrototypeDelegate is the principal a prototype grants its role to. The
// controller always delegates to a team (Kind == "team"), so Name is the team
// name and the reconciler finds the prototype delegating to a given team by
// matching Kind/Name.
type PrototypeDelegate struct {
	// Name is the delegate team (or user) name.
	Name string `json:"name"`
	// Kind is the delegate kind: "team" or "user".
	Kind string `json:"kind"`
}

// listPrototypesResponse is the GET /api/v1/organization/{orgname}/prototypes
// envelope.
type listPrototypesResponse struct {
	Prototypes []Prototype `json:"prototypes"`
}

// createPrototypeRequest is the POST /api/v1/organization/{orgname}/prototypes
// body (Quay's NewPrototype schema): a required role and a required delegate
// {kind, name}.
type createPrototypeRequest struct {
	Role     string            `json:"role"`
	Delegate PrototypeDelegate `json:"delegate"`
}

// updatePrototypeRequest is the
// PUT /api/v1/organization/{orgname}/prototypes/{prototypeid} body (Quay's
// PrototypeUpdate schema): only the role is mutable.
type updatePrototypeRequest struct {
	Role string `json:"role"`
}

// prototypesPath builds the /api/v1/organization/{orgname}/prototypes collection
// path with the org segment escaped; callers append a prototype id for item
// operations.
func prototypesPath(org string) string {
	return organizationPath(org) + "/prototypes"
}

// ListPrototypes returns the organization's default-permission prototypes via
// GET /api/v1/organization/{orgname}/prototypes. Reconcilers use it to find the
// prototype delegating to a given team (matching Delegate.Kind/Name) before
// creating or updating one, so re-runs do not pile up duplicates. A missing
// organization is returned as an *APIError reporting IsNotFound.
func (c *Client) ListPrototypes(ctx context.Context, org string) ([]Prototype, error) {
	out := &listPrototypesResponse{}
	if err := c.doJSON(ctx, http.MethodGet, prototypesPath(org), nil, out); err != nil {
		return nil, err
	}
	return out.Prototypes, nil
}

// CreatePrototype creates a default-permission prototype granting role (read,
// write, or admin) to the team delegateTeam via
// POST /api/v1/organization/{orgname}/prototypes with body
// {"role": "<role>", "delegate": {"kind": "team", "name": "<delegateTeam>"}}. It
// returns the created Prototype, including the id Quay assigns (needed for later
// update/delete).
func (c *Client) CreatePrototype(ctx context.Context, org, role, delegateTeam string) (*Prototype, error) {
	req := createPrototypeRequest{
		Role:     role,
		Delegate: PrototypeDelegate{Name: delegateTeam, Kind: PrototypeDelegateTeam},
	}
	out := &Prototype{}
	if err := c.doJSON(ctx, http.MethodPost, prototypesPath(org), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdatePrototype sets the role on an existing prototype via
// PUT /api/v1/organization/{orgname}/prototypes/{prototypeid}. Only the role is
// mutable; the delegate is fixed at creation. A missing prototype is returned as
// an *APIError reporting IsNotFound.
func (c *Client) UpdatePrototype(ctx context.Context, org, prototypeID, role string) error {
	req := updatePrototypeRequest{Role: role}
	path := prototypesPath(org) + "/" + url.PathEscape(prototypeID)
	return c.doJSON(ctx, http.MethodPut, path, req, nil)
}

// DeletePrototype deletes the prototype prototypeID via
// DELETE /api/v1/organization/{orgname}/prototypes/{prototypeid}. A missing
// prototype is returned as an *APIError reporting IsNotFound; use
// DeletePrototypeIfExists to treat that as success.
func (c *Client) DeletePrototype(ctx context.Context, org, prototypeID string) error {
	path := prototypesPath(org) + "/" + url.PathEscape(prototypeID)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// DeletePrototypeIfExists deletes the prototype and returns nil when it is
// already absent, so the call is idempotent for cleanup and finalizer logic.
func (c *Client) DeletePrototypeIfExists(ctx context.Context, org, prototypeID string) error {
	return ignoreNotFound(c.DeletePrototype(ctx, org, prototypeID))
}

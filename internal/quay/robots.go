package quay

import (
	"context"
	"net/http"
	"net/url"
)

// OrganizationRobot is the subset of a Quay organization robot account the
// controller reads back. The controller uses a dedicated robot as a durable,
// server-side ownership marker: the robot's Description carries an opaque,
// controller-managed token (the owning CR's UID), so create/adopt/delete
// decisions can be keyed on the Quay org itself rather than solely on the CR's
// status.created field (ADR-19, "Ownership and the claim model").
type OrganizationRobot struct {
	// Name is the fully-qualified robot username, "<orgname>+<shortname>".
	Name string `json:"name"`
	// Description is the free-text field (Quay allows up to 255 chars). The
	// controller stores its opaque ownership token here.
	Description string `json:"description,omitempty"`
}

// createOrganizationRobotRequest is the PUT
// /api/v1/organization/{orgname}/robots/{shortname} body. Quay's
// CREATE_ROBOT_SCHEMA accepts an optional description (max 255 chars); the
// controller sets it to the ownership token.
type createOrganizationRobotRequest struct {
	Description string `json:"description,omitempty"`
}

// organizationRobotPath builds the
// /api/v1/organization/{orgname}/robots/{shortname} path with each segment
// escaped.
func organizationRobotPath(org, shortname string) string {
	return organizationPath(org) + "/robots/" + url.PathEscape(shortname)
}

// GetOrganizationRobot fetches the org robot {org}+{shortname} via
// GET /api/v1/organization/{orgname}/robots/{shortname}. A missing robot is
// returned as an *APIError reporting IsNotFound. Quay always returns the robot's
// description, so the caller reads the ownership token from the result.
func (c *Client) GetOrganizationRobot(ctx context.Context, org, shortname string) (*OrganizationRobot, error) {
	out := &OrganizationRobot{}
	if err := c.doJSON(ctx, http.MethodGet, organizationRobotPath(org, shortname), nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateOrganizationRobot creates the org robot {org}+{shortname} with the given
// description via PUT /api/v1/organization/{orgname}/robots/{shortname}.
//
// Quay's create-robot endpoint is NOT idempotent: re-creating an existing robot
// returns a 400 naming the duplicate, which CreateOrganizationRobot maps to an
// *APIError reporting IsConflict so a reconciler can treat a re-run as benign.
// Because Quay does not update an existing robot's description on PUT, the marker
// description is effectively write-once; callers verify an existing marker by
// GetOrganizationRobot rather than re-PUTting.
func (c *Client) CreateOrganizationRobot(ctx context.Context, org, shortname, description string) error {
	req := createOrganizationRobotRequest{Description: description}
	err := c.doJSON(ctx, http.MethodPut, organizationRobotPath(org, shortname), req, nil)
	return mapDuplicateToConflict(err)
}

// DeleteOrganizationRobot deletes the org robot {org}+{shortname} via
// DELETE /api/v1/organization/{orgname}/robots/{shortname}. A missing robot is
// returned as an *APIError reporting IsNotFound; use
// DeleteOrganizationRobotIfExists to treat that as success.
func (c *Client) DeleteOrganizationRobot(ctx context.Context, org, shortname string) error {
	return c.doJSON(ctx, http.MethodDelete, organizationRobotPath(org, shortname), nil, nil)
}

// DeleteOrganizationRobotIfExists deletes the org robot and returns nil when it
// is already absent, so the call is idempotent.
func (c *Client) DeleteOrganizationRobotIfExists(ctx context.Context, org, shortname string) error {
	return ignoreNotFound(c.DeleteOrganizationRobot(ctx, org, shortname))
}

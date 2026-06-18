package quay

import (
	"context"
	"net/http"
	"net/url"
)

// Organization is the subset of a Quay organization the reconciler reads back.
// Quay returns more fields; only the ones the controller uses are decoded.
type Organization struct {
	// Name is the organization (namespace) name.
	Name string `json:"name"`
	// Email is the organization contact email.
	Email string `json:"email,omitempty"`
	// IsOrgAdmin reports whether the authenticated user administers the org.
	IsOrgAdmin bool `json:"is_org_admin,omitempty"`
}

// createOrganizationRequest is the POST /api/v1/organization/ body.
type createOrganizationRequest struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// updateOrganizationRequest is the PUT /api/v1/organization/{orgname} body
// (Quay's changeOrganizationDetails). Quay 3.17.3 accepts email, invoice_email,
// invoice_email_address, and tag_expiration_s on this endpoint; the controller
// only programs the contact email, so only email is sent.
type updateOrganizationRequest struct {
	Email string `json:"email,omitempty"`
}

// CreateOrganization creates a Quay organization named name with the given
// contact email via POST /api/v1/organization/.
//
// Quay returns 201 with an empty body on success. When the organization already
// exists Quay responds 400 with a duplicate message; CreateOrganization maps
// that to an *APIError reporting IsConflict so reconcilers can treat a re-run as
// idempotent (use CreateOrganizationIfNotExists for the convenience wrapper).
func (c *Client) CreateOrganization(ctx context.Context, name, email string) error {
	req := createOrganizationRequest{Name: name, Email: email}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/organization/", req, nil)
	return mapDuplicateToConflict(err)
}

// CreateOrganizationIfNotExists creates the organization and returns nil when it
// already exists, so the call is idempotent across reconciler re-runs.
func (c *Client) CreateOrganizationIfNotExists(ctx context.Context, name, email string) error {
	err := c.CreateOrganization(ctx, name, email)
	if IsConflict(err) {
		return nil
	}
	return err
}

// GetOrganization fetches the organization named name via
// GET /api/v1/organization/{orgname}. A missing organization is returned as an
// *APIError reporting IsNotFound.
func (c *Client) GetOrganization(ctx context.Context, name string) (*Organization, error) {
	org := &Organization{}
	path := "/api/v1/organization/" + url.PathEscape(name)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, org); err != nil {
		return nil, err
	}
	return org, nil
}

// UpdateOrganization applies mutable organization fields to an existing org via
// PUT /api/v1/organization/{orgname} (Quay's changeOrganizationDetails). It
// programs the contact email; reconcilers call it only when GetOrganization
// reports drift from the desired email.
func (c *Client) UpdateOrganization(ctx context.Context, name, email string) error {
	req := updateOrganizationRequest{Email: email}
	path := "/api/v1/organization/" + url.PathEscape(name)
	return c.doJSON(ctx, http.MethodPut, path, req, nil)
}

// DeleteOrganization deletes the organization named name via
// DELETE /api/v1/organization/{orgname}. A missing organization is returned as
// an *APIError reporting IsNotFound; use DeleteOrganizationIfExists to treat
// that as success.
func (c *Client) DeleteOrganization(ctx context.Context, name string) error {
	path := "/api/v1/organization/" + url.PathEscape(name)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteOrganizationIfExists deletes the organization and returns nil when it is
// already absent, so the call is idempotent.
func (c *Client) DeleteOrganizationIfExists(ctx context.Context, name string) error {
	err := c.DeleteOrganization(ctx, name)
	if IsNotFound(err) {
		return nil
	}
	return err
}

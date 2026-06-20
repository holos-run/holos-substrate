package keycloak

import (
	"context"
	"net/http"
	"net/url"
)

// User is the subset of a Keycloak user representation the reconcilers read back
// and create. Keycloak returns more fields; only the ones the controller uses
// are decoded/sent.
type User struct {
	// ID is the user's UUID (read-only; assigned by Keycloak on create).
	ID string `json:"id,omitempty"`
	// Username is the user's login name.
	Username string `json:"username,omitempty"`
	// Email is the user's email address — the key first-broker-login auto-links
	// a federated identity to (ADR-20).
	Email string `json:"email,omitempty"`
	// Enabled reports whether the account may authenticate. No omitempty: a
	// desired false (a disabled account) must be sent rather than silently
	// dropped on create/update.
	Enabled bool `json:"enabled"`
	// EmailVerified marks the email trusted, which (with the IdP's Trust Email
	// flag) lets first-broker-login auto-link without a verification step. No
	// omitempty so an explicit false is sent.
	EmailVerified bool `json:"emailVerified"`
}

// FederatedIdentity is the link between a local user and an external identity
// provider account, created via
// POST /admin/realms/{realm}/users/{id}/federated-identity/{provider} so a
// first broker login auto-links to this pre-provisioned user (ADR-20).
type FederatedIdentity struct {
	// IdentityProvider is the IdP alias the link is for (the path's {provider}).
	IdentityProvider string `json:"identityProvider"`
	// UserID is the user's id at the external provider (the "sub" claim).
	UserID string `json:"userId"`
	// UserName is the user's username at the external provider.
	UserName string `json:"userName"`
}

// FindUserByEmail returns the user whose email exactly matches email via
// GET /admin/realms/{realm}/users?email=&exact=true, or nil when none exists
// (an absent user is not an error — the reconciler pre-creates on nil). Keycloak
// returns a (possibly empty) array; the first exact match is returned.
func (c *Client) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	return c.findUser(ctx, "email", email)
}

// FindUserByUsername returns the user whose username exactly matches username
// via GET /admin/realms/{realm}/users?username=&exact=true, or nil when none
// exists.
func (c *Client) FindUserByUsername(ctx context.Context, username string) (*User, error) {
	return c.findUser(ctx, "username", username)
}

// findUser performs an exact-match user query on the given field and returns the
// first result, or nil when the result set is empty.
func (c *Client) findUser(ctx context.Context, field, value string) (*User, error) {
	q := url.Values{field: {value}, "exact": {"true"}}
	path := c.adminPath("/users?" + q.Encode())
	var users []User
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &users); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, nil
	}
	return &users[0], nil
}

// CreateUser creates the user via POST /admin/realms/{realm}/users and returns
// the new user's UUID (parsed from the Location header). An already-existing
// user (same username/email) is surfaced as an *APIError reporting IsConflict so
// callers can treat a re-run as idempotent.
func (c *Client) CreateUser(ctx context.Context, user User) (string, error) {
	return c.doCreate(ctx, c.adminPath("/users"), user)
}

// AddUserToGroup adds the user to the group via
// PUT /admin/realms/{realm}/users/{id}/groups/{groupId}. Keycloak's PUT is
// idempotent — adding a user already in the group is a 204 — so no *IfNotExists
// wrapper is needed.
func (c *Client) AddUserToGroup(ctx context.Context, userID, groupID string) error {
	path := c.adminPath("/users/" + url.PathEscape(userID) + "/groups/" + url.PathEscape(groupID))
	return c.doJSON(ctx, http.MethodPut, path, nil, nil)
}

// RemoveUserFromGroup removes the user from the group via
// DELETE /admin/realms/{realm}/users/{id}/groups/{groupId}. A user not in the
// group is returned as an *APIError reporting IsNotFound; use
// RemoveUserFromGroupIfMember to treat that as success.
func (c *Client) RemoveUserFromGroup(ctx context.Context, userID, groupID string) error {
	path := c.adminPath("/users/" + url.PathEscape(userID) + "/groups/" + url.PathEscape(groupID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// RemoveUserFromGroupIfMember removes the user from the group and returns nil
// when the user is already not a member, so the call is idempotent.
func (c *Client) RemoveUserFromGroupIfMember(ctx context.Context, userID, groupID string) error {
	err := c.RemoveUserFromGroup(ctx, userID, groupID)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// CreateFederatedIdentity links the local user to an external identity-provider
// account via POST /admin/realms/{realm}/users/{id}/federated-identity/{provider},
// which is what lets a first broker login auto-link to this pre-provisioned user
// rather than creating a duplicate (ADR-20). The link.IdentityProvider must
// match provider. An already-present link is surfaced as an *APIError reporting
// IsConflict; use CreateFederatedIdentityIfNotExists to treat a *matching*
// existing link as success.
func (c *Client) CreateFederatedIdentity(ctx context.Context, userID, provider string, link FederatedIdentity) error {
	path := c.adminPath("/users/" + url.PathEscape(userID) + "/federated-identity/" + url.PathEscape(provider))
	_, err := c.doCreate(ctx, path, link)
	return err
}

// ListFederatedIdentities returns the user's existing federated-identity links
// via GET /admin/realms/{realm}/users/{id}/federated-identity, so a reconciler
// can tell whether a conflict on create means "already linked to the same
// upstream account" (benign) or "linked to a different account" (drift).
func (c *Client) ListFederatedIdentities(ctx context.Context, userID string) ([]FederatedIdentity, error) {
	path := c.adminPath("/users/" + url.PathEscape(userID) + "/federated-identity")
	var links []FederatedIdentity
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &links); err != nil {
		return nil, err
	}
	return links, nil
}

// CreateFederatedIdentityIfNotExists creates the federated-identity link and,
// on a 409 conflict, returns nil ONLY when an existing link for the same
// provider already points at the same upstream userId — i.e. the desired state
// already holds. A conflict where the provider is bound to a *different* userId
// is surfaced (not swallowed), because silently reporting success would leave
// the Keycloak user federated to the wrong external identity (the auto-link
// would never reach the intended account). This makes the call idempotent
// across re-runs without masking a mis-link.
func (c *Client) CreateFederatedIdentityIfNotExists(ctx context.Context, userID, provider string, link FederatedIdentity) error {
	err := c.CreateFederatedIdentity(ctx, userID, provider, link)
	if !IsConflict(err) {
		return err
	}
	existing, lerr := c.ListFederatedIdentities(ctx, userID)
	if lerr != nil {
		return lerr
	}
	for _, e := range existing {
		if e.IdentityProvider != provider {
			continue
		}
		if e.UserID == link.UserID {
			return nil // same upstream account already linked: desired state holds.
		}
		// Provider already bound to a DIFFERENT upstream account: surface the
		// conflict rather than masking a mis-link.
		return err
	}
	// 409 reported but no link for this provider is present: surface the conflict
	// rather than silently succeeding.
	return err
}

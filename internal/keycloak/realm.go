package keycloak

import (
	"context"
	"net/http"
)

// Realm is the subset of a Keycloak realm representation the controller reads
// back. Keycloak returns far more; only the fields the reconciler keys on are
// decoded. It is the result of the reachability probe the Instance
// reconciler runs.
type Realm struct {
	// ID is the realm's internal id (often equal to Realm for the well-known
	// realms).
	ID string `json:"id,omitempty"`
	// Realm is the realm name (e.g. holos).
	Realm string `json:"realm,omitempty"`
	// Enabled reports whether the realm is enabled.
	Enabled bool `json:"enabled,omitempty"`
}

// GetRealm fetches the realm's top-level representation via
// GET /admin/realms/{realm}. It is the Instance reconciler's reachability
// probe: a successful call proves both that the admin credential authenticated
// (the request carries a fresh bearer token obtained via the client_credentials
// grant) and that the configured realm exists and is reachable over the
// (optionally caBundle-trusted) TLS connection. A missing realm is returned as an
// *APIError reporting IsNotFound; an auth failure surfaces as an *APIError from
// the token exchange.
func (c *Client) GetRealm(ctx context.Context) (*Realm, error) {
	realm := &Realm{}
	if err := c.doJSON(ctx, http.MethodGet, c.adminPath(""), nil, realm); err != nil {
		return nil, err
	}
	return realm, nil
}

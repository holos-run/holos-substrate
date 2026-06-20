package keycloak

import (
	"context"
	"net/http"
	"net/url"
)

// ProtocolMapperClientRole is the Keycloak protocol-mapper type that folds a
// client's client-role names into a token claim. The platform uses it (as
// quay-client-roles) to emit the my-project-<role> client roles into the shared
// groups claim (ADR-20); the project KeycloakClient reconciler ensures one of
// these scoped to its own clientId.
const ProtocolMapperClientRole = "oidc-usermodel-client-role-mapper"

// OIDCClient is the subset of a Keycloak client representation the reconcilers
// read back and create. It is the Keycloak OIDC client, distinct from this
// package's own *Client (the API client).
type OIDCClient struct {
	// ID is the client's UUID (read-only; assigned by Keycloak on create). It is
	// the {clientUUID} path segment of every per-client admin endpoint, distinct
	// from ClientID.
	ID string `json:"id,omitempty"`
	// ClientID is the human/URL identifier, e.g. https://quay.holos.localhost —
	// the value find-by-clientId queries on.
	ClientID string `json:"clientId,omitempty"`
	// Name is the client's display name.
	Name string `json:"name,omitempty"`
	// Enabled reports whether the client may be used.
	Enabled bool `json:"enabled,omitempty"`
	// PublicClient is true for a public client, false for confidential.
	PublicClient bool `json:"publicClient,omitempty"`
	// RedirectURIs are the client's allowed redirect URIs.
	RedirectURIs []string `json:"redirectUris,omitempty"`
	// WebOrigins are the client's allowed CORS web origins.
	WebOrigins []string `json:"webOrigins,omitempty"`
}

// ProtocolMapper is the subset of a client protocol-mapper representation the
// reconcilers read back and create.
type ProtocolMapper struct {
	// ID is the mapper's UUID (read-only).
	ID string `json:"id,omitempty"`
	// Name is the mapper's name within the client.
	Name string `json:"name"`
	// Protocol is the mapper's protocol, e.g. openid-connect.
	Protocol string `json:"protocol"`
	// ProtocolMapper is the mapper type, e.g. ProtocolMapperClientRole.
	ProtocolMapper string `json:"protocolMapper"`
	// Config is the mapper's type-specific configuration (string values, per the
	// Admin API representation).
	Config map[string]string `json:"config,omitempty"`
}

// ClientRole is the subset of a client-role representation the reconcilers read
// back, create, and assign to a group. The my-project-<role> values are client
// roles on the consumer client (ADR-20).
type ClientRole struct {
	// ID is the role's UUID (read-only; needed to assign the role to a group).
	ID string `json:"id,omitempty"`
	// Name is the role name, e.g. my-project-owner.
	Name string `json:"name"`
	// Description is the free-text role description.
	Description string `json:"description,omitempty"`
	// ContainerID is the owning client's UUID (set by Keycloak on read).
	ContainerID string `json:"containerId,omitempty"`
	// ClientRole marks the role as a client (not realm) role on read.
	ClientRole bool `json:"clientRole,omitempty"`
}

// FindClientByClientID returns the OIDC client whose clientId exactly matches
// clientID via GET /admin/realms/{realm}/clients?clientId=, or nil when none
// exists (an absent client is not an error). The clients query already matches
// clientId exactly, so the first result is returned.
func (c *Client) FindClientByClientID(ctx context.Context, clientID string) (*OIDCClient, error) {
	q := url.Values{"clientId": {clientID}}
	path := c.adminPath("/clients?" + q.Encode())
	var clients []OIDCClient
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &clients); err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	return &clients[0], nil
}

// CreateClient creates the OIDC client via POST /admin/realms/{realm}/clients
// and returns the new client's UUID (parsed from the Location header). An
// already-existing client (same clientId) is surfaced as an *APIError reporting
// IsConflict.
func (c *Client) CreateClient(ctx context.Context, client OIDCClient) (string, error) {
	return c.doCreate(ctx, c.adminPath("/clients"), client)
}

// UpdateClient applies the client representation to an existing client via
// PUT /admin/realms/{realm}/clients/{clientUUID}, where clientUUID is the
// client's id (not its clientId). Keycloak's PUT is a full update; callers pass
// the desired representation (typically read, mutated, written back).
func (c *Client) UpdateClient(ctx context.Context, clientUUID string, client OIDCClient) error {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID))
	return c.doJSON(ctx, http.MethodPut, path, client, nil)
}

// ListProtocolMappers returns the client's protocol mappers via
// GET /admin/realms/{realm}/clients/{clientUUID}/protocol-mappers/models, so a
// reconciler can detect whether the desired client-role mapper is already
// present before creating it.
func (c *Client) ListProtocolMappers(ctx context.Context, clientUUID string) ([]ProtocolMapper, error) {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/protocol-mappers/models")
	var mappers []ProtocolMapper
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &mappers); err != nil {
		return nil, err
	}
	return mappers, nil
}

// CreateProtocolMapper adds a protocol mapper to the client via
// POST /admin/realms/{realm}/clients/{clientUUID}/protocol-mappers/models. An
// already-existing mapper (same name) is surfaced as an *APIError reporting
// IsConflict; use EnsureClientRoleMapper for the idempotent ensure of the
// client-role mapper specifically.
func (c *Client) CreateProtocolMapper(ctx context.Context, clientUUID string, mapper ProtocolMapper) error {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/protocol-mappers/models")
	_, err := c.doCreate(ctx, path, mapper)
	return err
}

// EnsureClientRoleMapper idempotently ensures an oidc-usermodel-client-role-mapper
// named name is present on the client, scoped to emit clientID's client roles
// into the shared groups claim — the quay-client-roles shape retargeted to this
// client (ADR-20). It no-ops when a mapper of that name already exists (checked
// via ListProtocolMappers, and tolerating a concurrent creator's 409).
//
// The config mirrors the platform's quay-client-roles mapper: the target client
// for client-role mapping, the claim name (groups), token-claim inclusion, and
// multivalued string output.
func (c *Client) EnsureClientRoleMapper(ctx context.Context, clientUUID, name, clientID, claimName string) error {
	mappers, err := c.ListProtocolMappers(ctx, clientUUID)
	if err != nil {
		return err
	}
	for _, m := range mappers {
		if m.Name == name {
			return nil
		}
	}
	mapper := ProtocolMapper{
		Name:           name,
		Protocol:       "openid-connect",
		ProtocolMapper: ProtocolMapperClientRole,
		Config: map[string]string{
			"usermodel.clientRoleMapping.clientId": clientID,
			"claim.name":                           claimName,
			"jsonType.label":                       "String",
			"multivalued":                          "true",
			"id.token.claim":                       "true",
			"access.token.claim":                   "true",
			"userinfo.token.claim":                 "true",
		},
	}
	err = c.CreateProtocolMapper(ctx, clientUUID, mapper)
	if IsConflict(err) {
		return nil
	}
	return err
}

// ListClientRoles returns the client's roles via
// GET /admin/realms/{realm}/clients/{clientUUID}/roles.
func (c *Client) ListClientRoles(ctx context.Context, clientUUID string) ([]ClientRole, error) {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/roles")
	var roles []ClientRole
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

// GetClientRole fetches one client role by name via
// GET /admin/realms/{realm}/clients/{clientUUID}/roles/{roleName}, returning the
// representation (notably its ID) needed to assign the role to a group. A
// missing role is returned as an *APIError reporting IsNotFound.
func (c *Client) GetClientRole(ctx context.Context, clientUUID, roleName string) (*ClientRole, error) {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/roles/" + url.PathEscape(roleName))
	role := &ClientRole{}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, role); err != nil {
		return nil, err
	}
	return role, nil
}

// CreateClientRole creates a client role via
// POST /admin/realms/{realm}/clients/{clientUUID}/roles. Keycloak returns 201
// with no parseable id here (the role is addressed by name), so this returns no
// id. An already-existing role is surfaced as an *APIError reporting IsConflict;
// use CreateClientRoleIfNotExists to treat that as success.
func (c *Client) CreateClientRole(ctx context.Context, clientUUID string, role ClientRole) error {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/roles")
	_, err := c.doCreate(ctx, path, role)
	return err
}

// CreateClientRoleIfNotExists creates the client role and returns nil when it
// already exists, so the call is idempotent across reconciler re-runs.
func (c *Client) CreateClientRoleIfNotExists(ctx context.Context, clientUUID string, role ClientRole) error {
	err := c.CreateClientRole(ctx, clientUUID, role)
	if IsConflict(err) {
		return nil
	}
	return err
}

// AssignClientRoleToGroup grants the client role to the group via
// POST /admin/realms/{realm}/groups/{groupId}/role-mappings/clients/{clientUUID}
// with a body of one role representation. The role must carry its ID and Name
// (resolve via GetClientRole). This is the join that makes a member of the role
// group hold the my-project-<role> client role, which the client's client-role
// mapper then emits into the groups claim (ADR-20). Re-assigning an already-held
// role is idempotent on Keycloak's side (204).
func (c *Client) AssignClientRoleToGroup(ctx context.Context, groupID, clientUUID string, role ClientRole) error {
	path := c.adminPath("/groups/" + url.PathEscape(groupID) + "/role-mappings/clients/" + url.PathEscape(clientUUID))
	return c.doJSON(ctx, http.MethodPost, path, []ClientRole{role}, nil)
}

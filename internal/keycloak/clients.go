package keycloak

import (
	"context"
	"net/http"
	"net/url"
)

// RawClient is a Keycloak ClientRepresentation kept as an opaque field map so a
// read-modify-write preserves every field the controller does not manage —
// protocol, clientAuthenticatorType, attributes (including PKCE config), service
// account / flow flags, default scopes, and anything else Keycloak or another
// owner set. The typed OIDCClient is a lossy view sufficient for create and for
// reading the handful of fields the controller keys on; a full PUT update must
// go through RawClient to avoid clobbering unmanaged fields.
type RawClient map[string]any

// ClientFields are the client fields the controller manages on an update. Only
// the non-nil fields are applied onto a fetched RawClient, so an update touches
// exactly the desired keys and leaves the rest of the representation intact.
type ClientFields struct {
	// Name, when non-nil, sets the client's display name.
	Name *string
	// Enabled, when non-nil, sets the enabled flag.
	Enabled *bool
	// PublicClient, when non-nil, sets public (true) vs confidential (false).
	PublicClient *bool
	// RedirectURIs, when non-nil, replaces the redirect URI list.
	RedirectURIs *[]string
	// WebOrigins, when non-nil, replaces the CORS web-origin list.
	WebOrigins *[]string
	// Attributes, when non-nil, MERGES the given attribute keys onto the client's
	// existing attributes map (rather than replacing it), so a managed attribute
	// such as the PKCE code-challenge method is set without clobbering unmanaged
	// attributes Keycloak or another owner stored. Only the supplied keys are
	// written.
	Attributes map[string]string
}

// apply writes the set (non-nil) fields onto raw, leaving every other key
// untouched. Attributes are merged key-by-key onto the existing attributes map
// so unmanaged attribute keys survive.
func (f ClientFields) apply(raw RawClient) {
	if f.Name != nil {
		raw["name"] = *f.Name
	}
	if f.Enabled != nil {
		raw["enabled"] = *f.Enabled
	}
	if f.PublicClient != nil {
		raw["publicClient"] = *f.PublicClient
	}
	if f.RedirectURIs != nil {
		raw["redirectUris"] = *f.RedirectURIs
	}
	if f.WebOrigins != nil {
		raw["webOrigins"] = *f.WebOrigins
	}
	if f.Attributes != nil {
		attrs, _ := raw["attributes"].(map[string]any)
		if attrs == nil {
			attrs = map[string]any{}
		}
		for k, v := range f.Attributes {
			attrs[k] = v
		}
		raw["attributes"] = attrs
	}
}

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
	// Enabled reports whether the client may be used. No omitempty: UpdateClient
	// is a full PUT, so a desired false must be sent to disable a client rather
	// than silently dropped.
	Enabled bool `json:"enabled"`
	// PublicClient is true for a public client, false for confidential. No
	// omitempty: a confidential client needs publicClient:false on the wire, and
	// the full-PUT update must be able to converge a public client to
	// confidential.
	PublicClient bool `json:"publicClient"`
	// RedirectURIs are the client's allowed redirect URIs.
	RedirectURIs []string `json:"redirectUris,omitempty"`
	// WebOrigins are the client's allowed CORS web origins.
	WebOrigins []string `json:"webOrigins,omitempty"`
	// Attributes carries the client's attribute map (e.g. the PKCE
	// pkce.code.challenge.method). Set on create to program managed attributes;
	// omitempty so an unset map is not sent.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// PKCECodeChallengeMethodAttr is the Keycloak client-attribute key holding the
// PKCE code-challenge method (e.g. "S256"). The KeycloakClient reconciler sets it
// to S256 on public clients so the authorization-code flow requires PKCE, per the
// platform's public-client guardrail (keycloak-clients.md).
const PKCECodeChallengeMethodAttr = "pkce.code.challenge.method"

// PKCEMethodS256 is the SHA-256 PKCE code-challenge method value.
const PKCEMethodS256 = "S256"

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

// ClientSecret is the subset of a Keycloak CredentialRepresentation returned by
// the client-secret endpoint: the generated confidential-client secret value the
// reconciler delivers to the consumer's Secret (ADR-20).
type ClientSecret struct {
	// Type is the credential type, e.g. "secret".
	Type string `json:"type,omitempty"`
	// Value is the generated client secret string.
	Value string `json:"value,omitempty"`
}

// GetClientSecret fetches a confidential client's current secret via
// GET /admin/realms/{realm}/clients/{clientUUID}/client-secret, returning the
// generated secret value. Keycloak generates a secret for a confidential client
// on creation, so the reconciler reads it back (rather than regenerating) to
// deliver it generate-once to the consumer's Secret. A missing client is
// returned as an *APIError reporting IsNotFound.
func (c *Client) GetClientSecret(ctx context.Context, clientUUID string) (*ClientSecret, error) {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/client-secret")
	secret := &ClientSecret{}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// DeleteClient deletes the OIDC client identified by UUID via
// DELETE /admin/realms/{realm}/clients/{clientUUID}. A missing client is
// returned as an *APIError reporting IsNotFound; use
// DeleteClientByClientIDIfExists to treat that as success (the finalizer's
// idempotent cleanup).
func (c *Client) DeleteClient(ctx context.Context, clientUUID string) error {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteClientByClientIDIfExists resolves the client by its clientId and deletes
// it, returning nil when no such client exists — so the finalizer's cleanup is
// idempotent across re-runs.
func (c *Client) DeleteClientByClientIDIfExists(ctx context.Context, clientID string) error {
	oidc, err := c.FindClientByClientID(ctx, clientID)
	if err != nil {
		return err
	}
	if oidc == nil {
		return nil
	}
	err = c.DeleteClient(ctx, oidc.ID)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// GetClientRaw fetches the full ClientRepresentation by UUID via
// GET /admin/realms/{realm}/clients/{clientUUID} as an opaque RawClient, so a
// caller can mutate only the managed fields and PUT it back without dropping the
// rest. A missing client is returned as an *APIError reporting IsNotFound.
func (c *Client) GetClientRaw(ctx context.Context, clientUUID string) (RawClient, error) {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID))
	raw := RawClient{}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// UpdateClientRaw replaces the client via PUT /admin/realms/{realm}/clients/{clientUUID}
// with the given full ClientRepresentation. Keycloak's PUT is a full update, so
// raw must be a complete representation (typically one fetched by GetClientRaw
// and then mutated) — not a sparse subset — otherwise unmanaged fields would be
// dropped. Prefer UpdateClientFields, which does the fetch-merge-PUT safely.
func (c *Client) UpdateClientRaw(ctx context.Context, clientUUID string, raw RawClient) error {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID))
	return c.doJSON(ctx, http.MethodPut, path, raw, nil)
}

// UpdateClientFields applies only the set (non-nil) fields of f to the client,
// losslessly: it fetches the current full representation via GetClientRaw,
// overwrites just the managed keys, and PUTs the merged representation back. This
// is the safe update path — it never drops Keycloak ClientRepresentation fields
// the controller does not manage (protocol, clientAuthenticatorType, attributes
// such as PKCE config, service-account/flow flags, default scopes), which a full
// PUT of the lossy OIDCClient subset would clobber.
func (c *Client) UpdateClientFields(ctx context.Context, clientUUID string, f ClientFields) error {
	raw, err := c.GetClientRaw(ctx, clientUUID)
	if err != nil {
		return err
	}
	f.apply(raw)
	return c.UpdateClientRaw(ctx, clientUUID, raw)
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

// UpdateProtocolMapper replaces an existing mapper's definition via
// PUT /admin/realms/{realm}/clients/{clientUUID}/protocol-mappers/models/{id}.
// The mapper's ID must be set (the id of the mapper being updated).
func (c *Client) UpdateProtocolMapper(ctx context.Context, clientUUID string, mapper ProtocolMapper) error {
	path := c.adminPath("/clients/" + url.PathEscape(clientUUID) + "/protocol-mappers/models/" + url.PathEscape(mapper.ID))
	return c.doJSON(ctx, http.MethodPut, path, mapper, nil)
}

// clientRoleMapperConfig builds the desired oidc-usermodel-client-role-mapper
// config: the target client for client-role mapping, the claim name, and the
// token-claim/multivalued flags — the platform's quay-client-roles shape
// retargeted to clientID (ADR-20).
func clientRoleMapperConfig(clientID, claimName string) map[string]string {
	return map[string]string{
		"usermodel.clientRoleMapping.clientId": clientID,
		"claim.name":                           claimName,
		"jsonType.label":                       "String",
		"multivalued":                          "true",
		"id.token.claim":                       "true",
		"access.token.claim":                   "true",
		"userinfo.token.claim":                 "true",
	}
}

// mapperMatchesDesired reports whether an existing mapper already has the desired
// type and every desired config key/value, so a converged mapper is left
// untouched. Extra config keys Keycloak adds on its own are ignored; only the
// keys the platform programs must match.
func mapperMatchesDesired(existing ProtocolMapper, desiredType string, desiredConfig map[string]string) bool {
	if existing.ProtocolMapper != desiredType {
		return false
	}
	for k, v := range desiredConfig {
		if existing.Config[k] != v {
			return false
		}
	}
	return true
}

// EnsureClientRoleMapper idempotently ensures an oidc-usermodel-client-role-mapper
// named name is present on the client AND configured to emit clientID's client
// roles into the claimName claim — the quay-client-roles shape retargeted to
// this client (ADR-20).
//
// It is desired-state aware rather than name-only: a same-named mapper whose
// type or programmed config drifts (e.g. a stale or hand-edited mapper pointing
// at the wrong clientId or claim) is PUT back to the desired definition rather
// than left broken while reporting success. When no mapper of that name exists
// it creates one, tolerating a concurrent creator's 409 by re-reading and
// converging.
func (c *Client) EnsureClientRoleMapper(ctx context.Context, clientUUID, name, clientID, claimName string) error {
	desiredConfig := clientRoleMapperConfig(clientID, claimName)
	mappers, err := c.ListProtocolMappers(ctx, clientUUID)
	if err != nil {
		return err
	}
	for _, m := range mappers {
		if m.Name != name {
			continue
		}
		// A same-named mapper exists: leave it alone only when it already matches
		// the desired type and config; otherwise PUT the corrected definition.
		if mapperMatchesDesired(m, ProtocolMapperClientRole, desiredConfig) {
			return nil
		}
		corrected := ProtocolMapper{
			ID:             m.ID,
			Name:           name,
			Protocol:       "openid-connect",
			ProtocolMapper: ProtocolMapperClientRole,
			Config:         desiredConfig,
		}
		return c.UpdateProtocolMapper(ctx, clientUUID, corrected)
	}

	mapper := ProtocolMapper{
		Name:           name,
		Protocol:       "openid-connect",
		ProtocolMapper: ProtocolMapperClientRole,
		Config:         desiredConfig,
	}
	err = c.CreateProtocolMapper(ctx, clientUUID, mapper)
	if !IsConflict(err) {
		return err
	}
	// A concurrent reconcile created it: re-read and converge if needed.
	mappers, lerr := c.ListProtocolMappers(ctx, clientUUID)
	if lerr != nil {
		return lerr
	}
	for _, m := range mappers {
		if m.Name != name {
			continue
		}
		if mapperMatchesDesired(m, ProtocolMapperClientRole, desiredConfig) {
			return nil
		}
		corrected := ProtocolMapper{
			ID:             m.ID,
			Name:           name,
			Protocol:       "openid-connect",
			ProtocolMapper: ProtocolMapperClientRole,
			Config:         desiredConfig,
		}
		return c.UpdateProtocolMapper(ctx, clientUUID, corrected)
	}
	// The 409 was reported but no such mapper is present on re-read; surface the
	// original conflict rather than silently succeeding.
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

// ListGroupClientRoles returns the client roles currently mapped to the group for
// the given client via
// GET /admin/realms/{realm}/groups/{groupId}/role-mappings/clients/{clientUUID},
// so a reconciler can detect and prune client roles that are no longer desired
// (reconciling conferral to the desired set rather than add-only).
func (c *Client) ListGroupClientRoles(ctx context.Context, groupID, clientUUID string) ([]ClientRole, error) {
	path := c.adminPath("/groups/" + url.PathEscape(groupID) + "/role-mappings/clients/" + url.PathEscape(clientUUID))
	var roles []ClientRole
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

// RemoveClientRoleFromGroup unassigns the client role from the group via
// DELETE /admin/realms/{realm}/groups/{groupId}/role-mappings/clients/{clientUUID}
// with a body of one role representation. The role must carry its ID and Name. It
// is the inverse of AssignClientRoleToGroup; Keycloak treats removing a role the
// group does not hold as a 204 no-op, so the call is idempotent.
func (c *Client) RemoveClientRoleFromGroup(ctx context.Context, groupID, clientUUID string, role ClientRole) error {
	path := c.adminPath("/groups/" + url.PathEscape(groupID) + "/role-mappings/clients/" + url.PathEscape(clientUUID))
	return c.doJSON(ctx, http.MethodDelete, path, []ClientRole{role}, nil)
}

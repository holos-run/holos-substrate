package keycloak

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/holos-run/holos-substrate/internal/keycloak"
)

// fakeKeycloakClient is a recording, in-memory stand-in for the Keycloak Admin
// API the reconcilers drive. It satisfies both InstanceClient and GroupClient so a
// test injects it via the reconciler's client factory, exercising the full
// reconcile loop without HTTP or a live Keycloak. It records every call so tests
// can assert create-vs-adopt behavior, role conferral, custodian wiring, and
// idempotent delete.
type fakeKeycloakClient struct {
	mu sync.Mutex

	// realmReachable controls GetRealm: when false it returns a 404 *APIError so a
	// reconciler's reachability probe fails as it would against an unreachable realm.
	realmReachable bool
	// realmErr, when non-nil, is returned by GetRealm regardless of reachability —
	// used to simulate an auth/transport failure (a non-404 error).
	realmErr error

	// groups is the set of group paths that already exist in the fake. A path is
	// stored normalized (leading slash, no trailing slash). The mapped value is the
	// group's synthetic UUID.
	groups map[string]string
	// nextGroupID is the monotonically-increasing source of synthetic group UUIDs.
	nextGroupID int

	// clients maps a clientId to its synthetic UUID, modeling FindClientByClientID.
	clients map[string]string
	// clientObjects stores the current managed fields for each clientId so
	// FindClientByClientID reads back creates and updates.
	clientObjects map[string]keycloak.OIDCClient
	// clientRoles records, per "<clientUUID>/<role>", a synthetic role UUID,
	// modeling GetClientRole.
	clientRoles map[string]string
	// roleAssignments records each "<groupID>/<clientUUID>/<role>" the reconciler
	// assigned, so a test asserts conferral happened.
	roleAssignments map[string]bool

	// fgapResources/fgapPolicies/fgapPermissions record the custodian-delegation
	// objects created on the admin-permissions client, so a test asserts the FGAP v2
	// wiring ran.
	fgapResources   []string
	fgapPolicies    []string
	fgapPermissions []string
	fgapDeletes     []string

	// groupGetNotFoundOnce, when an entry is true for a normalized path, makes the
	// next GetGroupByPath for that path report NotFound exactly once (then the entry
	// is cleared). It simulates the create-race window: the reconciler's initial GET
	// 404s, but EnsureGroupByPathCreated then finds the group already present
	// (created=false) because a concurrent actor created it.
	groupGetNotFoundOnce map[string]bool

	// assignRoleErrFor, when an entry "<clientUUID>/<role>" is true, makes
	// AssignClientRoleToGroup return an error for that role — used to simulate a
	// partial failure where an earlier role assigns but a later one fails.
	assignRoleErrFor map[string]bool

	// ensureErr, when non-nil, is returned by EnsureGroupByPathCreated to simulate a
	// create failure.
	ensureErr error
	// deleteErr, when non-nil, is returned by DeleteGroupByPathIfExists.
	deleteErr error

	// Recorded calls, in order, e.g. "Ensure:/projects/p/roles/owner".
	calls []string

	// gotCABundle records the caBundle the reconciler's factory was last invoked
	// with, so a test asserts the instance's CABundle is threaded through.
	gotCABundle []byte

	// users maps an email to the synthetic user, modeling FindUserByEmail/CreateUser.
	users map[string]*keycloak.User
	// nextUserID is the monotonically-increasing source of synthetic user UUIDs.
	nextUserID int
	// createUserCount counts CreateUser calls, so a test asserts a present user is
	// reused rather than duplicated (no second create).
	createUserCount int
	// groupMembers records each "<userID>/<groupID>" the reconciler joined, so a
	// test asserts membership assignment and pruning.
	groupMembers map[string]bool
	// listUserGroupsErr, when non-nil, is returned by ListUserGroups to simulate
	// a remote read failure after status already has a validation timestamp.
	listUserGroupsErr error
	// listUserGroupsErrFor returns an error for one user ID while allowing earlier
	// users to converge, so tests can exercise partial mutation failures.
	listUserGroupsErrFor map[string]error
	// federatedLinks records, per "<userID>/<provider>", the upstream subject
	// (userId) of the link created, so a test asserts the link was made and that the
	// subject-verified prune respects an out-of-band recreated link.
	federatedLinks map[string]string
	// lastUpdateFields records, per clientUUID, the most recent ClientFields passed
	// to UpdateClientFields, so a test asserts PKCE set/removal on update.
	lastUpdateFields map[string]keycloak.ClientFields

	// clientRolesByClient records, per "<clientUUID>", the set of role names
	// created on that client, so a test asserts client-role convergence.
	createdClientRoles map[string]map[string]bool
	// roleMappers records each clientUUID the client-role mapper was ensured on.
	roleMappers map[string]bool
	// clientSecrets maps a clientUUID to the generated confidential secret value
	// GetClientSecret returns.
	clientSecrets map[string]string
	// deletedClients records each clientId DeleteClientByClientIDIfExists removed.
	deletedClients []string
	// createdClientAttrs records, per clientId, the attributes map passed to
	// CreateClient, so a test asserts the PKCE attribute was programmed on create.
	createdClientAttrs map[string]map[string]string
	// clientDescriptions records, per clientUUID, the client's current description:
	// CreateClient seeds it from the create representation and UpdateClientFields
	// overwrites it whenever a non-nil Description is sent, so a test asserts both
	// the create value and drift correction on update.
	clientDescriptions map[string]string

	// createClientErr / updateClientErr, when non-nil, are returned by
	// CreateClient / UpdateClientFields to simulate a Keycloak failure.
	createClientErr error
	updateClientErr error
}

// newFakeKeycloakClient returns a reachable fake with the given pre-existing group
// paths (each normalized and assigned a synthetic UUID).
func newFakeKeycloakClient(existingGroups ...string) *fakeKeycloakClient {
	f := &fakeKeycloakClient{
		realmReachable:       true,
		groups:               map[string]string{},
		clients:              map[string]string{},
		clientObjects:        map[string]keycloak.OIDCClient{},
		clientRoles:          map[string]string{},
		roleAssignments:      map[string]bool{},
		users:                map[string]*keycloak.User{},
		groupMembers:         map[string]bool{},
		listUserGroupsErrFor: map[string]error{},
		federatedLinks:       map[string]string{},
		createdClientRoles:   map[string]map[string]bool{},
		roleMappers:          map[string]bool{},
		clientSecrets:        map[string]string{},
		createdClientAttrs:   map[string]map[string]string{},
		lastUpdateFields:     map[string]keycloak.ClientFields{},
		clientDescriptions:   map[string]string{},
	}
	for _, p := range existingGroups {
		f.addGroup(p)
	}
	return f
}

func (f *fakeKeycloakClient) record(call string) { f.calls = append(f.calls, call) }

// normPath normalizes a path the way internal/keycloak does, so fake keys match
// the reconciler's paths regardless of leading/trailing slashes.
func normPath(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return ""
	}
	return "/" + trimmed
}

// addGroup seeds a group at path with a fresh synthetic UUID and returns the id.
func (f *fakeKeycloakClient) addGroup(path string) string {
	f.nextGroupID++
	id := "grp-" + strconv.Itoa(f.nextGroupID)
	f.groups[normPath(path)] = id
	return id
}

// notFoundErr builds an *APIError that keycloak.IsNotFound recognizes.
func notFoundErr(path string) error {
	return &keycloak.APIError{StatusCode: http.StatusNotFound, Method: http.MethodGet, Path: path, Message: "not found"}
}

func (f *fakeKeycloakClient) GetRealm(ctx context.Context) (*keycloak.Realm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetRealm")
	if f.realmErr != nil {
		return nil, f.realmErr
	}
	if !f.realmReachable {
		return nil, notFoundErr("/admin/realms/holos")
	}
	return &keycloak.Realm{Realm: "holos", Enabled: true}, nil
}

func (f *fakeKeycloakClient) GetGroupByPath(ctx context.Context, path string) (*keycloak.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Get:" + normPath(path))
	if f.groupGetNotFoundOnce[normPath(path)] {
		delete(f.groupGetNotFoundOnce, normPath(path))
		return nil, notFoundErr(path)
	}
	id, ok := f.groups[normPath(path)]
	if !ok {
		return nil, notFoundErr(path)
	}
	return &keycloak.Group{ID: id, Path: normPath(path)}, nil
}

func (f *fakeKeycloakClient) EnsureGroupByPathCreated(ctx context.Context, path string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Ensure:" + normPath(path))
	if f.ensureErr != nil {
		return "", false, f.ensureErr
	}
	if id, ok := f.groups[normPath(path)]; ok {
		// Already present: not created by this call (the race / pre-exists case).
		return id, false, nil
	}
	return f.addGroup(path), true, nil
}

func (f *fakeKeycloakClient) DeleteGroupByPathIfExists(ctx context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Delete:" + normPath(path))
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.groups, normPath(path))
	return nil
}

func (f *fakeKeycloakClient) DeleteGroup(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteGroup:" + id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for path, groupID := range f.groups {
		if groupID == id {
			delete(f.groups, path)
			return nil
		}
	}
	return notFoundErr("/groups/" + id)
}

func (f *fakeKeycloakClient) FindClientByClientID(ctx context.Context, clientID string) (*keycloak.OIDCClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FindClient:" + clientID)
	id, ok := f.clients[clientID]
	if !ok {
		return nil, nil
	}
	if existing, ok := f.clientObjects[clientID]; ok {
		cp := existing
		cp.ID = id
		cp.ClientID = clientID
		return &cp, nil
	}
	return &keycloak.OIDCClient{ID: id, ClientID: clientID}, nil
}

func (f *fakeKeycloakClient) GetClientRole(ctx context.Context, clientUUID, roleName string) (*keycloak.ClientRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetRole:" + clientUUID + "/" + roleName)
	id, ok := f.clientRoles[clientUUID+"/"+roleName]
	if !ok {
		return nil, notFoundErr("/roles/" + roleName)
	}
	return &keycloak.ClientRole{ID: id, Name: roleName, ContainerID: clientUUID}, nil
}

func (f *fakeKeycloakClient) AssignClientRoleToGroup(ctx context.Context, groupID, clientUUID string, role keycloak.ClientRole) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AssignRole:" + groupID + "/" + clientUUID + "/" + role.Name)
	if f.assignRoleErrFor[clientUUID+"/"+role.Name] {
		return &keycloak.APIError{StatusCode: http.StatusInternalServerError, Method: http.MethodPost, Path: "role-mappings", Message: "simulated assign failure"}
	}
	f.roleAssignments[groupID+"/"+clientUUID+"/"+role.Name] = true
	return nil
}

func (f *fakeKeycloakClient) ListGroupClientRoles(ctx context.Context, groupID, clientUUID string) ([]keycloak.ClientRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListGroupRoles:" + groupID + "/" + clientUUID)
	var out []keycloak.ClientRole
	prefix := groupID + "/" + clientUUID + "/"
	for k, ok := range f.roleAssignments {
		if ok && strings.HasPrefix(k, prefix) {
			out = append(out, keycloak.ClientRole{Name: k[len(prefix):]})
		}
	}
	return out, nil
}

func (f *fakeKeycloakClient) RemoveClientRoleFromGroup(ctx context.Context, groupID, clientUUID string, role keycloak.ClientRole) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveRole:" + groupID + "/" + clientUUID + "/" + role.Name)
	delete(f.roleAssignments, groupID+"/"+clientUUID+"/"+role.Name)
	return nil
}

func (f *fakeKeycloakClient) CreateGroupResource(ctx context.Context, permClientUUID string, resource keycloak.AuthzResource) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FGAPResource:" + resource.Name)
	f.fgapResources = append(f.fgapResources, resource.Name)
	return "res-" + resource.Name, nil
}

func (f *fakeKeycloakClient) CreateGroupPolicy(ctx context.Context, permClientUUID string, policy keycloak.GroupPolicy) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FGAPPolicy:" + policy.Name)
	f.fgapPolicies = append(f.fgapPolicies, policy.Name)
	return "pol-" + policy.Name, nil
}

func (f *fakeKeycloakClient) CreateScopePermission(ctx context.Context, permClientUUID string, permission keycloak.ScopePermission) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FGAPPermission:" + permission.Name)
	f.fgapPermissions = append(f.fgapPermissions, permission.Name)
	return "perm-" + permission.Name, nil
}

func (f *fakeKeycloakClient) FindPolicyByName(ctx context.Context, permClientUUID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FindPolicy:" + name)
	for _, p := range f.fgapPolicies {
		if p == name {
			return "pol-" + name, nil
		}
	}
	return "", nil
}

func (f *fakeKeycloakClient) FindPermissionByName(ctx context.Context, permClientUUID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FindPermission:" + name)
	for _, p := range f.fgapPermissions {
		if p == name {
			return "perm-" + name, nil
		}
	}
	return "", nil
}

func (f *fakeKeycloakClient) DeleteScopePermissionIfExists(ctx context.Context, permClientUUID, permissionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FGAPDelete:" + permissionID)
	f.fgapDeletes = append(f.fgapDeletes, permissionID)
	return nil
}

// seedClient registers an OIDC client (clientId → UUID) so FindClientByClientID
// resolves it.
func (f *fakeKeycloakClient) seedClient(clientID, uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients[clientID] = uuid
	f.clientObjects[clientID] = keycloak.OIDCClient{ID: uuid, ClientID: clientID}
}

// seedClientDescription sets a client's current description by UUID, modeling a
// console-set (drifted) description on a pre-existing client.
func (f *fakeKeycloakClient) seedClientDescription(clientUUID, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clientDescriptions[clientUUID] = description
	for clientID, id := range f.clients {
		if id != clientUUID {
			continue
		}
		current := f.clientObjects[clientID]
		current.ID = clientUUID
		current.ClientID = clientID
		current.Description = description
		f.clientObjects[clientID] = current
		break
	}
}

// seedClientRole registers a client role (clientUUID/role → UUID) so GetClientRole
// resolves it.
func (f *fakeKeycloakClient) seedClientRole(clientUUID, role, uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clientRoles[clientUUID+"/"+role] = uuid
}

// callsContain reports whether the recorded calls include the given call string.
func (f *fakeKeycloakClient) callsContain(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakeKeycloakClient) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeKeycloakClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// groupExists reports whether the named (normalized) group path currently exists.
func (f *fakeKeycloakClient) groupExists(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.groups[normPath(path)]
	return ok
}

// roleAssigned reports whether the role was assigned to groupID on clientUUID.
func (f *fakeKeycloakClient) roleAssigned(groupID, clientUUID, role string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roleAssignments[groupID+"/"+clientUUID+"/"+role]
}

// --- KeycloakUser reconciler seam ---

func (f *fakeKeycloakClient) FindUserByEmail(ctx context.Context, email string) (*keycloak.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FindUser:" + email)
	u, ok := f.users[email]
	if !ok {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}

func (f *fakeKeycloakClient) CreateUser(ctx context.Context, user keycloak.User) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateUser:" + user.Email)
	f.createUserCount++
	if _, ok := f.users[user.Email]; ok {
		return "", &keycloak.APIError{StatusCode: http.StatusConflict, Method: http.MethodPost, Path: "/users", Message: "user exists"}
	}
	f.nextUserID++
	id := "usr-" + strconv.Itoa(f.nextUserID)
	stored := user
	stored.ID = id
	f.users[user.Email] = &stored
	return id, nil
}

func (f *fakeKeycloakClient) DeleteUserIfExists(ctx context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteUser:" + userID)
	for email, u := range f.users {
		if u.ID == userID {
			delete(f.users, email)
			break
		}
	}
	return nil
}

func (f *fakeKeycloakClient) AddUserToGroup(ctx context.Context, userID, groupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AddMember:" + userID + "/" + groupID)
	f.groupMembers[userID+"/"+groupID] = true
	return nil
}

func (f *fakeKeycloakClient) ListUserGroups(ctx context.Context, userID string) ([]keycloak.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListUserGroups:" + userID)
	if f.listUserGroupsErr != nil {
		return nil, f.listUserGroupsErr
	}
	if err := f.listUserGroupsErrFor[userID]; err != nil {
		return nil, err
	}
	var out []keycloak.Group
	prefix := userID + "/"
	for k, member := range f.groupMembers {
		if member && strings.HasPrefix(k, prefix) {
			out = append(out, keycloak.Group{ID: k[len(prefix):]})
		}
	}
	return out, nil
}

func (f *fakeKeycloakClient) RemoveUserFromGroupIfMember(ctx context.Context, userID, groupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveMember:" + userID + "/" + groupID)
	delete(f.groupMembers, userID+"/"+groupID)
	return nil
}

func (f *fakeKeycloakClient) CreateFederatedIdentityIfNotExists(ctx context.Context, userID, provider string, link keycloak.FederatedIdentity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FederatedLink:" + userID + "/" + provider)
	f.federatedLinks[userID+"/"+provider] = link.UserID
	return nil
}

func (f *fakeKeycloakClient) DeleteFederatedIdentityIfExists(ctx context.Context, userID, provider string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FederatedUnlink:" + userID + "/" + provider)
	delete(f.federatedLinks, userID+"/"+provider)
	return nil
}

func (f *fakeKeycloakClient) ListFederatedIdentities(ctx context.Context, userID string) ([]keycloak.FederatedIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListFederated:" + userID)
	var out []keycloak.FederatedIdentity
	prefix := userID + "/"
	for k, subject := range f.federatedLinks {
		if strings.HasPrefix(k, prefix) {
			out = append(out, keycloak.FederatedIdentity{IdentityProvider: k[len(prefix):], UserID: subject})
		}
	}
	return out, nil
}

// setFederatedSubject seeds/overrides the upstream subject of a link, simulating
// an out-of-band recreation to a different subject.
func (f *fakeKeycloakClient) setFederatedSubject(userID, provider, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.federatedLinks[userID+"/"+provider] = subject
}

// --- KeycloakClient reconciler seam ---

func (f *fakeKeycloakClient) CreateClient(ctx context.Context, client keycloak.OIDCClient) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateClient:" + client.ClientID)
	if f.createClientErr != nil {
		return "", f.createClientErr
	}
	if _, ok := f.clients[client.ClientID]; ok {
		return "", &keycloak.APIError{StatusCode: http.StatusConflict, Method: http.MethodPost, Path: "/clients", Message: "client exists"}
	}
	f.nextGroupID++
	id := "cli-" + strconv.Itoa(f.nextGroupID)
	f.clients[client.ClientID] = id
	stored := client
	stored.ID = id
	f.clientObjects[client.ClientID] = stored
	f.clientDescriptions[id] = client.Description
	if client.Attributes != nil {
		attrs := map[string]string{}
		for k, v := range client.Attributes {
			attrs[k] = v
		}
		f.createdClientAttrs[client.ClientID] = attrs
	}
	return id, nil
}

func (f *fakeKeycloakClient) UpdateClientFields(ctx context.Context, clientUUID string, fields keycloak.ClientFields) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateClient:" + clientUUID)
	f.lastUpdateFields[clientUUID] = fields
	for clientID, id := range f.clients {
		if id != clientUUID {
			continue
		}
		current := f.clientObjects[clientID]
		current.ID = clientUUID
		current.ClientID = clientID
		if fields.Name != nil {
			current.Name = *fields.Name
		}
		if fields.Description != nil {
			current.Description = *fields.Description
		}
		if fields.Enabled != nil {
			current.Enabled = *fields.Enabled
		}
		if fields.PublicClient != nil {
			current.PublicClient = *fields.PublicClient
		}
		if fields.RedirectURIs != nil {
			current.RedirectURIs = append([]string(nil), (*fields.RedirectURIs)...)
		}
		if fields.WebOrigins != nil {
			current.WebOrigins = append([]string(nil), (*fields.WebOrigins)...)
		}
		if current.Attributes == nil {
			current.Attributes = map[string]string{}
		}
		for _, attr := range fields.RemoveAttributes {
			delete(current.Attributes, attr)
		}
		for k, v := range fields.Attributes {
			current.Attributes[k] = v
		}
		f.clientObjects[clientID] = current
		break
	}
	if fields.Description != nil {
		f.clientDescriptions[clientUUID] = *fields.Description
	}
	return f.updateClientErr
}

func (f *fakeKeycloakClient) DeleteClient(ctx context.Context, clientUUID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteClient:" + clientUUID)
	for clientID, id := range f.clients {
		if id == clientUUID {
			delete(f.clients, clientID)
			f.deletedClients = append(f.deletedClients, clientID)
			return nil
		}
	}
	return notFoundErr("/clients/" + clientUUID)
}

func (f *fakeKeycloakClient) CreateClientRoleIfNotExists(ctx context.Context, clientUUID string, role keycloak.ClientRole) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateClientRole:" + clientUUID + "/" + role.Name)
	if f.createdClientRoles[clientUUID] == nil {
		f.createdClientRoles[clientUUID] = map[string]bool{}
	}
	f.createdClientRoles[clientUUID][role.Name] = true
	// Register the role so a subsequent GetClientRole resolves it — modeling
	// Keycloak's create-then-readable behavior, so a test that does NOT pre-seed the
	// role still exercises the group reconciler's create-then-get path. Assign a
	// synthetic UUID only when the role is not already present (idempotent).
	key := clientUUID + "/" + role.Name
	if _, ok := f.clientRoles[key]; !ok {
		f.nextGroupID++
		f.clientRoles[key] = "role-" + strconv.Itoa(f.nextGroupID)
	}
	return nil
}

func (f *fakeKeycloakClient) EnsureClientRoleMapper(ctx context.Context, clientUUID, name, clientID, claimName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnsureMapper:" + clientUUID + "/" + name + "/" + clientID + "/" + claimName)
	f.roleMappers[clientUUID] = true
	return nil
}

func (f *fakeKeycloakClient) GetClientSecret(ctx context.Context, clientUUID string) (*keycloak.ClientSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetClientSecret:" + clientUUID)
	value := f.clientSecrets[clientUUID]
	if value == "" {
		value = "generated-secret-" + clientUUID
	}
	return &keycloak.ClientSecret{Type: "secret", Value: value}, nil
}

// --- user/client test inspection helpers ---

// seedUser registers a user by email with a synthetic UUID so FindUserByEmail
// resolves it, modeling a pre-existing Keycloak user.
func (f *fakeKeycloakClient) seedUser(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUserID++
	id := "usr-" + strconv.Itoa(f.nextUserID)
	f.users[email] = &keycloak.User{ID: id, Email: email, Username: email, Enabled: true}
	return id
}

// userExists reports whether a user with the email currently exists.
func (f *fakeKeycloakClient) userExists(email string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.users[email]
	return ok
}

// memberOf reports whether userID is a member of groupID.
func (f *fakeKeycloakClient) memberOf(userID, groupID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupMembers[userID+"/"+groupID]
}

// federated reports whether a federated-identity link exists for userID/provider.
func (f *fakeKeycloakClient) federated(userID, provider string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.federatedLinks[userID+"/"+provider]
	return ok
}

// updatePKCECleared reports whether the last UpdateClientFields for clientUUID
// requested removal of the PKCE code-challenge attribute.
func (f *fakeKeycloakClient) updatePKCECleared(clientUUID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.lastUpdateFields[clientUUID].RemoveAttributes {
		if k == keycloak.PKCECodeChallengeMethodAttr {
			return true
		}
	}
	return false
}

// updatePKCESet reports the PKCE attribute value the last UpdateClientFields for
// clientUUID set, or "" when none.
func (f *fakeKeycloakClient) updatePKCESet(clientUUID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUpdateFields[clientUUID].Attributes[keycloak.PKCECodeChallengeMethodAttr]
}

// clientExists reports whether an OIDC client with the clientId currently exists.
func (f *fakeKeycloakClient) clientExists(clientID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.clients[clientID]
	return ok
}

// clientRoleCreated reports whether role was created on the client UUID.
func (f *fakeKeycloakClient) clientRoleCreated(clientUUID, role string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createdClientRoles[clientUUID][role]
}

// mapperEnsured reports whether the client-role mapper was ensured on clientUUID.
func (f *fakeKeycloakClient) mapperEnsured(clientUUID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roleMappers[clientUUID]
}

// clientDescription returns the current description recorded for the client UUID.
func (f *fakeKeycloakClient) clientDescription(clientUUID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientDescriptions[clientUUID]
}

// createdClientPKCE returns the PKCE code-challenge attribute the client was
// created with, or "" when none was set.
func (f *fakeKeycloakClient) createdClientPKCE(clientID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createdClientAttrs[clientID][keycloak.PKCECodeChallengeMethodAttr]
}

// compile-time assertions that the fake satisfies all four reconciler seams.
var (
	_ InstanceClient   = (*fakeKeycloakClient)(nil)
	_ GroupClient      = (*fakeKeycloakClient)(nil)
	_ UserClient       = (*fakeKeycloakClient)(nil)
	_ ClientClient     = (*fakeKeycloakClient)(nil)
	_ MembershipClient = (*fakeKeycloakClient)(nil)
)

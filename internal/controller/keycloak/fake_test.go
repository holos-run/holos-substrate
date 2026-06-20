package keycloak

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/holos-run/holos-paas/internal/keycloak"
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

	// ensureErr, when non-nil, is returned by EnsureGroupByPath to simulate a
	// create failure.
	ensureErr error
	// deleteErr, when non-nil, is returned by DeleteGroupByPathIfExists.
	deleteErr error

	// Recorded calls, in order, e.g. "Ensure:/projects/p/roles/owner".
	calls []string

	// gotCABundle records the caBundle the reconciler's factory was last invoked
	// with, so a test asserts the instance's CABundle is threaded through.
	gotCABundle []byte
}

// newFakeKeycloakClient returns a reachable fake with the given pre-existing group
// paths (each normalized and assigned a synthetic UUID).
func newFakeKeycloakClient(existingGroups ...string) *fakeKeycloakClient {
	f := &fakeKeycloakClient{
		realmReachable:  true,
		groups:          map[string]string{},
		clients:         map[string]string{},
		clientRoles:     map[string]string{},
		roleAssignments: map[string]bool{},
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
	id, ok := f.groups[normPath(path)]
	if !ok {
		return nil, notFoundErr(path)
	}
	return &keycloak.Group{ID: id, Path: normPath(path)}, nil
}

func (f *fakeKeycloakClient) EnsureGroupByPath(ctx context.Context, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Ensure:" + normPath(path))
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	if id, ok := f.groups[normPath(path)]; ok {
		return id, nil
	}
	return f.addGroup(path), nil
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

func (f *fakeKeycloakClient) FindClientByClientID(ctx context.Context, clientID string) (*keycloak.OIDCClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FindClient:" + clientID)
	id, ok := f.clients[clientID]
	if !ok {
		return nil, nil
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

// compile-time assertions that the fake satisfies both reconciler seams.
var (
	_ InstanceClient = (*fakeKeycloakClient)(nil)
	_ GroupClient    = (*fakeKeycloakClient)(nil)
)

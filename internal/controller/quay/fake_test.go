package quay

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/holos-run/holos-paas/internal/quay"
)

// fakeOrgClient is a recording, in-memory stand-in for the Quay organization API
// the reconciler drives. It satisfies OrgClient so a test injects it via the
// reconciler's ClientFactory, exercising the full reconcile loop without HTTP or a
// live Quay. It records every call so tests can assert create-vs-adopt behavior
// and idempotent delete.
type fakeOrgClient struct {
	mu sync.Mutex

	// existing is the set of org names that already exist in the fake Quay.
	// GetOrganization returns a 404 APIError for any name not in this set.
	existing map[string]bool

	// emails records each org's current contact email, so UpdateOrganization
	// drift is observable and GetOrganization reads it back.
	emails map[string]string

	// markers records each org's ownership-marker robot description (the
	// holos-owner robot). A name absent from this map has no marker; GetOrganization
	// Robot then returns 404.
	markers map[string]string

	// teams records each org's teams keyed by "<org>/<team>", modeling the org
	// payload's teams map the reconciler reconciles spec.syncedTeams against.
	teams map[string]quay.Team
	// teamSync records each team's bound OIDC group keyed by "<org>/<team>"; a key
	// absent from this map means the team is not synced. It backs GetTeamMembers'
	// sync binding and the EnableTeamSync/DisableTeamSync transitions.
	teamSync map[string]string
	// prototypes records each org's default-permission prototypes keyed by org
	// name, modeling the org prototypes collection. IDs are assigned on create.
	prototypes map[string][]quay.Prototype
	// nextPrototypeID is the monotonically-increasing source of synthetic prototype
	// IDs CreatePrototype assigns.
	nextPrototypeID int

	// listTeamsErr/upsertTeamErr/enableSyncErr/listPrototypesErr/createPrototypeErr,
	// when non-nil, are returned by the corresponding method to simulate a Quay
	// failure on a team/prototype operation.
	listTeamsErr       error
	upsertTeamErr      error
	enableSyncErr      error
	deleteTeamErr      error
	listPrototypesErr  error
	createPrototypeErr error

	// getErr, when non-nil, is returned by GetOrganization regardless of the
	// org's existence — used to simulate a non-404 Quay error (auth/server).
	getErr error
	// createErr, when non-nil, is returned by CreateOrganization — used to
	// simulate a Quay create failure.
	createErr error
	// updateErr, when non-nil, is returned by UpdateOrganization.
	updateErr error
	// createRaceExisting, when non-empty, names an org that "appears" the moment
	// CreateOrganization is called even though GetOrganization 404'd — used to
	// simulate the create race where a duplicate (409 conflict) is returned.
	createRaceExisting string
	// deleteErr, when non-nil, is returned by DeleteOrganizationIfExists.
	deleteErr error
	// robotCreateErr, when non-nil, is returned by CreateOrganizationRobot — used
	// to simulate a failed marker stamp.
	robotCreateErr error
	// robotCreateErrs, when non-empty, is consumed by CreateOrganizationRobot
	// before robotCreateErr so tests can fail one marker write and let a later
	// restore succeed.
	robotCreateErrs []error
	// robotGetErr, when non-nil, is returned by GetOrganizationRobot.
	robotGetErr error

	// Recorded calls, in order, e.g. "Get:acme", "Create:acme", "Delete:acme".
	calls []string

	// gotCABundle records the caBundle the reconciler's ClientFactory was last
	// invoked with, so a test asserts the spec's CABundle is threaded through to
	// the client factory.
	gotCABundle []byte
}

// newFakeOrgClient returns a fake with the given pre-existing org names.
func newFakeOrgClient(existing ...string) *fakeOrgClient {
	f := &fakeOrgClient{
		existing:   map[string]bool{},
		emails:     map[string]string{},
		markers:    map[string]string{},
		teams:      map[string]quay.Team{},
		teamSync:   map[string]string{},
		prototypes: map[string][]quay.Prototype{},
	}
	for _, name := range existing {
		f.existing[name] = true
	}
	return f
}

func (f *fakeOrgClient) record(call string) {
	f.calls = append(f.calls, call)
}

// notFoundError builds an *APIError that quay.IsNotFound recognizes, so the
// reconciler's create-vs-adopt branch behaves exactly as it would against a real
// 404 from Quay.
func notFoundError(name string) error {
	return &quay.APIError{
		StatusCode: http.StatusNotFound,
		Method:     http.MethodGet,
		Path:       "/api/v1/organization/" + name,
		Message:    "not found",
	}
}

// conflictError builds an *APIError that quay.IsConflict recognizes, mirroring a
// real already-exists response from Quay.
func conflictError(name string) error {
	return &quay.APIError{
		StatusCode: http.StatusConflict,
		Method:     http.MethodPost,
		Path:       "/api/v1/organization/",
		Message:    "organization " + name + " already exists",
	}
}

func (f *fakeOrgClient) GetOrganization(ctx context.Context, name string) (*quay.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Get:" + name)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.existing[name] {
		return &quay.Organization{Name: name, Email: f.emails[name]}, nil
	}
	return nil, notFoundError(name)
}

func (f *fakeOrgClient) CreateOrganization(ctx context.Context, name, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Create:" + name)
	if f.createErr != nil {
		return f.createErr
	}
	// Simulate a create race: the org "appeared" between GET and POST, so the
	// create returns a conflict and does not mark it as created-by-this-call.
	if f.createRaceExisting == name || f.existing[name] {
		f.existing[name] = true
		return conflictError(name)
	}
	f.existing[name] = true
	f.emails[name] = email
	return nil
}

func (f *fakeOrgClient) UpdateOrganization(ctx context.Context, name, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Update:" + name + ":" + email)
	if f.updateErr != nil {
		return f.updateErr
	}
	f.emails[name] = email
	return nil
}

func (f *fakeOrgClient) DeleteOrganizationIfExists(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Delete:" + name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.existing, name)
	delete(f.emails, name)
	delete(f.markers, name)
	return nil
}

func (f *fakeOrgClient) GetOrganizationRobot(ctx context.Context, org, shortname string) (*quay.OrganizationRobot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetRobot:" + org)
	if f.robotGetErr != nil {
		return nil, f.robotGetErr
	}
	desc, ok := f.markers[org]
	if !ok {
		return nil, notFoundError(org + "+" + shortname)
	}
	return &quay.OrganizationRobot{Name: org + "+" + shortname, Description: desc}, nil
}

func (f *fakeOrgClient) CreateOrganizationRobot(ctx context.Context, org, shortname, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateRobot:" + org + ":" + description)
	if len(f.robotCreateErrs) > 0 {
		err := f.robotCreateErrs[0]
		f.robotCreateErrs = f.robotCreateErrs[1:]
		if err != nil {
			return err
		}
	}
	if f.robotCreateErr != nil {
		return f.robotCreateErr
	}
	// Quay's create-robot endpoint is not idempotent: an existing robot is a
	// conflict (the marker is write-once).
	if _, ok := f.markers[org]; ok {
		return conflictError(org + "+" + shortname)
	}
	f.markers[org] = description
	return nil
}

func (f *fakeOrgClient) DeleteOrganizationRobotIfExists(ctx context.Context, org, shortname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteRobot:" + org)
	delete(f.markers, org)
	return nil
}

// setMarker seeds the ownership-marker robot description for org, so tests can
// simulate an org that already carries (or lacks) this CR's marker.
func (f *fakeOrgClient) setMarker(org, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markers[org] = description
}

// callsContain reports whether the recorded calls include the given call string.
func (f *fakeOrgClient) callsContain(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

// orgExists reports whether the named org currently exists in the fake.
func (f *fakeOrgClient) orgExists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existing[name]
}

// teamKey is the composite map key for the fake's per-team state.
func teamKey(org, team string) string { return org + "/" + team }

func (f *fakeOrgClient) ListTeams(ctx context.Context, org string) (map[string]quay.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListTeams:" + org)
	if f.listTeamsErr != nil {
		return nil, f.listTeamsErr
	}
	out := map[string]quay.Team{}
	prefix := org + "/"
	for k, t := range f.teams {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			name := k[len(prefix):]
			t.Name = name
			_, t.IsSynced = f.teamSync[k]
			out[name] = t
		}
	}
	return out, nil
}

func (f *fakeOrgClient) UpsertTeam(ctx context.Context, org, team, role, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpsertTeam:" + org + "/" + team + ":" + role)
	if f.upsertTeamErr != nil {
		return f.upsertTeamErr
	}
	f.teams[teamKey(org, team)] = quay.Team{Name: team, Role: role, Description: description}
	return nil
}

func (f *fakeOrgClient) DeleteTeamIfExists(ctx context.Context, org, team string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteTeam:" + org + "/" + team)
	if f.deleteTeamErr != nil {
		return f.deleteTeamErr
	}
	key := teamKey(org, team)
	delete(f.teams, key)
	delete(f.teamSync, key)
	return nil
}

func (f *fakeOrgClient) GetTeamMembers(ctx context.Context, org, team string) (*quay.TeamMembers, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetTeamMembers:" + org + "/" + team)
	if _, ok := f.teams[teamKey(org, team)]; !ok {
		return nil, notFoundError(org + "/" + team)
	}
	out := &quay.TeamMembers{Name: team}
	if group, ok := f.teamSync[teamKey(org, team)]; ok {
		out.Synced = &quay.TeamSync{
			Service: "oidc",
			Config:  map[string]any{"group_name": group},
		}
	}
	return out, nil
}

func (f *fakeOrgClient) EnableTeamSyncIfNotSynced(ctx context.Context, org, team, oidcGroup string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnableSync:" + org + "/" + team + ":" + oidcGroup)
	if f.enableSyncErr != nil {
		return f.enableSyncErr
	}
	// Idempotent: already bound to the same group is a no-op success.
	if cur, ok := f.teamSync[teamKey(org, team)]; ok && cur == oidcGroup {
		return nil
	}
	f.teamSync[teamKey(org, team)] = oidcGroup
	return nil
}

func (f *fakeOrgClient) DisableTeamSyncIfSynced(ctx context.Context, org, team string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DisableSync:" + org + "/" + team)
	delete(f.teamSync, teamKey(org, team))
	return nil
}

func (f *fakeOrgClient) ListPrototypes(ctx context.Context, org string) ([]quay.Prototype, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListPrototypes:" + org)
	if f.listPrototypesErr != nil {
		return nil, f.listPrototypesErr
	}
	// Return a copy so callers cannot mutate the fake's slice in place.
	return append([]quay.Prototype(nil), f.prototypes[org]...), nil
}

func (f *fakeOrgClient) CreatePrototype(ctx context.Context, org, role, delegateTeam string) (*quay.Prototype, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreatePrototype:" + org + ":" + delegateTeam + ":" + role)
	if f.createPrototypeErr != nil {
		return nil, f.createPrototypeErr
	}
	f.nextPrototypeID++
	p := quay.Prototype{
		ID:   fmt.Sprintf("proto-%d", f.nextPrototypeID),
		Role: role,
		Delegate: quay.PrototypeDelegate{
			Name: delegateTeam,
			Kind: quay.PrototypeDelegateTeam,
		},
	}
	f.prototypes[org] = append(f.prototypes[org], p)
	return &p, nil
}

func (f *fakeOrgClient) UpdatePrototype(ctx context.Context, org, prototypeID, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdatePrototype:" + org + ":" + prototypeID + ":" + role)
	list := f.prototypes[org]
	for i := range list {
		if list[i].ID == prototypeID {
			list[i].Role = role
			f.prototypes[org] = list
			return nil
		}
	}
	return notFoundError(prototypeID)
}

func (f *fakeOrgClient) DeletePrototypeIfExists(ctx context.Context, org, prototypeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeletePrototype:" + org + ":" + prototypeID)
	list := f.prototypes[org]
	out := list[:0:0]
	for _, p := range list {
		if p.ID != prototypeID {
			out = append(out, p)
		}
	}
	f.prototypes[org] = out
	return nil
}

// seedTeam pre-populates a team (optionally synced to oidcGroup, "" for unsynced)
// with an empty description, simulating a foreign (not controller-created) team.
// Use seedTeamWithDescription to stamp this CR's ownership marker (the heal
// candidate).
func (f *fakeOrgClient) seedTeam(org, team, role, oidcGroup string) {
	f.seedTeamWithDescription(org, team, role, "", oidcGroup)
}

// seedTeamWithDescription pre-populates a team with an explicit description, so a
// test can simulate a controller-created team carrying this CR's managedTeamMarker
// (managedTeamPrefix + the org CR UID) — the heal candidate whose status record was
// lost — or any other foreign-but-described team.
func (f *fakeOrgClient) seedTeamWithDescription(org, team, role, description, oidcGroup string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teams[teamKey(org, team)] = quay.Team{Name: team, Role: role, Description: description}
	if oidcGroup != "" {
		f.teamSync[teamKey(org, team)] = oidcGroup
	}
}

// teamRole returns a team's recorded role and whether it exists.
func (f *fakeOrgClient) teamRole(org, team string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.teams[teamKey(org, team)]
	return t.Role, ok
}

// teamDescription returns a team's description and whether it exists.
func (f *fakeOrgClient) teamDescription(org, team string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.teams[teamKey(org, team)]
	return t.Description, ok
}

// teamGroup returns a team's bound OIDC group and whether it is synced.
func (f *fakeOrgClient) teamGroup(org, team string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.teamSync[teamKey(org, team)]
	return g, ok
}

// teamPrototype returns the prototype delegating to team and whether one exists.
func (f *fakeOrgClient) teamPrototype(org, team string) (quay.Prototype, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prototypes[org] {
		if p.Delegate.Kind == quay.PrototypeDelegateTeam && p.Delegate.Name == team {
			return p, true
		}
	}
	return quay.Prototype{}, false
}

// compile-time assertion that the fake satisfies the reconciler's seam.
var _ OrgClient = (*fakeOrgClient)(nil)

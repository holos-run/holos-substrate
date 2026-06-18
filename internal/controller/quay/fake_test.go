package quay

import (
	"context"
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
	// robotGetErr, when non-nil, is returned by GetOrganizationRobot.
	robotGetErr error

	// Recorded calls, in order, e.g. "Get:acme", "Create:acme", "Delete:acme".
	calls []string

	// gotCABundle records the caBundle the reconciler's ClientFactory was last
	// invoked with, so a test asserts the spec's CABundle is threaded through to
	// the client factory (HOL-1320).
	gotCABundle []byte
}

// newFakeOrgClient returns a fake with the given pre-existing org names.
func newFakeOrgClient(existing ...string) *fakeOrgClient {
	f := &fakeOrgClient{
		existing: map[string]bool{},
		emails:   map[string]string{},
		markers:  map[string]string{},
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

// compile-time assertion that the fake satisfies the reconciler's seam.
var _ OrgClient = (*fakeOrgClient)(nil)

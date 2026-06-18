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

	// getErr, when non-nil, is returned by GetOrganization regardless of the
	// org's existence — used to simulate a non-404 Quay error (auth/server).
	getErr error
	// createErr, when non-nil, is returned by CreateOrganizationIfNotExists —
	// used to simulate a Quay create failure.
	createErr error
	// deleteErr, when non-nil, is returned by DeleteOrganizationIfExists.
	deleteErr error

	// Recorded calls, in order, e.g. "Get:acme", "Create:acme", "Delete:acme".
	calls []string
}

// newFakeOrgClient returns a fake with the given pre-existing org names.
func newFakeOrgClient(existing ...string) *fakeOrgClient {
	f := &fakeOrgClient{existing: map[string]bool{}}
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

func (f *fakeOrgClient) GetOrganization(ctx context.Context, name string) (*quay.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Get:" + name)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.existing[name] {
		return &quay.Organization{Name: name}, nil
	}
	return nil, notFoundError(name)
}

func (f *fakeOrgClient) CreateOrganizationIfNotExists(ctx context.Context, name, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Create:" + name)
	if f.createErr != nil {
		return f.createErr
	}
	f.existing[name] = true
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
	return nil
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

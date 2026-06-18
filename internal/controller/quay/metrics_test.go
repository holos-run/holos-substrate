package quay

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TestCustomMetricsRegistered asserts the custom collectors registered into
// controller-runtime's metrics Registry without panicking. The package's init
// performed the registration at import time, so reaching the test body at all
// proves MustRegister did not panic; gathering the registry additionally
// confirms the collectors are present and collectable. A CounterVec emits no
// metric family until at least one label combination is observed, so the test
// records one sample of each before gathering.
func TestCustomMetricsRegistered(t *testing.T) {
	recordReconcile(kindOrganization, nil)
	recordQuayAPI(opGetOrganization, nil)

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics registry: %v", err)
	}

	want := map[string]bool{
		"holos_controller_reconcile_total":         false,
		"holos_controller_quay_api_requests_total": false,
	}
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected custom metric %q to be registered, but it was not gathered", name)
		}
	}
}

// TestRecordReconcile asserts the reconcile counter increments for the right
// kind/outcome label pair, with the outcome derived from the error.
func TestRecordReconcile(t *testing.T) {
	reconcileTotal.Reset()

	recordReconcile(kindOrganization, nil)
	recordReconcile(kindOrganization, nil)
	recordReconcile(kindRepository, errors.New("boom"))

	if got := testutil.ToFloat64(reconcileTotal.WithLabelValues(kindOrganization, outcomeSuccess)); got != 2 {
		t.Errorf("organization success reconciles = %v, want 2", got)
	}
	if got := testutil.ToFloat64(reconcileTotal.WithLabelValues(kindRepository, outcomeError)); got != 1 {
		t.Errorf("repository error reconciles = %v, want 1", got)
	}
}

// TestRecordQuayAPI asserts the Quay-API counter increments per operation/outcome.
func TestRecordQuayAPI(t *testing.T) {
	quayAPIRequestsTotal.Reset()

	recordQuayAPI(opGetOrganization, nil)
	recordQuayAPI(opCreateOrganization, errors.New("conflict"))

	if got := testutil.ToFloat64(quayAPIRequestsTotal.WithLabelValues(opGetOrganization, outcomeSuccess)); got != 1 {
		t.Errorf("get_organization success requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(quayAPIRequestsTotal.WithLabelValues(opCreateOrganization, outcomeError)); got != 1 {
		t.Errorf("create_organization error requests = %v, want 1", got)
	}
}

// TestOutcomeHelpers asserts ignoreNotFound/ignoreConflict translate the
// expected Quay control-flow branches to a nil (success) outcome while leaving
// other errors as failures.
func TestOutcomeHelpers(t *testing.T) {
	if outcomeLabel(nil) != outcomeSuccess {
		t.Errorf("outcomeLabel(nil) = %q, want %q", outcomeLabel(nil), outcomeSuccess)
	}
	if outcomeLabel(errors.New("x")) != outcomeError {
		t.Errorf("outcomeLabel(err) = %q, want %q", outcomeLabel(errors.New("x")), outcomeError)
	}
	// A plain (non-Quay-typed) error must NOT be swallowed by the ignore helpers.
	plain := errors.New("plain")
	if ignoreNotFound(plain) != plain {
		t.Errorf("ignoreNotFound passed through the wrong error")
	}
	if ignoreConflict(plain) != plain {
		t.Errorf("ignoreConflict passed through the wrong error")
	}
	if ignoreNotFound(nil) != nil || ignoreConflict(nil) != nil {
		t.Errorf("ignore helpers must leave nil as nil")
	}
}

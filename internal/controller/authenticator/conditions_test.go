package authenticator

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMarkReady asserts the success path sets Accepted, Programmed, and Ready all
// True with the observed generation, and that a redundant call reports no change.
func TestMarkReady(t *testing.T) {
	var conditions []metav1.Condition
	if !markReady(&conditions, ReasonReconciled, "backend configured", 3) {
		t.Fatal("markReady reported no change on first call")
	}
	for _, ct := range []string{ConditionAccepted, ConditionProgrammed, ConditionReady} {
		c := meta.FindStatusCondition(conditions, ct)
		if c == nil {
			t.Fatalf("condition %s not set", ct)
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("condition %s status = %q, want True", ct, c.Status)
		}
		if c.Reason != ReasonReconciled {
			t.Errorf("condition %s reason = %q, want %q", ct, c.Reason, ReasonReconciled)
		}
		if c.ObservedGeneration != 3 {
			t.Errorf("condition %s observedGeneration = %d, want 3", ct, c.ObservedGeneration)
		}
	}
	if markReady(&conditions, ReasonReconciled, "backend configured", 3) {
		t.Error("markReady reported a change on an identical second call")
	}
}

// TestMarkNotReady asserts the failure path sets Programmed and Ready False while
// leaving an already-True Accepted untouched.
func TestMarkNotReady(t *testing.T) {
	var conditions []metav1.Condition
	markReady(&conditions, ReasonReconciled, "ok", 1)

	if !markNotReady(&conditions, ReasonDiscoveryFailed, "issuer unreachable", 2) {
		t.Fatal("markNotReady reported no change")
	}
	if c := meta.FindStatusCondition(conditions, ConditionAccepted); c == nil || c.Status != metav1.ConditionTrue {
		t.Error("markNotReady changed Accepted; it should leave it untouched")
	}
	for _, ct := range []string{ConditionProgrammed, ConditionReady} {
		c := meta.FindStatusCondition(conditions, ct)
		if c == nil || c.Status != metav1.ConditionFalse {
			t.Errorf("condition %s not False after markNotReady", ct)
		}
		if c.Reason != ReasonDiscoveryFailed {
			t.Errorf("condition %s reason = %q, want %q", ct, c.Reason, ReasonDiscoveryFailed)
		}
	}
}

// TestMarkRejected asserts the invalid-spec rejection path drives Accepted,
// Programmed, and Ready all False — Accepted is not left lingering True from a
// previously valid generation.
func TestMarkRejected(t *testing.T) {
	var conditions []metav1.Condition
	markReady(&conditions, ReasonReconciled, "ok", 1)

	if !markRejected(&conditions, ReasonInvalidSpec, "malformed CEL expression", 2) {
		t.Fatal("markRejected reported no change")
	}
	for _, ct := range []string{ConditionAccepted, ConditionProgrammed, ConditionReady} {
		c := meta.FindStatusCondition(conditions, ct)
		if c == nil || c.Status != metav1.ConditionFalse {
			t.Errorf("condition %s not False after markRejected", ct)
		}
		if c.Reason != ReasonInvalidSpec {
			t.Errorf("condition %s reason = %q, want %q", ct, c.Reason, ReasonInvalidSpec)
		}
		if c.ObservedGeneration != 2 {
			t.Errorf("condition %s observedGeneration = %d, want 2", ct, c.ObservedGeneration)
		}
	}
}

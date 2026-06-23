package authenticator

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
	"github.com/holos-run/holos-paas/internal/authenticator"
)

// makeNamespace creates a uniquely-named namespace so each test's Backend is
// isolated.
func makeNamespace(ctx context.Context, t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := shared.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() { _ = shared.k8sClient.Delete(context.Background(), ns) })
	return ns.Name
}

// createBackend persists b and returns its object key.
func createBackend(ctx context.Context, t *testing.T, b *authenticatorv1alpha1.Backend) client.ObjectKey {
	t.Helper()
	if err := shared.k8sClient.Create(ctx, b); err != nil {
		t.Fatalf("creating Backend: %v", err)
	}
	return client.ObjectKeyFromObject(b)
}

// getBackend fetches the current Backend state.
func getBackend(ctx context.Context, t *testing.T, key client.ObjectKey) *authenticatorv1alpha1.Backend {
	t.Helper()
	b := &authenticatorv1alpha1.Backend{}
	if err := shared.k8sClient.Get(ctx, key, b); err != nil {
		t.Fatalf("getting Backend: %v", err)
	}
	return b
}

// condStatus returns the status of the named condition, or "" when absent.
func condStatus(b *authenticatorv1alpha1.Backend, condType string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(b.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Status
}

// condReason returns the reason of the named condition, or "" when absent.
func condReason(b *authenticatorv1alpha1.Backend, condType string) string {
	c := meta.FindStatusCondition(b.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Reason
}

// TestReconcileReadyOnDiscovery asserts a Backend whose OIDC discovery succeeds
// and whose default group mapping compiles reaches Accepted/Programmed/Ready=True
// and is registered in the store keyed by its host.
func TestReconcileReadyOnDiscovery(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	key := createBackend(ctx, t, makeBackend(ns, "backend-a", "api-a.example.test"))

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	b := getBackend(ctx, t, key)
	for _, ct := range []string{ConditionAccepted, ConditionProgrammed, ConditionReady} {
		if got := condStatus(b, ct); got != metav1.ConditionTrue {
			t.Errorf("%s = %q, want True", ct, got)
		}
	}
	if got := condReason(b, ConditionReady); got != ReasonReconciled {
		t.Errorf("Ready reason = %q, want %q", got, ReasonReconciled)
	}
	if b.Status.ObservedGeneration != b.Generation {
		t.Errorf("observedGeneration = %d, want %d", b.Status.ObservedGeneration, b.Generation)
	}
	if _, ok := store.Get("api-a.example.test"); !ok {
		t.Errorf("store missing entry for host api-a.example.test")
	}
}

// TestReconcileInvalidCELRejects asserts a malformed group-mapping CEL expression
// rejects the spec (Accepted/Programmed/Ready=False, reason InvalidSpec), does not
// requeue with an error, and leaves nothing in the store.
func TestReconcileInvalidCELRejects(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	b := makeBackend(ns, "backend-bad-cel", "api-bad.example.test")
	b.Spec.GroupMapping.CELExpression = `claims.groups[` // syntax error
	key := createBackend(ctx, t, b)

	res, err := reconcile(ctx, r, key)
	if err != nil {
		t.Fatalf("reconcile returned error for an invalid spec (should not requeue): %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 for a terminal rejection", res.RequeueAfter)
	}

	got := getBackend(ctx, t, key)
	if s := condStatus(got, ConditionAccepted); s != metav1.ConditionFalse {
		t.Errorf("Accepted = %q, want False", s)
	}
	if reason := condReason(got, ConditionReady); reason != ReasonInvalidSpec {
		t.Errorf("Ready reason = %q, want %q", reason, ReasonInvalidSpec)
	}
	if store.Len() != 0 {
		t.Errorf("store has %d entries, want 0 for a rejected spec", store.Len())
	}
}

// TestReconcileInvalidServerURLRejects asserts a Backend whose spec.server.url is
// not a valid http(s) URL (it only satisfies the CRD MinLength) is rejected as an
// invalid spec and never registered in the store.
func TestReconcileInvalidServerURLRejects(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	b := makeBackend(ns, "backend-bad-url", "api-badurl.example.test")
	b.Spec.Server.URL = "not a url" // passes MinLength, not a real URL
	key := createBackend(ctx, t, b)

	res, err := reconcile(ctx, r, key)
	if err != nil {
		t.Fatalf("reconcile returned error for an invalid server URL (should not requeue): %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 for a terminal rejection", res.RequeueAfter)
	}

	got := getBackend(ctx, t, key)
	if s := condStatus(got, ConditionReady); s != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", s)
	}
	if reason := condReason(got, ConditionReady); reason != ReasonInvalidSpec {
		t.Errorf("Ready reason = %q, want %q", reason, ReasonInvalidSpec)
	}
	if store.Len() != 0 {
		t.Errorf("store has %d entries, want 0 for a rejected spec", store.Len())
	}
}

// TestReconcileDiscoveryFailureNotReady asserts a Backend whose OIDC discovery
// fails is marked Programmed/Ready=False (reason DiscoveryFailed), requeues with
// an error, and is not registered in the store. Accepted stays True (the spec was
// understood; only discovery failed).
func TestReconcileDiscoveryFailureNotReady(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	discoverFail := func(context.Context, string, string, []byte) (authenticator.TokenVerifier, error) {
		return nil, fmt.Errorf("issuer unreachable")
	}
	r, store, _ := newReconciler(discoverFail)
	key := createBackend(ctx, t, makeBackend(ns, "backend-disc-fail", "api-fail.example.test"))

	if _, err := reconcile(ctx, r, key); err == nil {
		t.Fatalf("reconcile = nil error, want a requeue error on discovery failure")
	}

	b := getBackend(ctx, t, key)
	if s := condStatus(b, ConditionAccepted); s != metav1.ConditionTrue {
		t.Errorf("Accepted = %q, want True (spec understood)", s)
	}
	if s := condStatus(b, ConditionReady); s != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", s)
	}
	if reason := condReason(b, ConditionReady); reason != ReasonDiscoveryFailed {
		t.Errorf("Ready reason = %q, want %q", reason, ReasonDiscoveryFailed)
	}
	if store.Len() != 0 {
		t.Errorf("store has %d entries, want 0 for an unready backend", store.Len())
	}
}

// TestReconcileDeleteRemovesFromStore asserts that once a ready Backend is deleted
// and reconciled, its store entry is removed by object key (the not-found Get path
// cannot read spec.host).
func TestReconcileDeleteRemovesFromStore(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	key := createBackend(ctx, t, makeBackend(ns, "backend-del", "api-del.example.test"))

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("store has %d entries before delete, want 1", store.Len())
	}

	if err := shared.k8sClient.Delete(ctx, getBackend(ctx, t, key)); err != nil {
		t.Fatalf("deleting Backend: %v", err)
	}
	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	if store.Len() != 0 {
		t.Errorf("store has %d entries after delete, want 0", store.Len())
	}
}

// TestReconcileHostConflictDeterministic asserts that when two Backends declare
// the same spec.host, the lexicographically-smallest object key owns the store
// entry regardless of reconcile order, and the loser reports
// Ready=False/HostConflict. This is the order-independent convergence Codex's
// round-3 finding asked for: the same owner is chosen whichever Backend reconciles
// first.
func TestReconcileHostConflictDeterministic(t *testing.T) {
	const host = "shared.example.test"

	// keyA = "<ns>/backend-host-a" sorts before keyB = "<ns>/backend-host-b".
	t.Run("smaller reconciles first", func(t *testing.T) {
		ctx := context.Background()
		ns := makeNamespace(ctx, t)
		r, store, _ := newReconciler(discoverOK)

		keyA := createBackend(ctx, t, makeBackend(ns, "backend-host-a", host))
		if _, err := reconcile(ctx, r, keyA); err != nil {
			t.Fatalf("reconcile A: %v", err)
		}
		keyB := createBackend(ctx, t, makeBackend(ns, "backend-host-b", host))
		if _, err := reconcile(ctx, r, keyB); err == nil {
			t.Fatalf("reconcile B = nil error, want a requeue error on host conflict")
		}

		assertOwner(t, store, host, keyA.String())
		if reason := condReason(getBackend(ctx, t, keyB), ConditionReady); reason != ReasonHostConflict {
			t.Errorf("B Ready reason = %q, want %q", reason, ReasonHostConflict)
		}
	})

	// Reverse order: the larger key reconciles first but must yield to the smaller.
	t.Run("larger reconciles first", func(t *testing.T) {
		ctx := context.Background()
		ns := makeNamespace(ctx, t)
		r, store, _ := newReconciler(discoverOK)

		keyB := createBackend(ctx, t, makeBackend(ns, "backend-host-b", host))
		if _, err := reconcile(ctx, r, keyB); err != nil {
			t.Fatalf("reconcile B: %v", err)
		}
		keyA := createBackend(ctx, t, makeBackend(ns, "backend-host-a", host))
		// A is smaller, so it seizes ownership and reconciles cleanly.
		if _, err := reconcile(ctx, r, keyA); err != nil {
			t.Fatalf("reconcile A (should seize ownership): %v", err)
		}

		assertOwner(t, store, host, keyA.String())
		// Re-reconciling B now finds the host owned by the smaller A → conflict.
		if _, err := reconcile(ctx, r, keyB); err == nil {
			t.Fatalf("re-reconcile B = nil error, want a host-conflict requeue error")
		}
		if reason := condReason(getBackend(ctx, t, keyB), ConditionReady); reason != ReasonHostConflict {
			t.Errorf("B Ready reason = %q, want %q", reason, ReasonHostConflict)
		}
	})
}

// TestReconcileHostConflictLoserRecoversAfterWinnerDeleted asserts the
// HostConflict loser becomes Ready once the winning Backend is deleted and the
// loser re-reconciles — the host is freed, so the loser now wins it.
func TestReconcileHostConflictLoserRecoversAfterWinnerDeleted(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	const host = "shared-recover.example.test"

	r, store, _ := newReconciler(discoverOK)

	keyA := createBackend(ctx, t, makeBackend(ns, "backend-host-a", host)) // winner (smaller)
	if _, err := reconcile(ctx, r, keyA); err != nil {
		t.Fatalf("reconcile A: %v", err)
	}
	keyB := createBackend(ctx, t, makeBackend(ns, "backend-host-b", host)) // loser (larger)
	if _, err := reconcile(ctx, r, keyB); err == nil {
		t.Fatalf("reconcile B = nil error, want host-conflict requeue")
	}
	if reason := condReason(getBackend(ctx, t, keyB), ConditionReady); reason != ReasonHostConflict {
		t.Fatalf("B Ready reason = %q, want %q", reason, ReasonHostConflict)
	}

	// Delete the winner A and reconcile it (frees the host from the store).
	if err := shared.k8sClient.Delete(ctx, getBackend(ctx, t, keyA)); err != nil {
		t.Fatalf("deleting A: %v", err)
	}
	if _, err := reconcile(ctx, r, keyA); err != nil {
		t.Fatalf("reconcile A after delete: %v", err)
	}

	// B re-reconciles and now wins the freed host.
	if _, err := reconcile(ctx, r, keyB); err != nil {
		t.Fatalf("reconcile B after winner deleted: %v", err)
	}
	if s := condStatus(getBackend(ctx, t, keyB), ConditionReady); s != metav1.ConditionTrue {
		t.Errorf("B Ready = %q, want True after winner deleted", s)
	}
	assertOwner(t, store, host, keyB.String())
}

// assertOwner fails the test unless host is owned by wantOwner in the store.
func assertOwner(t *testing.T, store *authenticator.Store, host, wantOwner string) {
	t.Helper()
	if owner, ok := store.Owner(host); !ok || owner != wantOwner {
		t.Errorf("store owner of %q = %q (ok=%v), want %q", host, owner, ok, wantOwner)
	}
}

// TestReconcileIsIdempotent asserts a second reconcile of an unchanged ready
// Backend is a no-op (no error, no requeue, store unchanged).
func TestReconcileIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	key := createBackend(ctx, t, makeBackend(ns, "backend-idem", "api-idem.example.test"))

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	gen1 := getBackend(ctx, t, key).Status.ObservedGeneration

	res, err := reconcile(ctx, r, key)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("second reconcile RequeueAfter = %v, want 0", res.RequeueAfter)
	}
	if gen2 := getBackend(ctx, t, key).Status.ObservedGeneration; gen2 != gen1 {
		t.Errorf("observedGeneration changed on a steady-state reconcile: %d -> %d", gen1, gen2)
	}
	if store.Len() != 1 {
		t.Errorf("store has %d entries, want 1", store.Len())
	}
}

package authenticator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
	"github.com/holos-run/holos-paas/internal/authenticator"
)

// validJWKS builds a JWKS document carrying a freshly-generated RSA public key,
// so a static-JWKS Backend fixture can validate offline without a live issuer.
func validJWKS(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       priv.Public(),
		KeyID:     "key-0",
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}
	return raw
}

// discoverFailing is a DiscoverFunc that fails if ever called, used by the
// static-JWKS tests to prove OIDC discovery is skipped when spec.oidc.jwks is set.
func discoverFailing(called *bool) authenticator.DiscoverFunc {
	return func(context.Context, string, string, []byte) (authenticator.TokenVerifier, error) {
		*called = true
		return nil, fmt.Errorf("Discover must not be called when spec.oidc.jwks is set")
	}
}

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

// TestReconcileStaticJWKSReadyOffline asserts a Backend carrying a valid static
// JWKS reaches Ready=True without any OIDC discovery: the injected Discover func
// fails if invoked, proving the static path skips it. The entry registers in the
// store keyed by host exactly as the discovery path does.
func TestReconcileStaticJWKSReadyOffline(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	var discoverCalled bool
	r, store, _ := newReconciler(discoverFailing(&discoverCalled))

	b := makeBackend(ns, "backend-static", "api-static.example.test")
	b.Spec.OIDC.JWKS = validJWKS(t)
	key := createBackend(ctx, t, b)

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if discoverCalled {
		t.Fatalf("Discover was called for a static-JWKS Backend; discovery must be skipped")
	}

	got := getBackend(ctx, t, key)
	for _, ct := range []string{ConditionAccepted, ConditionProgrammed, ConditionReady} {
		if s := condStatus(got, ct); s != metav1.ConditionTrue {
			t.Errorf("%s = %q, want True", ct, s)
		}
	}
	if reason := condReason(got, ConditionReady); reason != ReasonReconciled {
		t.Errorf("Ready reason = %q, want %q", reason, ReasonReconciled)
	}
	if _, ok := store.Get("api-static.example.test"); !ok {
		t.Errorf("store missing entry for host api-static.example.test")
	}
}

// TestReconcileMalformedStaticJWKSRejects asserts a Backend whose spec.oidc.jwks
// is unparseable is rejected as an invalid spec (Accepted/Ready=False, reason
// InvalidSpec) — not a transient DiscoveryFailed — never requeues with an error,
// leaves nothing in the store, and never calls Discover.
func TestReconcileMalformedStaticJWKSRejects(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	var discoverCalled bool
	r, store, _ := newReconciler(discoverFailing(&discoverCalled))

	b := makeBackend(ns, "backend-bad-jwks", "api-badjwks.example.test")
	b.Spec.OIDC.JWKS = []byte("not json")
	key := createBackend(ctx, t, b)

	res, err := reconcile(ctx, r, key)
	if err != nil {
		t.Fatalf("reconcile returned error for an invalid JWKS (should not requeue): %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 for a terminal rejection", res.RequeueAfter)
	}
	if discoverCalled {
		t.Fatalf("Discover was called for a static-JWKS Backend; discovery must be skipped")
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

// TestBackendCredentialRefMutualExclusion asserts the CRD CEL validation rule
// (x-kubernetes-validations on BackendSpec) rejects a Backend that sets BOTH
// credentialsSecretRef and serviceAccountRef, and accepts a Backend that sets
// only one, or neither. The rejection is enforced by the API server at admission
// time (HOL-1399), so the assertion is on the Create error, not the reconciler.
func TestBackendCredentialRefMutualExclusion(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	secretRef := &authenticatorv1alpha1.SecretReference{Name: "custom-creds"}
	saRef := &authenticatorv1alpha1.ServiceAccountReference{Name: "custom-impersonator"}

	cases := []struct {
		name       string
		secretRef  *authenticatorv1alpha1.SecretReference
		saRef      *authenticatorv1alpha1.ServiceAccountReference
		wantReject bool
	}{
		{name: "both-rejected", secretRef: secretRef, saRef: saRef, wantReject: true},
		{name: "only-credentialsSecretRef", secretRef: secretRef, wantReject: false},
		{name: "only-serviceAccountRef", saRef: saRef, wantReject: false},
		{name: "neither", wantReject: false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := makeBackend(ns, fmt.Sprintf("mutex-%d", i), fmt.Sprintf("api-mutex-%d.example.test", i))
			b.Spec.CredentialsSecretRef = tc.secretRef
			b.Spec.ServiceAccountRef = tc.saRef

			err := shared.k8sClient.Create(ctx, b)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("Create accepted a Backend setting both credentialsSecretRef and serviceAccountRef; want CEL rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("Create rejected a valid Backend (%s): %v", tc.name, err)
			}
			t.Cleanup(func() { _ = shared.k8sClient.Delete(context.Background(), b) })
		})
	}
}

// TestBackendGroupsPrefixMutualExclusion asserts the CRD CEL validation rule
// (x-kubernetes-validations on BackendSpec) rejects a Backend that sets BOTH
// oidc.groupsPrefix and groupMapping.celExpression, and admits a Backend that
// sets only oidc.groupsPrefix (which round-trips). The rejection is enforced by
// the API server at admission time (HOL-1406), so the assertion is on the Create
// error, not the reconciler.
func TestBackendGroupsPrefixMutualExclusion(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	cases := []struct {
		name          string
		groupsPrefix  string
		celExpression string
		wantReject    bool
	}{
		{name: "both-rejected", groupsPrefix: "oidc:", celExpression: "claims.groups", wantReject: true},
		{name: "only-groupsPrefix", groupsPrefix: "oidc:", wantReject: false},
		{name: "only-celExpression", celExpression: "claims.groups", wantReject: false},
		{name: "neither", wantReject: false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := makeBackend(ns, fmt.Sprintf("gp-mutex-%d", i), fmt.Sprintf("api-gp-mutex-%d.example.test", i))
			b.Spec.OIDC.GroupsPrefix = tc.groupsPrefix
			b.Spec.GroupMapping.CELExpression = tc.celExpression

			err := shared.k8sClient.Create(ctx, b)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("Create accepted a Backend setting both oidc.groupsPrefix and groupMapping.celExpression; want CEL rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("Create rejected a valid Backend (%s): %v", tc.name, err)
			}
			t.Cleanup(func() { _ = shared.k8sClient.Delete(context.Background(), b) })

			// The only-groupsPrefix case must round-trip the field through the API server.
			if tc.groupsPrefix != "" {
				got := getBackend(ctx, t, client.ObjectKeyFromObject(b))
				if got.Spec.OIDC.GroupsPrefix != tc.groupsPrefix {
					t.Errorf("oidc.groupsPrefix = %q, want %q after round-trip", got.Spec.OIDC.GroupsPrefix, tc.groupsPrefix)
				}
			}
		})
	}
}

// TestBackendServiceAccountRefDefaults asserts the CRD applies the
// ServiceAccountReference defaults — name holos-authenticator-impersonator and
// expirationSeconds 3600 — when a Backend sets serviceAccountRef with those
// subfields omitted.
func TestBackendServiceAccountRefDefaults(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	b := makeBackend(ns, "sa-defaults", "api-sa-defaults.example.test")
	// An empty ServiceAccountReference: the CRD should default name and
	// expirationSeconds.
	b.Spec.ServiceAccountRef = &authenticatorv1alpha1.ServiceAccountReference{}
	key := createBackend(ctx, t, b)
	t.Cleanup(func() { _ = shared.k8sClient.Delete(context.Background(), b) })

	got := getBackend(ctx, t, key)
	saRef := got.Spec.ServiceAccountRef
	if saRef == nil {
		t.Fatalf("serviceAccountRef is nil after create")
	}
	if saRef.Name != authenticatorv1alpha1.DefaultImpersonatorServiceAccountName {
		t.Errorf("serviceAccountRef.name = %q, want %q", saRef.Name, authenticatorv1alpha1.DefaultImpersonatorServiceAccountName)
	}
	if saRef.ExpirationSeconds == nil {
		t.Fatalf("serviceAccountRef.expirationSeconds is nil, want defaulted 3600")
	}
	if *saRef.ExpirationSeconds != 3600 {
		t.Errorf("serviceAccountRef.expirationSeconds = %d, want 3600", *saRef.ExpirationSeconds)
	}
}

// TestReconcileRecordsServiceAccountRef asserts that reconciling a Backend whose
// spec sets serviceAccountRef records the resolved ref (normalized with its
// defaults) on the Store Entry the Check path reads, and leaves CredentialsSecretRef
// at its zero value (HOL-1400). It is the reconciler half of the Check path's
// serviceAccountRef credential source.
func TestReconcileRecordsServiceAccountRef(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	b := makeBackend(ns, "sa-record", "api-sa-record.example.test")
	// Set only the name and audience; expirationSeconds is defaulted by the CRD.
	b.Spec.ServiceAccountRef = &authenticatorv1alpha1.ServiceAccountReference{
		Name:     "custom-impersonator",
		Audience: "https://kubernetes.default.svc",
	}
	key := createBackend(ctx, t, b)
	t.Cleanup(func() { _ = shared.k8sClient.Delete(context.Background(), b) })

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	entry, ok := store.Get("api-sa-record.example.test")
	if !ok {
		t.Fatalf("store missing entry for host api-sa-record.example.test")
	}
	if entry.ServiceAccountRef == nil {
		t.Fatalf("entry.ServiceAccountRef is nil; want the recorded ref")
	}
	if got := entry.ServiceAccountRef.Name; got != "custom-impersonator" {
		t.Errorf("entry.ServiceAccountRef.Name = %q, want %q", got, "custom-impersonator")
	}
	if got := entry.ServiceAccountRef.Audience; got != "https://kubernetes.default.svc" {
		t.Errorf("entry.ServiceAccountRef.Audience = %q, want %q", got, "https://kubernetes.default.svc")
	}
	if entry.ServiceAccountRef.ExpirationSeconds == nil || *entry.ServiceAccountRef.ExpirationSeconds != 3600 {
		t.Errorf("entry.ServiceAccountRef.ExpirationSeconds = %v, want 3600", entry.ServiceAccountRef.ExpirationSeconds)
	}
	if entry.CredentialsSecretRef != (authenticatorv1alpha1.SecretReference{}) {
		t.Errorf("entry.CredentialsSecretRef = %+v, want zero value", entry.CredentialsSecretRef)
	}
}

// TestReconcileNormalizesServiceAccountRefDefaults asserts the reconciler applies
// the ServiceAccountReference defaults defensively when building the Entry even if
// the spec's subfields are unset — so the Check path's TokenManager always receives
// a fully-resolved name and expiration. (The CRD also defaults these; this guards
// the Entry path independently.)
func TestReconcileNormalizesServiceAccountRefDefaults(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	r, store, _ := newReconciler(discoverOK)
	b := makeBackend(ns, "sa-normalize", "api-sa-normalize.example.test")
	b.Spec.ServiceAccountRef = &authenticatorv1alpha1.ServiceAccountReference{}
	key := createBackend(ctx, t, b)
	t.Cleanup(func() { _ = shared.k8sClient.Delete(context.Background(), b) })

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	entry, ok := store.Get("api-sa-normalize.example.test")
	if !ok {
		t.Fatalf("store missing entry")
	}
	saRef := entry.ServiceAccountRef
	if saRef == nil {
		t.Fatalf("entry.ServiceAccountRef is nil")
	}
	if saRef.Name != authenticatorv1alpha1.DefaultImpersonatorServiceAccountName {
		t.Errorf("entry SA name = %q, want %q", saRef.Name, authenticatorv1alpha1.DefaultImpersonatorServiceAccountName)
	}
	if saRef.ExpirationSeconds == nil || *saRef.ExpirationSeconds != 3600 {
		t.Errorf("entry SA expirationSeconds = %v, want 3600", saRef.ExpirationSeconds)
	}
}

package quay

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// makeNamespace creates a uniquely-named namespace so each test's Organization,
// credential Secret, and the controller namespace are isolated. The returned name
// is used both for the controller namespace and the resource namespace (the
// reconciler resolves the credential from the controller namespace, which the
// test sets to this same namespace).
func makeNamespace(ctx context.Context, t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := shared.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shared.k8sClient.Delete(cleanupCtx, ns)
	})
	return ns.Name
}

// makeOrg creates an Organization CR named orgName in namespace ns and returns
// its object key. credSecret, when non-empty, sets the credentialsSecretRef name;
// adopt sets spec.adopt.
func makeOrg(ctx context.Context, t *testing.T, ns, orgName, credSecret string, adopt bool) client.ObjectKey {
	t.Helper()
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: orgName},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:  orgName,
			Email: orgName + "@example.test",
			Adopt: adopt,
		},
	}
	if credSecret != "" {
		org.Spec.CredentialsSecretRef = &quayv1alpha1.SecretReference{Name: credSecret}
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	return client.ObjectKeyFromObject(org)
}

func TestOrganizationCRDRejectsInvalidQuayInputs(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)

	tests := []struct {
		name string
		spec quayv1alpha1.OrganizationSpec
	}{{
		name: "short organization name",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "a",
			Email: "org@example.test",
		},
	}, {
		name: "invalid organization separator",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "org---name",
			Email: "org@example.test",
		},
	}, {
		name: "single-label email domain",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "validorg",
			Email: "org@localhost",
		},
	}, {
		name: "email local part with whitespace",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "validorg",
			Email: "bad domain@example.com",
		},
	}, {
		name: "email domain with slash",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "validorg",
			Email: "org@exa/mple.test",
		},
	}, {
		name: "email domain label with whitespace",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "validorg",
			Email: "org@example. test",
		},
	}, {
		name: "short synced team name",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "validorg",
			Email: "org@example.test",
			SyncedTeams: []quayv1alpha1.SyncedTeam{{
				Name:      "a",
				OIDCGroup: "group",
				Role:      quayv1alpha1.OrganizationTeamRoleMember,
			}},
		},
	}, {
		name: "invalid synced team separator",
		spec: quayv1alpha1.OrganizationSpec{
			Name:  "validorg",
			Email: "org@example.test",
			SyncedTeams: []quayv1alpha1.SyncedTeam{{
				Name:      "team---name",
				OIDCGroup: "group",
				Role:      quayv1alpha1.OrganizationTeamRoleMember,
			}},
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			org := &quayv1alpha1.Organization{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "invalid-",
					Namespace:    ns,
				},
				Spec: tt.spec,
			}
			err := shared.k8sClient.Create(ctx, org)
			if !apierrors.IsInvalid(err) {
				t.Fatalf("Create() error = %v, want invalid", err)
			}
		})
	}
}

// getOrg fetches the current Organization state.
func getOrg(ctx context.Context, t *testing.T, key client.ObjectKey) *quayv1alpha1.Organization {
	t.Helper()
	org := &quayv1alpha1.Organization{}
	if err := shared.k8sClient.Get(ctx, key, org); err != nil {
		t.Fatalf("getting Organization: %v", err)
	}
	return org
}

// conditionStatus returns the status of the named condition, or "" if absent.
func conditionStatus(org *quayv1alpha1.Organization, condType string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(org.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Status
}

// conditionReason returns the reason of the named condition, or "" if absent.
func conditionReason(org *quayv1alpha1.Organization, condType string) string {
	c := meta.FindStatusCondition(org.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Reason
}

// reconcileUntilStable drives Reconcile repeatedly (the first pass adds the
// finalizer and requeues) until it returns without requeueing, or fails the test
// after a small bound. It returns the final result/error.
func reconcileUntilStable(ctx context.Context, t *testing.T, r *OrganizationReconciler, key client.ObjectKey) error {
	t.Helper()
	var lastErr error
	for i := 0; i < 5; i++ {
		res, err := reconcile(ctx, r, key)
		lastErr = err
		if err != nil {
			return err
		}
		// The first pass requeues immediately (RequeueAfter == requeueImmediately)
		// after adding the finalizer; keep looping only for that. The Organization
		// reconciler sets no other RequeueAfter, so any other result is stable.
		if res.RequeueAfter != requeueImmediately {
			return nil
		}
	}
	return lastErr
}

func TestReconcileCreatesOrganization(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "acme", "", false)

	fake := newFakeOrgClient() // acme does not exist yet
	r, recorder := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !fake.callsContain("Create:acme") {
		t.Errorf("expected a Create call for acme, calls were %v", fake.calls)
	}
	if !fake.orgExists("acme") {
		t.Error("expected acme to exist in Quay after reconcile")
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonCreated {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonCreated)
	}
	if !statusCreated(org) {
		t.Error("expected status.Created = true after creating the org")
	}
	if org.Status.ObservedGeneration != org.Generation {
		t.Errorf("observedGeneration = %d, want %d", org.Status.ObservedGeneration, org.Generation)
	}
	if org.Status.LastValidatedTime == nil {
		t.Errorf("lastValidatedTime not set on successful reconcile")
	}
	if org.Status.LastMutatedTime == nil || org.Status.LastMutationReason != quayv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want time with %q", org.Status.LastMutatedTime, org.Status.LastMutationReason, quayv1alpha1.MutationReasonSpecChange)
	}
	firstValidated := org.Status.LastValidatedTime.DeepCopy()
	firstMutated := org.Status.LastMutatedTime.DeepCopy()
	assertEvent(t, recorder, quayv1alpha1.ReasonCreated)

	time.Sleep(time.Second + 100*time.Millisecond)
	result, err := reconcile(ctx, r, key)
	if err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if result.RequeueAfter != quayExternalResourceResync {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, quayExternalResourceResync)
	}
	org = getOrg(ctx, t, key)
	if !org.Status.LastValidatedTime.After(firstValidated.Time) {
		t.Errorf("lastValidatedTime did not advance: first=%v second=%v", firstValidated, org.Status.LastValidatedTime)
	}
	if !org.Status.LastMutatedTime.Equal(firstMutated) {
		t.Errorf("lastMutatedTime changed on steady validation: first=%v second=%v", firstMutated, org.Status.LastMutatedTime)
	}
	assertNoEvent(t, recorder, quayv1alpha1.ReasonCreated)
}

func TestReconcileThreadsCABundleToClientFactory(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}

	caBundle := validTestCABundle(t)
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "acme"},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:     "acme",
			Email:    "acme@example.test",
			CABundle: caBundle,
		},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	key := client.ObjectKeyFromObject(org)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if string(fake.gotCABundle) != string(caBundle) {
		t.Errorf("ClientFactory received caBundle %q, want the spec's %q", fake.gotCABundle, caBundle)
	}
}

func TestReconcileInvalidCABundleFailsWithoutQuayCall(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}

	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "acme"},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:     "acme",
			Email:    "acme@example.test",
			CABundle: []byte("not a pem block"),
		},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	key := client.ObjectKeyFromObject(org)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	// reconcileUntilStable returns the reconcile error; an invalid caBundle must
	// fail the reconcile (so it requeues with backoff).
	if err := reconcileUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail for an invalid caBundle")
	}

	if len(fake.calls) != 0 {
		t.Errorf("expected no Quay calls for an invalid caBundle, calls were %v", fake.calls)
	}
	got := getOrg(ctx, t, key)
	if s := conditionStatus(got, quayv1alpha1.ConditionReady); s != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False for an invalid caBundle", s)
	}
	if reason := conditionReason(got, quayv1alpha1.ConditionReady); reason != quayv1alpha1.ReasonQuayError {
		t.Errorf("Ready reason = %q, want %q", reason, quayv1alpha1.ReasonQuayError)
	}
}

func TestReconcileAdoptsExistingOrganization(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "preexisting", "", true) // adopt: true

	fake := newFakeOrgClient("preexisting") // already exists
	r, recorder := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if fake.callsContain("Create:preexisting") {
		t.Errorf("did not expect a Create call when adopting; calls were %v", fake.calls)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonAdopted {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonAdopted)
	}
	if statusCreated(org) {
		t.Error("expected status.Created = false for an adopted (not created) org")
	}
	if desc, want := fake.markers["preexisting"], organizationOwnerToken(org, false); desc != want {
		t.Errorf("adopted marker = %q, want %q", desc, want)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonAdopted)
}

func TestReconcileHealsLegacyOrganizationMarker(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "legacy-marker", "", false)

	fake := newFakeOrgClient("legacy-marker")
	org := getOrg(ctx, t, key)
	fake.setMarker("legacy-marker", string(org.UID))
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	org = getOrg(ctx, t, key)
	if !statusCreated(org) {
		t.Fatal("legacy bare marker should be treated as created provenance")
	}
	if desc, want := fake.markers["legacy-marker"], organizationOwnerToken(org, true); desc != want {
		t.Errorf("marker = %q, want healed %q", desc, want)
	}
}

func TestReconcileRestoresLegacyMarkerWhenHealCreateFails(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "legacy-restore", "", false)

	fake := newFakeOrgClient("legacy-restore")
	org := getOrg(ctx, t, key)
	legacy := string(org.UID)
	fake.setMarker("legacy-restore", legacy)
	fake.robotCreateErrs = []error{
		&quay.APIError{StatusCode: http.StatusInternalServerError, Message: "create new marker failed"},
		nil,
	}
	r, _ := newReconciler(fake, ns)

	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	if _, err := reconcile(ctx, r, key); err == nil {
		t.Fatal("expected marker heal failure to surface as a reconcile error")
	}

	if desc := fake.markers["legacy-restore"]; desc != legacy {
		t.Fatalf("marker = %q, want restored legacy marker %q", desc, legacy)
	}
	org = getOrg(ctx, t, key)
	if org.Status.LastMutatedTime == nil {
		t.Fatal("lastMutatedTime not stamped after marker replacement mutation")
	}
}

func TestReconcileQuayErrorSetsReadyFalse(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "boom", "", false)

	fake := newFakeOrgClient()
	// Simulate a non-404 Quay error on GET (e.g. 500 / auth failure).
	fake.getErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	r, recorder := newReconciler(fake, ns)

	// First pass adds the finalizer; second performs the failing reconcile.
	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	_, err := reconcile(ctx, r, key)
	if err == nil {
		t.Fatal("expected reconcile to return an error so it requeues")
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonQuayError {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonQuayError)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonQuayError)
}

func TestReconcileMissingCredentialSecretSetsConditionAndNoQuayCall(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	// Deliberately do NOT create the credential Secret.
	key := makeOrg(ctx, t, ns, "nocreds", "", false)

	fake := newFakeOrgClient()
	r, recorder := newReconciler(fake, ns)

	// First pass adds the finalizer; second hits the missing credential.
	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	_, err := reconcile(ctx, r, key)
	if err == nil {
		t.Fatal("expected reconcile to return an error so it requeues for the missing Secret")
	}

	if len(fake.calls) != 0 {
		t.Errorf("expected no Quay calls when the credential Secret is missing, got %v", fake.calls)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonCredentialsNotFound {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonCredentialsNotFound)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonCredentialsNotFound)
}

func TestReconcileDeleteRemovesFinalizerAfterQuayDelete(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "doomed", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	// Provision the org and finalizer.
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if !fake.orgExists("doomed") {
		t.Fatal("expected doomed to exist before delete")
	}

	// Delete the CR — the finalizer keeps it around until the reconciler runs.
	org := getOrg(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if !fake.callsContain("Delete:doomed") {
		t.Errorf("expected a Delete call for doomed, calls were %v", fake.calls)
	}
	if fake.orgExists("doomed") {
		t.Error("expected doomed to be removed from Quay after delete")
	}

	// The CR should now be fully gone (finalizer removed → API server deletes it).
	if err := shared.k8sClient.Get(ctx, key, &quayv1alpha1.Organization{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Organization to be deleted, get returned %v", err)
	}
}

func TestReconcileConflictWhenExistingOrgNotAdopted(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// The org already exists in Quay but this CR did not create it and does not
	// set adopt — the claim model must refuse to seize it.
	key := makeOrg(ctx, t, ns, "foreign", "", false)

	fake := newFakeOrgClient("foreign") // pre-existing, externally created
	r, recorder := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		// Conflict is a terminal state, not an error/requeue storm.
		t.Fatalf("reconcile should not error on a conflict: %v", err)
	}

	if fake.callsContain("Create:foreign") {
		t.Errorf("must not create when an existing org is unowned; calls were %v", fake.calls)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonConflict {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonConflict)
	}
	if statusCreated(org) {
		t.Error("expected status.Created = false on a conflict")
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonConflict)
}

func TestReconcileCreateRaceDoesNotClaimOwnership(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// No adopt: the org does not exist at GET time, but another actor creates it
	// before our POST, which then returns a conflict. We must NOT stamp ownership.
	key := makeOrg(ctx, t, ns, "raced", "", false)

	fake := newFakeOrgClient()        // GET returns 404...
	fake.createRaceExisting = "raced" // ...but Create returns a conflict
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile should not error on a create-race conflict: %v", err)
	}

	org := getOrg(ctx, t, key)
	if statusCreated(org) {
		t.Error("status.Created must be false after losing a create race — the org was not created by this CR")
	}
	// Without adopt, a raced-in org is a Conflict, not a silent claim.
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonConflict {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonConflict)
	}
}

func TestReconcileDeleteReleasesAdoptedOrgWithoutDeleting(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// adopt: true claims a pre-existing org → status.Created stays false.
	key := makeOrg(ctx, t, ns, "shared-org", "", true)

	fake := newFakeOrgClient("shared-org") // pre-existing
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile adopt: %v", err)
	}
	if statusCreated(getOrg(ctx, t, key)) {
		t.Fatal("expected adopted org status.Created = false")
	}

	// Delete the CR; the finalizer must release (not delete) the Quay org.
	org := getOrg(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("Delete:shared-org") {
		t.Errorf("must NOT delete an adopted org; calls were %v", fake.calls)
	}
	if _, ok := fake.markers["shared-org"]; ok {
		t.Errorf("adopted marker should be stripped on release, markers=%v", fake.markers)
	}
	if !fake.orgExists("shared-org") {
		t.Error("expected the adopted org to survive CR deletion")
	}
	// The CR itself is gone (finalizer released).
	if err := shared.k8sClient.Get(ctx, key, &quayv1alpha1.Organization{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Organization CR to be deleted, get returned %v", err)
	}
}

func TestReconcileDeleteOrphansCreatedOrg(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "orphan-org", "", false)

	fake := newFakeOrgClient()
	r, recorder := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}

	org := getOrg(ctx, t, key)
	org.Spec.DeletionPolicy = quayv1alpha1.DeletionPolicyOrphan
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("setting deletionPolicy: %v", err)
	}
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("Delete:orphan-org") {
		t.Errorf("orphan policy must not delete org; calls were %v", fake.calls)
	}
	if !fake.callsContain("DeleteRobot:orphan-org") {
		t.Errorf("orphan policy should remove the ownership marker; calls were %v", fake.calls)
	}
	if !fake.orgExists("orphan-org") {
		t.Fatal("expected orphaned org to remain in Quay")
	}
	if _, ok := fake.markers["orphan-org"]; ok {
		t.Errorf("marker should be removed on orphan, markers=%v", fake.markers)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonReleased)
}

func TestReconcileAdoptsHolosMarkedTeamsAfterOrgOrphanTransfer(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	old := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "old-owner"},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:  "transfer-org",
			Email: "transfer-org@example.test",
			SyncedTeams: []quayv1alpha1.SyncedTeam{{
				Name:      "devs",
				OIDCGroup: "devs",
				Role:      quayv1alpha1.OrganizationTeamRoleMember,
			}},
		},
	}
	if err := shared.k8sClient.Create(ctx, old); err != nil {
		t.Fatalf("creating old Organization: %v", err)
	}
	oldKey := client.ObjectKeyFromObject(old)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, oldKey); err != nil {
		t.Fatalf("reconcile old owner: %v", err)
	}
	if role, ok := fake.teamRole("transfer-org", "devs"); !ok || role != "member" {
		t.Fatalf("old owner did not create team devs, role=%q exists=%v", role, ok)
	}

	old = getOrg(ctx, t, oldKey)
	old.Spec.DeletionPolicy = quayv1alpha1.DeletionPolicyOrphan
	if err := shared.k8sClient.Update(ctx, old); err != nil {
		t.Fatalf("setting old deletionPolicy: %v", err)
	}
	if err := shared.k8sClient.Delete(ctx, old); err != nil {
		t.Fatalf("deleting old Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, oldKey); err != nil {
		t.Fatalf("reconcile old orphan: %v", err)
	}

	replacement := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "new-owner"},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:  "transfer-org",
			Email: "transfer-org@example.test",
			Adopt: true,
			SyncedTeams: []quayv1alpha1.SyncedTeam{{
				Name:      "devs",
				OIDCGroup: "devs",
				Role:      quayv1alpha1.OrganizationTeamRoleCreator,
			}},
		},
	}
	if err := shared.k8sClient.Create(ctx, replacement); err != nil {
		t.Fatalf("creating replacement Organization: %v", err)
	}
	newKey := client.ObjectKeyFromObject(replacement)
	if err := reconcileUntilStable(ctx, t, r, newKey); err != nil {
		t.Fatalf("reconcile replacement adopt: %v", err)
	}

	replacement = getOrg(ctx, t, newKey)
	if got := conditionStatus(replacement, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Fatalf("replacement Ready = %q, want True", got)
	}
	if !containsString(replacement.Status.ManagedTeams, "devs") {
		t.Fatalf("replacement ManagedTeams = %v, want devs", replacement.Status.ManagedTeams)
	}
	if role, ok := fake.teamRole("transfer-org", "devs"); !ok || role != "creator" {
		t.Fatalf("team devs role = %q exists=%v, want adopted and updated to creator", role, ok)
	}
	if desc := fake.teams[teamKey("transfer-org", "devs")].Description; desc != managedTeamMarker(replacement) {
		t.Fatalf("team marker = %q, want replacement marker %q", desc, managedTeamMarker(replacement))
	}
}

func TestReconcileDeleteDeletesAdoptedOrgWhenPolicyDeleteAndOwned(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "delete-adopted", "", true)

	fake := newFakeOrgClient("delete-adopted")
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile adopt: %v", err)
	}

	org := getOrg(ctx, t, key)
	org.Spec.DeletionPolicy = quayv1alpha1.DeletionPolicyDelete
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("setting deletionPolicy: %v", err)
	}
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if !fake.callsContain("Delete:delete-adopted") {
		t.Errorf("expected explicit Delete policy to delete owned adopted org; calls were %v", fake.calls)
	}
	if fake.orgExists("delete-adopted") {
		t.Fatal("expected adopted org to be deleted")
	}
}

func TestReconcileDeletePolicyDeleteReleasesOrgWithForeignMarker(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "foreign-delete", "", true)

	fake := newFakeOrgClient("foreign-delete")
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile adopt: %v", err)
	}
	fake.setMarker("foreign-delete", "created:foreign")

	org := getOrg(ctx, t, key)
	org.Spec.DeletionPolicy = quayv1alpha1.DeletionPolicyDelete
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("setting deletionPolicy: %v", err)
	}
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("Delete:foreign-delete") {
		t.Errorf("foreign marker must release, not delete; calls were %v", fake.calls)
	}
	if !fake.orgExists("foreign-delete") {
		t.Fatal("expected foreign-owned org to remain in Quay")
	}
}

func TestReconcileDeletePolicyDeleteReleasesLegacyAdoptedOrgWithoutMarker(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "legacy-adopted", "", true)

	fake := newFakeOrgClient("legacy-adopted")
	r, recorder := newReconciler(fake, ns)
	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	org := getOrg(ctx, t, key)
	org.Spec.DeletionPolicy = quayv1alpha1.DeletionPolicyDelete
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("setting deletionPolicy: %v", err)
	}
	org = getOrg(ctx, t, key)
	setStatusCreated(org, false)
	if err := shared.k8sClient.Status().Update(ctx, org); err != nil {
		t.Fatalf("seeding adopted status: %v", err)
	}
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("Delete:legacy-adopted") {
		t.Errorf("missing marker must release, not delete; calls were %v", fake.calls)
	}
	if !fake.orgExists("legacy-adopted") {
		t.Fatal("expected legacy adopted org to remain in Quay")
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonReleased)
}

func TestReconcileHonorsCredentialSecretRefKey(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	// A credential Secret whose token lives under a custom key, not "token".
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "holos-controller-quay-creds"},
		Data: map[string][]byte{
			credentialKeyURL: []byte("https://quay.example.test"),
			"oauth":          []byte("token-under-custom-key"),
		},
	}
	if err := shared.k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}

	// Point credentialsSecretRef.key at the custom token key.
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "keyed"},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:                 "keyed",
			Email:                "keyed@example.test",
			CredentialsSecretRef: &quayv1alpha1.SecretReference{Key: "oauth"},
		},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	key := client.ObjectKeyFromObject(org)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The token resolved from the custom key, so the reconcile reached Quay and
	// created the org (rather than failing CredentialsNotFound on the missing
	// default "token" key).
	if !fake.orgExists("keyed") {
		t.Error("expected keyed to be created; the custom credential key was not honored")
	}
	if got := conditionReason(getOrg(ctx, t, key), quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonCreated {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonCreated)
	}
}

func TestReconcileStampsOwnershipMarkerOnCreate(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "marked", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The durable server-side marker must be stamped with the CR's UID.
	org := getOrg(ctx, t, key)
	desc, ok := fake.markers["marked"]
	if !ok {
		t.Fatalf("expected ownership marker robot to be stamped, markers=%v", fake.markers)
	}
	if want := organizationOwnerToken(org, true); desc != want {
		t.Errorf("marker description = %q, want %q", desc, want)
	}
	if !statusCreated(org) {
		t.Error("expected status.Created = true after a marked create")
	}
}

func TestReconcileAppliesEmailDriftToOwnedOrg(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "drifter", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	// First reconcile creates the org with the spec email and stamps the marker.
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if got := fake.emails["drifter"]; got != "drifter@example.test" {
		t.Fatalf("created email = %q, want drifter@example.test", got)
	}

	// Change the spec email; a subsequent reconcile must push the drift to Quay.
	org := getOrg(ctx, t, key)
	org.Spec.Email = "changed@example.test"
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("updating spec email: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile drift: %v", err)
	}

	if !fake.callsContain("Update:drifter:changed@example.test") {
		t.Errorf("expected an UpdateOrganization call for the new email, calls were %v", fake.calls)
	}
	if got := fake.emails["drifter"]; got != "changed@example.test" {
		t.Errorf("Quay email = %q, want changed@example.test after drift", got)
	}

	// A re-reconcile with no further drift must not call Update again (idempotent).
	before := len(fake.calls)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile steady: %v", err)
	}
	for _, c := range fake.calls[before:] {
		if strings.HasPrefix(c, "Update:") {
			t.Errorf("unexpected UpdateOrganization on a no-drift reconcile: %v", fake.calls[before:])
		}
	}
}

func TestReconcileStampsEmailMutationWhenTeamsFail(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "emailfail", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	before := getOrg(ctx, t, key)
	firstValidated := before.Status.LastValidatedTime.DeepCopy()
	firstMutated := before.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	org := getOrg(ctx, t, key)
	org.Spec.Email = "changed@example.test"
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("updating spec email: %v", err)
	}
	fake.listTeamsErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "teams boom"}

	if err := reconcileUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail after applying email drift")
	}

	if got := fake.emails["emailfail"]; got != "changed@example.test" {
		t.Fatalf("Quay email = %q, want changed@example.test despite later team failure", got)
	}
	org = getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False after team failure", got)
	}
	if org.Status.LastValidatedTime == nil || !org.Status.LastValidatedTime.Equal(firstValidated) {
		t.Errorf("lastValidatedTime changed on failed reconcile: before=%v after=%v", firstValidated, org.Status.LastValidatedTime)
	}
	if org.Status.LastMutatedTime == nil || !org.Status.LastMutatedTime.After(firstMutated.Time) {
		t.Errorf("lastMutatedTime did not advance after partial mutation: before=%v after=%v", firstMutated, org.Status.LastMutatedTime)
	}
	if got := org.Status.LastMutationReason; got != quayv1alpha1.MutationReasonSpecChange {
		t.Errorf("lastMutationReason = %q, want %q", got, quayv1alpha1.MutationReasonSpecChange)
	}
	if org.Status.LastDriftTime != nil {
		t.Errorf("lastDriftTime = %v, want nil for a pure spec-driven email update", org.Status.LastDriftTime)
	}
}

func TestReconcileHealsLostCreatedMarkerWithoutReleasing(t *testing.T) {
	// A successful create whose marker write was lost leaves status.Created true but
	// no marker on the org. The reconciler must re-stamp the marker and keep
	// ownership, never mis-classify the org as foreign and release it.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "healme", "", false)

	// The org exists in Quay with NO marker, and the CR is already recorded as the
	// creator (simulating the lost status/marker write).
	fake := newFakeOrgClient("healme")
	r, _ := newReconciler(fake, ns)

	org := getOrg(ctx, t, key)
	setStatusCreated(org, true)
	if err := shared.k8sClient.Status().Update(ctx, org); err != nil {
		t.Fatalf("seeding status.Created: %v", err)
	}

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	org = getOrg(ctx, t, key)
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonReconciled {
		t.Errorf("Ready reason = %q, want %q (must heal, not conflict/release)", got, quayv1alpha1.ReasonReconciled)
	}
	if !statusCreated(org) {
		t.Error("status.Created must remain true after healing")
	}
	if desc, want := fake.markers["healme"], organizationOwnerToken(org, true); desc != want {
		t.Errorf("marker = %q, want %q", desc, want)
	}
}

func TestReconcileMarkerStampFailureAfterCreatePersistsCreatedAndHeals(t *testing.T) {
	// A clean create whose marker stamp then fails must still persist
	// status.Created=true (so the org is not later mistaken for foreign), and a
	// subsequent reconcile must re-stamp the marker (heal) rather than release the
	// org.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "stampfail", "", false)

	fake := newFakeOrgClient()
	// The marker stamp fails on this first reconcile (non-conflict Quay error).
	fake.robotCreateErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "robot boom"}
	r, _ := newReconciler(fake, ns)

	// Drive the finalizer add and the create; the create succeeds but the marker
	// stamp errors, so the reconcile requeues with an error.
	if _, err := reconcile(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	if _, err := reconcile(ctx, r, key); err == nil {
		t.Fatal("expected the marker-stamp failure to surface as a requeue error")
	}

	// The org was created and ownership was recorded despite the marker failure.
	if !fake.orgExists("stampfail") {
		t.Fatal("expected the org to be created")
	}
	if !statusCreated(getOrg(ctx, t, key)) {
		t.Error("status.Created must be persisted true even though the marker stamp failed")
	}

	// Clear the marker fault; the next reconcile must heal (re-stamp), not release.
	fake.robotCreateErr = nil
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile heal: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonReconciled {
		t.Errorf("Ready reason = %q, want %q after heal", got, quayv1alpha1.ReasonReconciled)
	}
	if desc, want := fake.markers["stampfail"], organizationOwnerToken(org, true); desc != want {
		t.Errorf("marker = %q, want %q", desc, want)
	}
}

func TestReconcileDeleteSucceedsBeforeMarkerCleanup(t *testing.T) {
	// The org must be deleted before the marker, so a failed org delete leaves the
	// marker intact for the retry to re-verify ownership.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "ordereddelete", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}

	org := getOrg(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	// The recorded call order must place the org delete before the marker delete.
	var orgIdx, markerIdx = -1, -1
	for i, c := range fake.calls {
		if c == "Delete:ordereddelete" && orgIdx == -1 {
			orgIdx = i
		}
		if c == "DeleteRobot:ordereddelete" && markerIdx == -1 {
			markerIdx = i
		}
	}
	if orgIdx == -1 {
		t.Fatalf("expected an org delete call, calls were %v", fake.calls)
	}
	if markerIdx != -1 && markerIdx < orgIdx {
		t.Errorf("marker delete (%d) must not precede org delete (%d); calls were %v", markerIdx, orgIdx, fake.calls)
	}
	if fake.orgExists("ordereddelete") {
		t.Error("expected the owned org to be deleted")
	}
}

func TestReconcileConflictWhenMarkerHoldsForeignToken(t *testing.T) {
	// An org marked by a DIFFERENT owner must never be seized — not even on a
	// create-race conflict path that re-evaluates ownership.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "owned-elsewhere", "", true) // even adopt: true

	fake := newFakeOrgClient("owned-elsewhere")
	fake.setMarker("owned-elsewhere", "some-other-cr-uid")
	r, recorder := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile should not error on a marker conflict: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonConflict {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonConflict)
	}
	if statusCreated(org) {
		t.Error("status.Created must stay false on a foreign-marker conflict")
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonConflict)
}

func TestReconcileDeleteReleasesWhenMarkerForeignAfterRecreate(t *testing.T) {
	// This CR created and owns the org (status.Created true), but in the delete gap
	// another actor recreated the same global org name with a foreign (or absent)
	// marker. The retried delete must NOT destroy the recreated org — it releases
	// instead.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "recreated", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)

	// Provision: create + marker + status.Created.
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}

	// Simulate the foreign recreate: the org still exists but its marker now holds
	// a foreign token (a different actor recreated it in the delete gap).
	fake.setMarker("recreated", "foreign-actor-uid")

	org := getOrg(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("deleting Organization: %v", err)
	}
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("Delete:recreated") {
		t.Errorf("must NOT delete an org whose marker is foreign; calls were %v", fake.calls)
	}
	if !fake.orgExists("recreated") {
		t.Error("expected the recreated (foreign) org to survive the delete")
	}
	// The CR itself is gone (finalizer released).
	if err := shared.k8sClient.Get(ctx, key, &quayv1alpha1.Organization{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Organization CR to be deleted, get returned %v", err)
	}
}

// makeOrgWithTeams creates an Organization CR with the given spec.syncedTeams and
// returns its object key. It mirrors makeOrg but sets the synced teams so the
// team-reconciliation path is exercised.
func makeOrgWithTeams(ctx context.Context, t *testing.T, ns, orgName string, teams []quayv1alpha1.SyncedTeam) client.ObjectKey {
	t.Helper()
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: orgName},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:        orgName,
			Email:       orgName + "@example.test",
			SyncedTeams: teams,
		},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization with teams: %v", err)
	}
	return client.ObjectKeyFromObject(org)
}

// repoRole returns a pointer to a RepositoryRole, for the optional
// SyncedTeam.RepositoryPermission field.
func repoRole(r quayv1alpha1.RepositoryRole) *quayv1alpha1.RepositoryRole { return &r }

// updateSyncedTeams sets spec.syncedTeams on the live Organization and persists it.
func updateSyncedTeams(ctx context.Context, t *testing.T, key client.ObjectKey, teams []quayv1alpha1.SyncedTeam) {
	t.Helper()
	org := getOrg(ctx, t, key)
	org.Spec.SyncedTeams = teams
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("updating spec.syncedTeams: %v", err)
	}
}

func TestReconcileSyncedTeamsZeroIsNoop(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrg(ctx, t, ns, "noteams", "", false)

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if len(org.Status.ManagedTeams) != 0 {
		t.Errorf("ManagedTeams = %v, want empty for an org with no synced teams", org.Status.ManagedTeams)
	}
	// No team operations should have run (only org/marker calls).
	for _, c := range fake.calls {
		if strings.HasPrefix(c, "UpsertTeam:") || strings.HasPrefix(c, "EnableSync:") {
			t.Errorf("unexpected team call on a zero-team org: %v", fake.calls)
		}
	}
}

func TestReconcileSyncedTeamsCreatesTeamSyncAndDefaultPermission(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "withteam", []quayv1alpha1.SyncedTeam{
		{
			Name:                 "devs",
			OIDCGroup:            "dev-group",
			Role:                 quayv1alpha1.OrganizationTeamRoleMember,
			RepositoryPermission: repoRole(quayv1alpha1.RepositoryRoleWrite),
		},
	})

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if role, ok := fake.teamRole("withteam", "devs"); !ok || role != "member" {
		t.Errorf("team devs role = %q (exists=%v), want member", role, ok)
	}
	if group, ok := fake.teamGroup("withteam", "devs"); !ok || group != "dev-group" {
		t.Errorf("team devs synced group = %q (synced=%v), want dev-group", group, ok)
	}
	p, ok := fake.teamPrototype("withteam", "devs")
	if !ok || p.Role != "write" {
		t.Errorf("default permission = %+v (exists=%v), want role write delegating to devs", p, ok)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if !containsString(org.Status.ManagedTeams, "devs") {
		t.Errorf("ManagedTeams = %v, want to contain devs", org.Status.ManagedTeams)
	}
}

func TestReconcileSyncedTeamsRoleDrift(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "roledrift", []quayv1alpha1.SyncedTeam{
		{Name: "ops", OIDCGroup: "ops-group", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}

	// Change the team role; a subsequent reconcile must upsert the new role.
	updateSyncedTeams(ctx, t, key, []quayv1alpha1.SyncedTeam{
		{Name: "ops", OIDCGroup: "ops-group", Role: quayv1alpha1.OrganizationTeamRoleCreator},
	})
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile drift: %v", err)
	}

	if role, _ := fake.teamRole("roledrift", "ops"); role != "creator" {
		t.Errorf("team ops role = %q after drift, want creator", role)
	}
}

func TestReconcileSyncedTeamsOIDCGroupRebind(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "rebind", []quayv1alpha1.SyncedTeam{
		{Name: "t1", OIDCGroup: "group-a", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if group, _ := fake.teamGroup("rebind", "t1"); group != "group-a" {
		t.Fatalf("initial group = %q, want group-a", group)
	}

	// Change the bound OIDC group; the reconciler must disable then re-enable sync.
	updateSyncedTeams(ctx, t, key, []quayv1alpha1.SyncedTeam{
		{Name: "t1", OIDCGroup: "group-b", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	before := len(fake.calls)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile rebind: %v", err)
	}

	if group, _ := fake.teamGroup("rebind", "t1"); group != "group-b" {
		t.Errorf("team group = %q after rebind, want group-b", group)
	}
	sawDisable := false
	for _, c := range fake.calls[before:] {
		if c == "DisableSync:rebind/t1" {
			sawDisable = true
		}
	}
	if !sawDisable {
		t.Errorf("expected a DisableSync call during oidcGroup rebind, calls were %v", fake.calls[before:])
	}
}

func TestReconcileSyncedTeamsDefaultPermissionAddChangeRemove(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// Start with no default permission.
	key := makeOrgWithTeams(ctx, t, ns, "permteam", []quayv1alpha1.SyncedTeam{
		{Name: "team", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if _, ok := fake.teamPrototype("permteam", "team"); ok {
		t.Fatal("did not expect a default permission before one is requested")
	}

	// Add a default permission (read).
	updateSyncedTeams(ctx, t, key, []quayv1alpha1.SyncedTeam{
		{Name: "team", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember, RepositoryPermission: repoRole(quayv1alpha1.RepositoryRoleRead)},
	})
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile add permission: %v", err)
	}
	p, ok := fake.teamPrototype("permteam", "team")
	if !ok || p.Role != "read" {
		t.Fatalf("default permission = %+v (exists=%v), want read", p, ok)
	}
	protoID := p.ID

	// Change the permission (read → admin): same prototype updated in place.
	updateSyncedTeams(ctx, t, key, []quayv1alpha1.SyncedTeam{
		{Name: "team", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember, RepositoryPermission: repoRole(quayv1alpha1.RepositoryRoleAdmin)},
	})
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile change permission: %v", err)
	}
	p, ok = fake.teamPrototype("permteam", "team")
	if !ok || p.Role != "admin" {
		t.Fatalf("default permission = %+v after change, want admin", p)
	}
	if p.ID != protoID {
		t.Errorf("prototype ID changed on a role update (%q → %q); want in-place update", protoID, p.ID)
	}

	// Remove the permission: the prototype is deleted.
	updateSyncedTeams(ctx, t, key, []quayv1alpha1.SyncedTeam{
		{Name: "team", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile remove permission: %v", err)
	}
	if _, ok := fake.teamPrototype("permteam", "team"); ok {
		t.Error("expected the default permission to be deleted when removed from spec")
	}
}

func TestReconcileSyncedTeamsRemovedFromSpecIsDeprovisioned(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "removal", []quayv1alpha1.SyncedTeam{
		{Name: "keep", OIDCGroup: "keep-g", Role: quayv1alpha1.OrganizationTeamRoleMember},
		{Name: "drop", OIDCGroup: "drop-g", Role: quayv1alpha1.OrganizationTeamRoleMember, RepositoryPermission: repoRole(quayv1alpha1.RepositoryRoleRead)},
	})
	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if _, ok := fake.teamRole("removal", "drop"); !ok {
		t.Fatal("expected team drop to exist before removal")
	}

	// Drop "drop" from the spec; it must be de-provisioned (team + prototype gone).
	updateSyncedTeams(ctx, t, key, []quayv1alpha1.SyncedTeam{
		{Name: "keep", OIDCGroup: "keep-g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile removal: %v", err)
	}

	if _, ok := fake.teamRole("removal", "drop"); ok {
		t.Error("expected team drop to be deleted after removal from spec")
	}
	if _, ok := fake.teamPrototype("removal", "drop"); ok {
		t.Error("expected drop's default permission to be deleted")
	}
	if _, ok := fake.teamRole("removal", "keep"); !ok {
		t.Error("expected team keep to survive")
	}
	org := getOrg(ctx, t, key)
	if containsString(org.Status.ManagedTeams, "drop") {
		t.Errorf("ManagedTeams = %v, must not contain the de-provisioned team drop", org.Status.ManagedTeams)
	}
	if !containsString(org.Status.ManagedTeams, "keep") {
		t.Errorf("ManagedTeams = %v, want to still contain keep", org.Status.ManagedTeams)
	}
}

func TestReconcileSyncedTeamsPreexistingUnmanagedIsConflict(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "teamconflict", []quayv1alpha1.SyncedTeam{
		{Name: "foreign", OIDCGroup: "want-group", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	// The org is created by this CR, but a team of the desired name pre-exists and
	// is NOT bound to the desired group (so it is not a heal candidate).
	r, recorder := newReconciler(fake, ns)
	// First create the org so the team pre-exists on an owned org; seed the foreign
	// team before reconcile by pre-seeding the fake after the org is known to exist.
	// Seed directly: the team exists with a different (or no) sync binding.
	fake.seedTeam("teamconflict", "foreign", "member", "")

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile should not error on a team conflict: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False on a team conflict", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonTeamConflict {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonTeamConflict)
	}
	if containsString(org.Status.ManagedTeams, "foreign") {
		t.Errorf("ManagedTeams = %v, must not adopt the foreign team", org.Status.ManagedTeams)
	}
	// The foreign team must not have been modified.
	if !fake.callsContain("ListTeams:teamconflict") {
		t.Errorf("expected the reconciler to list teams; calls were %v", fake.calls)
	}
	for _, c := range fake.calls {
		if c == "UpsertTeam:teamconflict/foreign:member" {
			t.Errorf("must not upsert a foreign team; calls were %v", fake.calls)
		}
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonTeamConflict)
}

func TestReconcileSyncedTeamsUnmanagedOutsideSpecIsIgnored(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "ignoreteam", []quayv1alpha1.SyncedTeam{
		{Name: "mine", OIDCGroup: "mine-g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	// A team that the controller never created and is not in the spec at all.
	fake.seedTeam("ignoreteam", "stranger", "admin", "stranger-group")
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True (a stranger team outside the spec is ignored)", got)
	}
	// The stranger team must be untouched: still present, still admin, still synced.
	if role, ok := fake.teamRole("ignoreteam", "stranger"); !ok || role != "admin" {
		t.Errorf("stranger role = %q (exists=%v), want untouched admin", role, ok)
	}
	if containsString(org.Status.ManagedTeams, "stranger") {
		t.Errorf("ManagedTeams = %v, must not include an ignored stranger team", org.Status.ManagedTeams)
	}
	if !containsString(org.Status.ManagedTeams, "mine") {
		t.Errorf("ManagedTeams = %v, want to contain mine", org.Status.ManagedTeams)
	}
}

func TestReconcileSyncedTeamsHealsLostStatus(t *testing.T) {
	// A team that pre-exists and carries this CR's managedTeamMarker (a lost status
	// write after our own create) is healed into ownership via the marker alone —
	// even when unsynced — rather than treated as an adoption conflict.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "healteam", []quayv1alpha1.SyncedTeam{
		{Name: "synced", OIDCGroup: "bound-group", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	// The team already exists and carries THIS CR's ownership marker (the prefix
	// plus the org CR UID), but status.managedTeams does NOT record it (the status
	// write was lost after a prior create). The marker alone heals it back into
	// ownership — independent of the sync binding (here it is even unsynced).
	marker := managedTeamPrefix + string(getOrg(ctx, t, key).UID)
	fake.seedTeamWithDescription("healteam", "synced", "member", marker, "")
	r, _ := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile heal: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True (a bound team must heal, not conflict)", got)
	}
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonCreated {
		t.Errorf("Ready reason = %q, want %q (healed, not TeamConflict)", got, quayv1alpha1.ReasonCreated)
	}
	if !containsString(org.Status.ManagedTeams, "synced") {
		t.Errorf("ManagedTeams = %v, want the healed team recorded", org.Status.ManagedTeams)
	}
}

func TestReconcileSyncedTeamsBoundButUnmarkedIsConflict(t *testing.T) {
	// A hand-created team that merely happens to be bound to the desired oidcGroup
	// but lacks the controller's ownership-marker description must NOT be
	// healed/adopted — it is a TeamConflict. The heal rule requires the durable
	// description marker, not just a coincidental group match.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "boundforeign", []quayv1alpha1.SyncedTeam{
		{Name: "shared", OIDCGroup: "shared-group", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	// Bound to the desired group, but with NO controller description (foreign).
	fake.seedTeam("boundforeign", "shared", "member", "shared-group")
	r, recorder := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile should not error on a team conflict: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonTeamConflict {
		t.Errorf("Ready reason = %q, want %q (a bound-but-unmarked team must not be adopted)", got, quayv1alpha1.ReasonTeamConflict)
	}
	if containsString(org.Status.ManagedTeams, "shared") {
		t.Errorf("ManagedTeams = %v, must not adopt a bound-but-unmarked foreign team", org.Status.ManagedTeams)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonTeamConflict)
}

func TestReconcileSyncedTeamsForeignMarkerIsConflict(t *testing.T) {
	// The ownership marker embeds the CR UID, so a team carrying a DIFFERENT CR's
	// marker (or any non-matching description) must not be healed — it is a conflict.
	// This is the team-level analog of the org's foreign-marker conflict.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "foreignmarker", []quayv1alpha1.SyncedTeam{
		{Name: "team", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	// Marker prefix is right but the UID is some other CR's — unforgeable, so this
	// is not our team.
	fake.seedTeamWithDescription("foreignmarker", "team", "member", managedTeamPrefix+"some-other-cr-uid", "g")
	r, recorder := newReconciler(fake, ns)

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile should not error on a team conflict: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonTeamConflict {
		t.Errorf("Ready reason = %q, want %q (a foreign-marker team must not be adopted)", got, quayv1alpha1.ReasonTeamConflict)
	}
	if containsString(org.Status.ManagedTeams, "team") {
		t.Errorf("ManagedTeams = %v, must not adopt a foreign-marker team", org.Status.ManagedTeams)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonTeamConflict)
}

func TestReconcileSyncedTeamsRecoversAfterPostUpsertFailure(t *testing.T) {
	// If UpsertTeam succeeds but the following sync step fails, the team carries this
	// CR's marker. The next reconcile must heal it back into ownership (via the
	// marker) and complete — never wedge into a false TeamConflict.
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "recover", []quayv1alpha1.SyncedTeam{
		{Name: "team", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})
	fake := newFakeOrgClient()
	// The sync step fails on the first create pass, after UpsertTeam stamps the
	// marker, so the reconcile errors and the status write does not record the team.
	fake.enableSyncErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "sync boom"}
	r, _ := newReconciler(fake, ns)

	// Drive the finalizer + create + failing sync; expect a requeue error after
	// UpsertTeam has already stamped the external team.
	if err := reconcileUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail after UpsertTeam succeeded and sync failed")
	}

	// The team was created (carries our marker) and the partial progress is now
	// persisted so the failure path records both managedTeams and mutation stamps.
	if _, ok := fake.teamRole("recover", "team"); !ok {
		t.Fatal("expected the team to be created despite the sync failure")
	}
	failed := getOrg(ctx, t, key)
	if !containsString(failed.Status.ManagedTeams, "team") {
		t.Fatalf("ManagedTeams = %v, want to contain team after partial failure", failed.Status.ManagedTeams)
	}
	if failed.Status.LastMutatedTime == nil || failed.Status.LastMutationReason != quayv1alpha1.MutationReasonSpecChange {
		t.Fatalf("mutation status = (%v, %q), want stamped SpecChange after partial failure", failed.Status.LastMutatedTime, failed.Status.LastMutationReason)
	}
	if failed.Status.LastValidatedTime != nil {
		t.Fatalf("lastValidatedTime = %v, want nil on failed validation", failed.Status.LastValidatedTime)
	}

	// Clear the sync fault; the next reconcile must heal (not conflict) and finish.
	fake.enableSyncErr = nil
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile recover: %v", err)
	}

	org := getOrg(ctx, t, key)
	if got := conditionReason(org, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonReconciled {
		t.Errorf("Ready reason = %q, want %q (healed, not TeamConflict)", got, quayv1alpha1.ReasonReconciled)
	}
	if group, ok := fake.teamGroup("recover", "team"); !ok || group != "g" {
		t.Errorf("team sync = %q (synced=%v), want bound to g after recovery", group, ok)
	}
	if !containsString(org.Status.ManagedTeams, "team") {
		t.Errorf("ManagedTeams = %v, want the recovered team recorded", org.Status.ManagedTeams)
	}
}

func TestReconcileDeprovisionFailureDoesNotRestampWithoutMutation(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "deprovision-retry", []quayv1alpha1.SyncedTeam{
		{Name: "drop", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	org := getOrg(ctx, t, key)
	org.Spec.SyncedTeams = nil
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("removing synced team from spec: %v", err)
	}
	fake.deleteTeamErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "delete boom"}

	time.Sleep(time.Second + 100*time.Millisecond)
	if err := reconcileUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected first deprovision reconcile to fail deleting the team")
	}
	failed := getOrg(ctx, t, key)
	if failed.Status.LastMutatedTime == nil {
		t.Fatal("lastMutatedTime not set after sync disable succeeded before delete failure")
	}
	firstFailureMutation := failed.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	if _, err := reconcile(ctx, r, key); err == nil {
		t.Fatal("expected second deprovision reconcile to keep failing deleting the team")
	}
	failed = getOrg(ctx, t, key)
	if !failed.Status.LastMutatedTime.Equal(firstFailureMutation) {
		t.Errorf("lastMutatedTime changed on retry without a new remote mutation: first=%v second=%v", firstFailureMutation, failed.Status.LastMutatedTime)
	}
}

func TestReconcileDeprovisionAlreadyMissingTeamConverges(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeOrgWithTeams(ctx, t, ns, "deprovision-missing", []quayv1alpha1.SyncedTeam{
		{Name: "gone", OIDCGroup: "g", Role: quayv1alpha1.OrganizationTeamRoleMember},
	})

	fake := newFakeOrgClient()
	r, _ := newReconciler(fake, ns)
	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	before := getOrg(ctx, t, key)
	firstMutated := before.Status.LastMutatedTime.DeepCopy()

	fake.mu.Lock()
	delete(fake.teams, teamKey("deprovision-missing", "gone"))
	delete(fake.teamSync, teamKey("deprovision-missing", "gone"))
	fake.mu.Unlock()

	org := getOrg(ctx, t, key)
	org.Spec.SyncedTeams = nil
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("removing synced team from spec: %v", err)
	}

	if err := reconcileUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile missing team deprovision: %v", err)
	}

	org = getOrg(ctx, t, key)
	if got := conditionStatus(org, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True when already-missing managed team converges", got)
	}
	if containsString(org.Status.ManagedTeams, "gone") {
		t.Errorf("ManagedTeams = %v, want gone removed", org.Status.ManagedTeams)
	}
	if org.Status.LastMutatedTime == nil || !org.Status.LastMutatedTime.Equal(firstMutated) {
		t.Errorf("lastMutatedTime changed for already-missing team: before=%v after=%v", firstMutated, org.Status.LastMutatedTime)
	}
}

func TestNormalizeManagedTeamsSortsAndDeduplicates(t *testing.T) {
	got := normalizeManagedTeams([]string{"beta", "alpha", "beta", "", "alpha"})
	want := []string{"alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("normalizeManagedTeams length = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeManagedTeams = %v, want %v", got, want)
		}
	}
}

// assertEvent fails the test unless the recorder captured an event whose text
// contains want. record.FakeRecorder formats events as "<Type> <Reason>
// <Message>" and delivers them on a buffered channel; this drains what is
// currently buffered.
func assertEvent(t *testing.T, recorder *record.FakeRecorder, want string) {
	t.Helper()
	for {
		select {
		case e := <-recorder.Events:
			if strings.Contains(e, want) {
				return
			}
		default:
			t.Errorf("expected an event containing %q, none found", want)
			return
		}
	}
}

func assertNoEvent(t *testing.T, recorder *record.FakeRecorder, want string) {
	t.Helper()
	for {
		select {
		case e := <-recorder.Events:
			if strings.Contains(e, want) {
				t.Errorf("unexpected event containing %q: %s", want, e)
				return
			}
		default:
			return
		}
	}
}

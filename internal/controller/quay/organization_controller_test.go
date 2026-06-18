package quay

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
		_ = shared.k8sClient.Delete(context.Background(), ns)
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
		org.Spec.CredentialsSecretRef = quayv1alpha1.SecretReference{Name: credSecret}
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	return client.ObjectKeyFromObject(org)
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
	ctx := context.Background()
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
	if got := conditionStatus(org, ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := conditionReason(org, ConditionReady); got != ReasonCreated {
		t.Errorf("Ready reason = %q, want %q", got, ReasonCreated)
	}
	if !org.Status.Created {
		t.Error("expected status.Created = true after creating the org")
	}
	if org.Status.ObservedGeneration != org.Generation {
		t.Errorf("observedGeneration = %d, want %d", org.Status.ObservedGeneration, org.Generation)
	}
	assertEvent(t, recorder, ReasonCreated)
}

func TestReconcileAdoptsExistingOrganization(t *testing.T) {
	ctx := context.Background()
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
	if got := conditionStatus(org, ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := conditionReason(org, ConditionReady); got != ReasonAdopted {
		t.Errorf("Ready reason = %q, want %q", got, ReasonAdopted)
	}
	if org.Status.Created {
		t.Error("expected status.Created = false for an adopted (not created) org")
	}
	assertEvent(t, recorder, ReasonAdopted)
}

func TestReconcileQuayErrorSetsReadyFalse(t *testing.T) {
	ctx := context.Background()
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
	if got := conditionStatus(org, ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := conditionReason(org, ConditionReady); got != ReasonQuayError {
		t.Errorf("Ready reason = %q, want %q", got, ReasonQuayError)
	}
	assertEvent(t, recorder, ReasonQuayError)
}

func TestReconcileMissingCredentialSecretSetsConditionAndNoQuayCall(t *testing.T) {
	ctx := context.Background()
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
	if got := conditionStatus(org, ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := conditionReason(org, ConditionReady); got != ReasonCredentialsNotFound {
		t.Errorf("Ready reason = %q, want %q", got, ReasonCredentialsNotFound)
	}
	assertEvent(t, recorder, ReasonCredentialsNotFound)
}

func TestReconcileDeleteRemovesFinalizerAfterQuayDelete(t *testing.T) {
	ctx := context.Background()
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
	ctx := context.Background()
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
	if got := conditionStatus(org, ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False", got)
	}
	if got := conditionReason(org, ConditionReady); got != ReasonConflict {
		t.Errorf("Ready reason = %q, want %q", got, ReasonConflict)
	}
	if org.Status.Created {
		t.Error("expected status.Created = false on a conflict")
	}
	assertEvent(t, recorder, ReasonConflict)
}

func TestReconcileCreateRaceDoesNotClaimOwnership(t *testing.T) {
	ctx := context.Background()
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
	if org.Status.Created {
		t.Error("status.Created must be false after losing a create race — the org was not created by this CR")
	}
	// Without adopt, a raced-in org is a Conflict, not a silent claim.
	if got := conditionReason(org, ConditionReady); got != ReasonConflict {
		t.Errorf("Ready reason = %q, want %q", got, ReasonConflict)
	}
}

func TestReconcileDeleteReleasesAdoptedOrgWithoutDeleting(t *testing.T) {
	ctx := context.Background()
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
	if getOrg(ctx, t, key).Status.Created {
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
	if !fake.orgExists("shared-org") {
		t.Error("expected the adopted org to survive CR deletion")
	}
	// The CR itself is gone (finalizer released).
	if err := shared.k8sClient.Get(ctx, key, &quayv1alpha1.Organization{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Organization CR to be deleted, get returned %v", err)
	}
}

func TestReconcileHonorsCredentialSecretRefKey(t *testing.T) {
	ctx := context.Background()
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
			CredentialsSecretRef: quayv1alpha1.SecretReference{Key: "oauth"},
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
	if got := conditionReason(getOrg(ctx, t, key), ConditionReady); got != ReasonCreated {
		t.Errorf("Ready reason = %q, want %q", got, ReasonCreated)
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

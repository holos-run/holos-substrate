package keycloak

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-substrate/api/keycloak/v1alpha1"
)

// makeNamespace creates a namespace with the given name, tolerating an
// already-exists from a prior test.
func makeNamespace(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := shared.k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %q: %v", name, err)
	}
}

// createIgnoreExists creates obj, tolerating an already-exists error from a prior
// test run within the shared envtest control plane.
func createIgnoreExists(t *testing.T, ctx context.Context, obj client.Object) {
	t.Helper()
	if err := shared.k8sClient.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
	}
}

func conditionStatus(conds []metav1.Condition, condType string) (metav1.ConditionStatus, string, bool) {
	c := meta.FindStatusCondition(conds, condType)
	if c == nil {
		return "", "", false
	}
	return c.Status, c.Reason, true
}

// containsToken reports whether s contains the substring token — used to match a
// recorded event string against an expected reason.
func containsToken(s, token string) bool { return strings.Contains(s, token) }

func TestInstanceReconcile(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	const ns = "kc-instance-test"
	makeNamespace(t, ctx, ns)

	t.Run("ready when realm reachable and threads caBundle", func(t *testing.T) {
		caBundle := validTestCABundle(t)
		createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
		inst := &keycloakv1alpha1.KeycloakInstance{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ready"},
			Spec: keycloakv1alpha1.KeycloakInstanceSpec{
				URL:      "https://keycloak.example.test",
				Realm:    "holos",
				CABundle: caBundle,
			},
		}
		if err := shared.k8sClient.Create(ctx, inst); err != nil {
			t.Fatalf("creating instance: %v", err)
		}
		fake := newFakeKeycloakClient()
		r, recorder := newInstanceReconciler(fake, ns)
		if _, err := reconcileInstance(ctx, r, client.ObjectKeyFromObject(inst)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := &keycloakv1alpha1.KeycloakInstance{}
		if err := shared.k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), got); err != nil {
			t.Fatalf("get: %v", err)
		}
		status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
		if !ok || status != metav1.ConditionTrue || reason != ReasonReconciled {
			t.Errorf("Ready = (%v, %v, %v), want (True, %s)", status, reason, ok, ReasonReconciled)
		}
		if got.Status.ObservedGeneration != got.Generation {
			t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
		}
		if string(fake.gotCABundle) != string(caBundle) {
			t.Errorf("caBundle was not threaded to the client factory")
		}
		if !fake.callsContain("GetRealm") {
			t.Errorf("expected a GetRealm reachability probe, calls = %v", fake.calls)
		}
		if got.Status.LastValidatedTime == nil {
			t.Errorf("lastValidatedTime not set on successful validation")
		}
		assertEvent(t, recorder, ReasonReconciled)
	})

	t.Run("credentials not found requeues with condition", func(t *testing.T) {
		const cns = "kc-instance-nocreds"
		makeNamespace(t, ctx, cns)
		inst := &keycloakv1alpha1.KeycloakInstance{
			ObjectMeta: metav1.ObjectMeta{Namespace: cns, Name: "nocreds"},
			Spec: keycloakv1alpha1.KeycloakInstanceSpec{
				URL:   "https://keycloak.example.test",
				Realm: "holos",
			},
		}
		if err := shared.k8sClient.Create(ctx, inst); err != nil {
			t.Fatalf("creating instance: %v", err)
		}
		fake := newFakeKeycloakClient()
		r, _ := newInstanceReconciler(fake, cns) // empty namespace has no credential Secret
		if _, err := reconcileInstance(ctx, r, client.ObjectKeyFromObject(inst)); err == nil {
			t.Fatalf("expected a requeue error for missing credential")
		}
		got := &keycloakv1alpha1.KeycloakInstance{}
		if err := shared.k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), got); err != nil {
			t.Fatalf("get: %v", err)
		}
		status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
		if !ok || status != metav1.ConditionFalse || reason != ReasonCredentialsNotFound {
			t.Errorf("Ready = (%v, %v, %v), want (False, %s)", status, reason, ok, ReasonCredentialsNotFound)
		}
		if fake.callsContain("GetRealm") {
			t.Errorf("must not probe Keycloak before the credential resolves")
		}
		if got.Status.LastValidatedTime != nil {
			t.Errorf("lastValidatedTime advanced on failed credential resolution: %v", got.Status.LastValidatedTime)
		}
	})

	t.Run("unreachable realm marks not ready", func(t *testing.T) {
		const uns = "kc-instance-unreachable"
		makeNamespace(t, ctx, uns)
		createIgnoreExists(t, ctx, newCredentialSecret(uns, keycloakv1alpha1.DefaultCredentialsSecretName))
		inst := &keycloakv1alpha1.KeycloakInstance{
			ObjectMeta: metav1.ObjectMeta{Namespace: uns, Name: "unreachable"},
			Spec: keycloakv1alpha1.KeycloakInstanceSpec{
				URL:   "https://keycloak.example.test",
				Realm: "holos",
			},
		}
		if err := shared.k8sClient.Create(ctx, inst); err != nil {
			t.Fatalf("creating instance: %v", err)
		}
		fake := newFakeKeycloakClient()
		fake.realmReachable = false
		r, _ := newInstanceReconciler(fake, uns)
		if _, err := reconcileInstance(ctx, r, client.ObjectKeyFromObject(inst)); err == nil {
			t.Fatalf("expected a requeue error for an unreachable realm")
		}
		got := &keycloakv1alpha1.KeycloakInstance{}
		if err := shared.k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), got); err != nil {
			t.Fatalf("get: %v", err)
		}
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonKeycloakError {
			t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonKeycloakError)
		}
		if got.Status.LastValidatedTime != nil {
			t.Errorf("lastValidatedTime advanced on failed realm validation: %v", got.Status.LastValidatedTime)
		}
	})
}

// assertEvent fails the test unless a recorded event contains the given reason
// token.
func assertEvent(t *testing.T, recorder *record.FakeRecorder, reason string) {
	t.Helper()
	for {
		select {
		case e := <-recorder.Events:
			if containsToken(e, reason) {
				return
			}
		default:
			t.Errorf("expected an event mentioning %q", reason)
			return
		}
	}
}

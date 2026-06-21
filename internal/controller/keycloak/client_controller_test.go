package keycloak

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
)

func getKClient(t *testing.T, ctx context.Context, key client.ObjectKey) *keycloakv1alpha1.KeycloakClient {
	t.Helper()
	got := &keycloakv1alpha1.KeycloakClient{}
	if err := shared.k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get client: %v", err)
	}
	return got
}

// reconcileClientToSteady runs the finalizer pass then the provision pass.
func reconcileClientToSteady(t *testing.T, ctx context.Context, r *ClientReconciler, key client.ObjectKey) {
	t.Helper()
	if _, err := reconcileClient(ctx, r, key); err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	if _, err := reconcileClient(ctx, r, key); err != nil {
		t.Fatalf("reconcile (provision): %v", err)
	}
}

func TestClientReconcileCreatePublicWithRolesAndMapper(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	const ns = "kc-client-create"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-project"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:     "https://my-project.holos.internal",
			Type:         keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef:  keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			RedirectURIs: []string{"https://my-project.holos.internal/callback"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{
				{ClientRef: "my-project", Role: "my-project-owner"},
				{ClientRef: "my-project", Role: "my-project-editor"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, recorder := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	got := getKClient(t, ctx, key)
	status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
	if !ok || status != metav1.ConditionTrue || reason != ReasonCreated {
		t.Errorf("Ready = (%v, %v, %v), want (True, %s)", status, reason, ok, ReasonCreated)
	}
	if !fake.clientExists("https://my-project.holos.internal") {
		t.Errorf("client was not created in Keycloak")
	}
	uuid := fake.clients["https://my-project.holos.internal"]
	if !fake.clientRoleCreated(uuid, "my-project-owner") || !fake.clientRoleCreated(uuid, "my-project-editor") {
		t.Errorf("declared client roles were not created; calls = %v", fake.calls)
	}
	if !fake.mapperEnsured(uuid) {
		t.Errorf("client-role mapper was not ensured; calls = %v", fake.calls)
	}
	assertEvent(t, recorder, ReasonCreated)
}

func TestClientReconcileAdoptExisting(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-adopt"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "existing"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://existing.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	fake.seedClient("https://existing.holos.internal", "existing-uuid") // pre-existing
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	got := getKClient(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionTrue || reason != ReasonAdopted {
		t.Errorf("Ready = (%v, %v), want (True, %s)", status, reason, ReasonAdopted)
	}
	if got.Status.Created || !got.Status.Adopted {
		t.Errorf("adopted client must be Adopted, not Created: Created=%v Adopted=%v", got.Status.Created, got.Status.Adopted)
	}
	if !fake.callsContain("UpdateClient:existing-uuid") {
		t.Errorf("adopted client was not converged via UpdateClientFields; calls = %v", fake.calls)
	}
}

func TestClientReconcileConflictWithoutAdopt(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-conflict"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "foreign"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://foreign.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			// Adopt defaults false.
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	fake.seedClient("https://foreign.holos.internal", "foreign-uuid") // pre-existing, foreign
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	got := getKClient(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonConflict {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonConflict)
	}
	if got.Status.Created || got.Status.Adopted {
		t.Errorf("conflict must not claim the client: Created=%v Adopted=%v", got.Status.Created, got.Status.Adopted)
	}
	if fake.callsContain("UpdateClient:foreign-uuid") {
		t.Errorf("a conflicting, unadopted client must not be reconfigured")
	}
}

func TestClientReconcilePublicSetsPKCE(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-pkce"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pkce"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://pkce.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	if got := fake.createdClientPKCE("https://pkce.holos.internal"); got != "S256" {
		t.Errorf("public client PKCE attribute = %q, want S256", got)
	}
}

func TestClientReconcileConfidentialSecretDelivery(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-secret"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "confidential"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://confidential.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypeConfidential,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			SecretRef:   &keycloakv1alpha1.ClientSecretReference{Name: "confidential-oidc", Key: "clientSecret"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	delivered := &corev1.Secret{}
	skey := types.NamespacedName{Namespace: ns, Name: "confidential-oidc"}
	if err := shared.k8sClient.Get(ctx, skey, delivered); err != nil {
		t.Fatalf("delivered secret not found: %v", err)
	}
	uuid := fake.clients["https://confidential.holos.internal"]
	want := "generated-secret-" + uuid
	if got := string(delivered.Data["clientSecret"]); got != want {
		t.Errorf("delivered clientSecret = %q, want %q", got, want)
	}
	if len(delivered.OwnerReferences) == 0 {
		t.Errorf("delivered secret missing owner reference for GC")
	}

	// Generate-once: a second reconcile must not overwrite an externally-rotated
	// value. Mutate the Secret, then verify the next reconcile leaves it.
	delivered.Data["clientSecret"] = []byte("rotated-out-of-band")
	if err := shared.k8sClient.Update(ctx, delivered); err != nil {
		t.Fatalf("update delivered secret: %v", err)
	}
	if _, err := reconcileClient(ctx, r, key); err != nil {
		t.Fatalf("reconcile (second pass): %v", err)
	}
	again := &corev1.Secret{}
	if err := shared.k8sClient.Get(ctx, skey, again); err != nil {
		t.Fatalf("get delivered secret: %v", err)
	}
	if got := string(again.Data["clientSecret"]); got != "rotated-out-of-band" {
		t.Errorf("generate-once violated: clientSecret = %q, want the rotated value preserved", got)
	}
}

func TestClientReconcileConfidentialClearsPKCE(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-pkce-clear"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	// An adopted confidential client that already carries a stale PKCE S256
	// attribute must converge to no-PKCE: the update requests removal of the PKCE
	// attribute (and does not set it), and a public client still sets it.
	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "conf"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://conf.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypeConfidential,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			SecretRef:   &keycloakv1alpha1.ClientSecretReference{Name: "conf-oidc", Key: "clientSecret"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	fake.seedClient("https://conf.holos.internal", "conf-uuid") // pre-existing, with stale S256
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	if !fake.updatePKCECleared("conf-uuid") {
		t.Errorf("confidential client update did not request PKCE attribute removal; fields = %+v", fake.lastUpdateFields["conf-uuid"])
	}
	if fake.updatePKCESet("conf-uuid") != "" {
		t.Errorf("confidential client update must not set the PKCE attribute, got %q", fake.updatePKCESet("conf-uuid"))
	}
}

func TestClientReconcileSecretCollisionMissingKeyFails(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-secret-collide"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	// A foreign Secret already occupies the target name but lacks the requested key.
	createIgnoreExists(t, ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "collide-oidc"},
		Data:       map[string][]byte{"unrelated": []byte("x")},
	})

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "collide"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://collide.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypeConfidential,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			SecretRef:   &keycloakv1alpha1.ClientSecretReference{Name: "collide-oidc", Key: "clientSecret"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	if _, err := reconcileClient(ctx, r, key); err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	// The provision pass must fail (collision), not report success.
	if _, err := reconcileClient(ctx, r, key); err == nil {
		t.Fatalf("expected a reconcile error on secret-name collision, got nil")
	}
	got := getKClient(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonKeycloakError {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonKeycloakError)
	}
	// The foreign Secret must be left untouched.
	foreign := &corev1.Secret{}
	if err := shared.k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "collide-oidc"}, foreign); err != nil {
		t.Fatalf("get foreign secret: %v", err)
	}
	if _, ok := foreign.Data["clientSecret"]; ok {
		t.Errorf("foreign Secret must not be overwritten with the client secret")
	}
}

func TestClientReconcileReservedNameRejected(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-reserved"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	for _, reserved := range []string{"argocd", "kargo", "https://quay.holos.internal"} {
		kclient := &keycloakv1alpha1.KeycloakClient{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "reserved-" + sanitize(reserved)},
			Spec: keycloakv1alpha1.KeycloakClientSpec{
				ClientID:    reserved,
				Type:        keycloakv1alpha1.KeycloakClientTypePublic,
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, kclient); err != nil {
			t.Fatalf("create client %q: %v", reserved, err)
		}
		fake := newFakeKeycloakClient()
		r, _ := newClientReconciler(fake, ns)
		key := client.ObjectKeyFromObject(kclient)
		reconcileClientToSteady(t, ctx, r, key)

		got := getKClient(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonReserved {
			t.Errorf("clientId %q: Ready = (%v, %v), want (False, %s)", reserved, status, reason, ReasonReserved)
		}
		if fake.clientExists(reserved) {
			t.Errorf("reserved clientId %q must not be created in Keycloak", reserved)
		}
	}
}

// sanitize turns a clientId into a valid k8s object name fragment for the test.
func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		}
	}
	return string(out)
}

func TestClientReconcileReferenceGrant(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const clientNS = "kc-client-xns-from"
	const instNS = "kc-client-xns-to"
	makeNamespace(t, ctx, clientNS)
	makeNamespace(t, ctx, instNS)
	createIgnoreExists(t, ctx, newCredentialSecret(clientNS, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, instNS, "kc")

	newClient := func(name string) *keycloakv1alpha1.KeycloakClient {
		c := &keycloakv1alpha1.KeycloakClient{
			ObjectMeta: metav1.ObjectMeta{Namespace: clientNS, Name: name},
			Spec: keycloakv1alpha1.KeycloakClientSpec{
				ClientID:    "https://" + name + ".holos.internal",
				Type:        keycloakv1alpha1.KeycloakClientTypePublic,
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc", Namespace: instNS},
			},
		}
		if err := shared.k8sClient.Create(ctx, c); err != nil {
			t.Fatalf("create client: %v", err)
		}
		return c
	}

	t.Run("denied without a grant", func(t *testing.T) {
		kclient := newClient("denied")
		fake := newFakeKeycloakClient()
		r, _ := newClientReconciler(fake, clientNS)
		key := client.ObjectKeyFromObject(kclient)
		_, _ = reconcileClient(ctx, r, key) // finalizer
		if _, err := reconcileClient(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getKClient(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonReferenceNotGranted {
			t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonReferenceNotGranted)
		}
		if fake.clientExists("https://denied.holos.internal") {
			t.Errorf("denied reference must not reach Keycloak")
		}
	})

	t.Run("allowed with a grant", func(t *testing.T) {
		grant := &securityv1alpha1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Namespace: instNS, Name: "allow-keycloakclient"},
			Spec: securityv1alpha1.ReferenceGrantSpec{
				From: []securityv1alpha1.ReferenceGrantFrom{{
					Group:     keycloakv1alpha1.GroupVersion.Group,
					Kind:      "KeycloakClient",
					Namespace: clientNS,
				}},
				To: []securityv1alpha1.ReferenceGrantTo{{
					Group: keycloakv1alpha1.GroupVersion.Group,
					Kind:  "KeycloakInstance",
				}},
			},
		}
		createIgnoreExists(t, ctx, grant)

		kclient := newClient("allowed")
		fake := newFakeKeycloakClient()
		r, _ := newClientReconciler(fake, clientNS)
		key := client.ObjectKeyFromObject(kclient)
		reconcileClientToSteady(t, ctx, r, key)
		got := getKClient(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionTrue || reason != ReasonCreated {
			t.Errorf("Ready = (%v, %v), want (True, %s)", status, reason, ReasonCreated)
		}
		if !fake.clientExists("https://allowed.holos.internal") {
			t.Errorf("granted reference should have provisioned the client")
		}
	})
}

func TestClientDelete(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-delete"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://del.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient()
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)
	if !fake.clientExists("https://del.holos.internal") {
		t.Fatalf("client not provisioned before delete")
	}

	if err := shared.k8sClient.Delete(ctx, getKClient(t, ctx, key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := reconcileClient(ctx, r, key); err != nil {
		t.Fatalf("reconcile (delete): %v", err)
	}
	if fake.clientExists("https://del.holos.internal") {
		t.Errorf("created client was not deleted in Keycloak on finalize")
	}
	// The CR's finalizer should be gone, so it is fully removed.
	if err := shared.k8sClient.Get(ctx, key, &keycloakv1alpha1.KeycloakClient{}); err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("client CR still present after finalize: err = %v", err)
	}
}

package keycloak

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-substrate/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-substrate/api/security/v1alpha1"
)

func getKClient(t *testing.T, ctx context.Context, key client.ObjectKey) *keycloakv1alpha1.Client {
	t.Helper()
	got := &keycloakv1alpha1.Client{}
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-project"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:     "https://my-project.holos.internal",
			Type:         keycloakv1alpha1.ClientTypePublic,
			InstanceRef:  keycloakv1alpha1.InstanceReference{Name: "kc"},
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

	fake := newFakeClient()
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
	if got.Status.LastValidatedTime == nil {
		t.Errorf("lastValidatedTime not set on successful reconcile")
	}
	if got.Status.LastMutatedTime == nil || got.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want time with %q", got.Status.LastMutatedTime, got.Status.LastMutationReason, keycloakv1alpha1.MutationReasonSpecChange)
	}
	assertEvent(t, recorder, ReasonCreated)
}

func TestClientSteadyStateRefreshesValidationOnly(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-observe"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "observe"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://observe.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)
	first := getKClient(t, ctx, key)
	if first.Status.LastValidatedTime == nil || first.Status.LastMutatedTime == nil {
		t.Fatalf("initial timestamps not set: validated=%v mutated=%v", first.Status.LastValidatedTime, first.Status.LastMutatedTime)
	}
	firstValidated := first.Status.LastValidatedTime.DeepCopy()
	firstMutated := first.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	result, err := reconcileClient(ctx, r, key)
	if err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if result.RequeueAfter != keycloakExternalResourceResync {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, keycloakExternalResourceResync)
	}
	second := getKClient(t, ctx, key)
	if !second.Status.LastValidatedTime.After(firstValidated.Time) {
		t.Errorf("lastValidatedTime did not advance: first=%v second=%v", firstValidated, second.Status.LastValidatedTime)
	}
	if !second.Status.LastMutatedTime.Equal(firstMutated) {
		t.Errorf("lastMutatedTime changed on steady validation: first=%v second=%v", firstMutated, second.Status.LastMutatedTime)
	}
}

func TestClientReconcileDescriptionPropagatedOnCreate(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	const ns = "kc-client-desc-create"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	const want = "the my-project OIDC client"
	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "desc-create"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://desc-create.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			Description: want,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	uuid := fake.clients["https://desc-create.holos.internal"]
	if uuid == "" {
		t.Fatalf("client was not created in Keycloak; calls = %v", fake.calls)
	}
	if got := fake.clientDescription(uuid); got != want {
		t.Errorf("description = %q, want %q", got, want)
	}
}

func TestClientReconcileDescriptionDriftCorrected(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-desc-drift"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	const want = "spec-owned description"
	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "desc-drift"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://desc-drift.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			Description: want,
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
	// Pre-existing client whose description was changed in the console (drift).
	fake.seedClient("https://desc-drift.holos.internal", "drift-uuid")
	fake.seedClientDescription("drift-uuid", "console-set drift")
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	if got := fake.clientDescription("drift-uuid"); got != want {
		t.Errorf("description after reconcile = %q, want spec value %q (drift not corrected)", got, want)
	}
}

func TestClientReconcileDescriptionClearedWhenOmitted(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-desc-clear"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "desc-clear"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://desc-clear.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			// Description omitted.
			Adopt: true,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
	// Pre-existing client carrying a console-set description that the empty spec
	// value must actively clear.
	fake.seedClient("https://desc-clear.holos.internal", "clear-uuid")
	fake.seedClientDescription("clear-uuid", "stale console description")
	r, _ := newClientReconciler(fake, ns)
	key := client.ObjectKeyFromObject(kclient)
	reconcileClientToSteady(t, ctx, r, key)

	if got := fake.clientDescription("clear-uuid"); got != "" {
		t.Errorf("description = %q, want empty (omitted spec must clear it)", got)
	}
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "existing"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://existing.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "foreign"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://foreign.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			// Adopt defaults false.
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pkce"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://pkce.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "confidential"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://confidential.holos.internal",
			Type:        keycloakv1alpha1.ClientTypeConfidential,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			SecretRef:   &keycloakv1alpha1.ClientSecretReference{Name: "confidential-oidc", Key: "clientSecret"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
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
	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "conf"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://conf.holos.internal",
			Type:        keycloakv1alpha1.ClientTypeConfidential,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			SecretRef:   &keycloakv1alpha1.ClientSecretReference{Name: "conf-oidc", Key: "clientSecret"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "collide"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://collide.holos.internal",
			Type:        keycloakv1alpha1.ClientTypeConfidential,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			SecretRef:   &keycloakv1alpha1.ClientSecretReference{Name: "collide-oidc", Key: "clientSecret"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create client: %v", err)
	}

	fake := newFakeClient()
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

// TestClientReconcilePreviouslyReservedIDsNowReconcile asserts the transparent
// contract (HOL-1421): clientIds the controller used to refuse on org-policy
// grounds (the platform argocd/kargo/quay clients and Keycloak built-ins like
// realm-management) are now reconciled verbatim — the client is created in Keycloak
// at exactly the declared clientId and the resource reports Ready/Created, with no
// reserved-name rejection.
func TestClientReconcilePreviouslyReservedIDsNowReconcile(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-formerly-reserved"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	for _, formerlyReserved := range []string{"argocd", "kargo", "https://quay.holos.internal", "realm-management"} {
		kclient := &keycloakv1alpha1.Client{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "formerly-" + sanitize(formerlyReserved)},
			Spec: keycloakv1alpha1.ClientSpec{
				ClientID:    formerlyReserved,
				Type:        keycloakv1alpha1.ClientTypePublic,
				InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, kclient); err != nil {
			t.Fatalf("create client %q: %v", formerlyReserved, err)
		}
		fake := newFakeClient()
		r, _ := newClientReconciler(fake, ns)
		key := client.ObjectKeyFromObject(kclient)
		reconcileClientToSteady(t, ctx, r, key)

		got := getKClient(t, ctx, key)
		status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
		if !ok || status != metav1.ConditionTrue || reason != ReasonCreated {
			t.Errorf("clientId %q: Ready = (%v, %v, %v), want (True, %s)", formerlyReserved, status, reason, ok, ReasonCreated)
		}
		if !fake.clientExists(formerlyReserved) {
			t.Errorf("formerly-reserved clientId %q must now be created verbatim in Keycloak; calls = %v", formerlyReserved, fake.calls)
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

	newClient := func(name string) *keycloakv1alpha1.Client {
		c := &keycloakv1alpha1.Client{
			ObjectMeta: metav1.ObjectMeta{Namespace: clientNS, Name: name},
			Spec: keycloakv1alpha1.ClientSpec{
				ClientID:    "https://" + name + ".holos.internal",
				Type:        keycloakv1alpha1.ClientTypePublic,
				InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc", Namespace: instNS},
			},
		}
		if err := shared.k8sClient.Create(ctx, c); err != nil {
			t.Fatalf("create client: %v", err)
		}
		return c
	}

	t.Run("denied without a grant", func(t *testing.T) {
		kclient := newClient("denied")
		fake := newFakeClient()
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
					Kind:      "Client",
					Namespace: clientNS,
				}},
				To: []securityv1alpha1.ReferenceGrantTo{{
					Group: keycloakv1alpha1.GroupVersion.Group,
					Kind:  "Instance",
				}},
			},
		}
		createIgnoreExists(t, ctx, grant)

		kclient := newClient("allowed")
		fake := newFakeClient()
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

func TestClientRenameTransferFlow(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-transfer"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	old := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "old-client"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:     "https://transfer.holos.internal",
			Type:         keycloakv1alpha1.ClientTypePublic,
			InstanceRef:  keycloakv1alpha1.InstanceReference{Name: "kc"},
			RedirectURIs: []string{"https://transfer.holos.internal/callback"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{{
				ClientRef: "old-client",
				Role:      "transfer-owner",
			}},
		},
	}
	if err := shared.k8sClient.Create(ctx, old); err != nil {
		t.Fatalf("create old client: %v", err)
	}
	oldKey := client.ObjectKeyFromObject(old)

	fake := newFakeClient()
	r, _ := newClientReconciler(fake, ns)
	reconcileClientToSteady(t, ctx, r, oldKey)
	old = getKClient(t, ctx, oldKey)
	if !old.Status.Created || old.Status.ClientUUID == "" {
		t.Fatalf("old status = %+v, want created with ClientUUID", old.Status)
	}
	oldID := old.Status.ClientUUID
	if !fake.clientRoleCreated(oldID, "transfer-owner") || !fake.mapperEnsured(oldID) {
		t.Fatalf("old client roles/mapper were not converged; calls = %v", fake.calls)
	}

	old.Spec.DeletionPolicy = keycloakv1alpha1.DeletionPolicyOrphan
	if err := shared.k8sClient.Update(ctx, old); err != nil {
		t.Fatalf("setting old deletionPolicy: %v", err)
	}
	fake.resetCalls()
	if err := shared.k8sClient.Delete(ctx, old); err != nil {
		t.Fatalf("delete old client: %v", err)
	}
	if _, err := reconcileClient(ctx, r, oldKey); err != nil {
		t.Fatalf("reconcile old orphan: %v", err)
	}
	if gotCalls := fake.callCount(); gotCalls != 0 {
		t.Fatalf("orphan transfer should not call Keycloak; calls were %v", fake.calls)
	}
	if !fake.clientExists("https://transfer.holos.internal") {
		t.Fatal("orphaned client should remain in Keycloak")
	}

	replacement := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "new-client"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:     "https://transfer.holos.internal",
			Type:         keycloakv1alpha1.ClientTypePublic,
			InstanceRef:  keycloakv1alpha1.InstanceReference{Name: "kc"},
			RedirectURIs: []string{"https://transfer.holos.internal/callback"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{{
				ClientRef: "new-client",
				Role:      "transfer-owner",
			}},
			Adopt: true,
		},
	}
	if err := shared.k8sClient.Create(ctx, replacement); err != nil {
		t.Fatalf("create replacement client: %v", err)
	}
	newKey := client.ObjectKeyFromObject(replacement)
	reconcileClientToSteady(t, ctx, r, newKey)
	replacement = getKClient(t, ctx, newKey)
	status, reason, _ := conditionStatus(replacement.Status.Conditions, ConditionReady)
	if status != metav1.ConditionTrue || reason != ReasonAdopted {
		t.Fatalf("replacement Ready = (%v, %v), want (True, %s)", status, reason, ReasonAdopted)
	}
	if replacement.Status.Created || !replacement.Status.Adopted {
		t.Fatalf("replacement Created=%v Adopted=%v, want adopted provenance", replacement.Status.Created, replacement.Status.Adopted)
	}
	if replacement.Status.ClientUUID != oldID {
		t.Fatalf("replacement ClientUUID = %q, want transferred ID %q", replacement.Status.ClientUUID, oldID)
	}
	if !fake.clientRoleCreated(oldID, "transfer-owner") || !fake.mapperEnsured(oldID) {
		t.Fatalf("replacement should converge roles/mapper on transferred client; calls = %v", fake.calls)
	}

	replacement.Spec.DeletionPolicy = keycloakv1alpha1.DeletionPolicyDelete
	if err := shared.k8sClient.Update(ctx, replacement); err != nil {
		t.Fatalf("setting replacement deletionPolicy: %v", err)
	}
	fake.resetCalls()
	if err := shared.k8sClient.Delete(ctx, replacement); err != nil {
		t.Fatalf("delete replacement client: %v", err)
	}
	if _, err := reconcileClient(ctx, r, newKey); err != nil {
		t.Fatalf("reconcile replacement delete: %v", err)
	}
	if fake.clientExists("https://transfer.holos.internal") {
		t.Fatal("transferred client should be deleted after explicit Delete")
	}
	if !fake.callsContain("DeleteClient:" + oldID) {
		t.Fatalf("explicit Delete should delete transferred client by pinned UUID; calls were %v", fake.calls)
	}
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

	kclient := &keycloakv1alpha1.Client{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del"},
		Spec: keycloakv1alpha1.ClientSpec{
			ClientID:    "https://del.holos.internal",
			Type:        keycloakv1alpha1.ClientTypePublic,
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, kclient); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeClient()
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
	if err := shared.k8sClient.Get(ctx, key, &keycloakv1alpha1.Client{}); err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("client CR still present after finalize: err = %v", err)
	}
}

func TestClientDeletionPolicy(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-client-delpolicy"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	t.Run("orphan leaves created client untouched without Keycloak calls", func(t *testing.T) {
		kclient := &keycloakv1alpha1.Client{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "orphan"},
			Spec: keycloakv1alpha1.ClientSpec{
				ClientID:       "https://orphan-client.holos.internal",
				Type:           keycloakv1alpha1.ClientTypePublic,
				InstanceRef:    keycloakv1alpha1.InstanceReference{Name: "kc"},
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyOrphan,
			},
		}
		if err := shared.k8sClient.Create(ctx, kclient); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
		r, _ := newClientReconciler(fake, ns)
		key := client.ObjectKeyFromObject(kclient)
		reconcileClientToSteady(t, ctx, r, key)
		if !fake.clientExists("https://orphan-client.holos.internal") {
			t.Fatalf("precondition: client should exist")
		}
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getKClient(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileClient(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if gotCalls := fake.callCount(); gotCalls != 0 {
			t.Errorf("deletionPolicy Orphan made Keycloak calls: %v", fake.calls)
		}
		if !fake.clientExists("https://orphan-client.holos.internal") {
			t.Errorf("orphaned client should remain in Keycloak")
		}
	})

	t.Run("adopted client with explicit delete is deleted after UUID verification", func(t *testing.T) {
		kclient := &keycloakv1alpha1.Client{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adopted-delete"},
			Spec: keycloakv1alpha1.ClientSpec{
				ClientID:       "https://adopted-delete.holos.internal",
				Type:           keycloakv1alpha1.ClientTypePublic,
				InstanceRef:    keycloakv1alpha1.InstanceReference{Name: "kc"},
				Adopt:          true,
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyDelete,
			},
		}
		if err := shared.k8sClient.Create(ctx, kclient); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
		fake.seedClient("https://adopted-delete.holos.internal", "adopted-delete-uuid")
		r, _ := newClientReconciler(fake, ns)
		key := client.ObjectKeyFromObject(kclient)
		reconcileClientToSteady(t, ctx, r, key)
		if !getKClient(t, ctx, key).Status.Adopted {
			t.Fatalf("expected adopted client")
		}
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getKClient(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileClient(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if fake.clientExists("https://adopted-delete.holos.internal") {
			t.Errorf("adopted client with deletionPolicy Delete should be deleted")
		}
		if !fake.callsContain("DeleteClient:adopted-delete-uuid") {
			t.Errorf("expected delete by verified client UUID; calls = %v", fake.calls)
		}
	})

	t.Run("adopted client explicit delete releases out-of-band replacement", func(t *testing.T) {
		kclient := &keycloakv1alpha1.Client{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adopted-replaced"},
			Spec: keycloakv1alpha1.ClientSpec{
				ClientID:       "https://adopted-replaced.holos.internal",
				Type:           keycloakv1alpha1.ClientTypePublic,
				InstanceRef:    keycloakv1alpha1.InstanceReference{Name: "kc"},
				Adopt:          true,
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyDelete,
			},
		}
		if err := shared.k8sClient.Create(ctx, kclient); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
		fake.seedClient("https://adopted-replaced.holos.internal", "claimed-client-uuid")
		r, _ := newClientReconciler(fake, ns)
		key := client.ObjectKeyFromObject(kclient)
		reconcileClientToSteady(t, ctx, r, key)
		fake.seedClient("https://adopted-replaced.holos.internal", "replacement-client-uuid")
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getKClient(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileClient(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if !fake.clientExists("https://adopted-replaced.holos.internal") {
			t.Errorf("foreign replacement client must not be deleted")
		}
		if fake.callsContain("DeleteClient:replacement-client-uuid") {
			t.Errorf("replacement client must not be deleted; calls = %v", fake.calls)
		}
	})
}

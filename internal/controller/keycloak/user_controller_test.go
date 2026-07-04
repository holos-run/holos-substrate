package keycloak

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
)

type legacyManagedGroupsReader struct {
	users []unstructured.Unstructured
	err   error
}

func (r legacyManagedGroupsReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return errors.New("unexpected Get")
}

func (r legacyManagedGroupsReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.err != nil {
		return r.err
	}
	users, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return errors.New("unexpected list type")
	}
	users.Items = append(users.Items, r.users...)
	return nil
}

func getUser(t *testing.T, ctx context.Context, key client.ObjectKey) *keycloakv1alpha1.KeycloakUser {
	t.Helper()
	got := &keycloakv1alpha1.KeycloakUser{}
	if err := shared.k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get user: %v", err)
	}
	return got
}

// reconcileUserToSteady runs the finalizer pass then the provision pass.
func reconcileUserToSteady(t *testing.T, ctx context.Context, r *UserReconciler, key client.ObjectKey) {
	t.Helper()
	if _, err := reconcileUser(ctx, r, key); err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	if _, err := reconcileUser(ctx, r, key); err != nil {
		t.Fatalf("reconcile (provision): %v", err)
	}
}

func TestCheckNoLegacyUserManagedGroups(t *testing.T) {
	ctx := context.Background()
	newRawUser := func(namespace, name string, managedGroups any) unstructured.Unstructured {
		u := unstructured.Unstructured{Object: map[string]any{
			"apiVersion": keycloakv1alpha1.GroupVersion.String(),
			"kind":       "KeycloakUser",
			"metadata": map[string]any{
				"namespace": namespace,
				"name":      name,
			},
			"status": map[string]any{},
		}}
		if managedGroups != nil {
			u.Object["status"].(map[string]any)["managedGroups"] = managedGroups
		}
		return u
	}

	if err := checkNoLegacyUserManagedGroups(ctx, legacyManagedGroupsReader{users: []unstructured.Unstructured{
		newRawUser("project-a", "alice", nil),
		newRawUser("project-a", "bob", []any{}),
	}}); err != nil {
		t.Fatalf("clean legacy status rejected: %v", err)
	}

	err := checkNoLegacyUserManagedGroups(ctx, legacyManagedGroupsReader{users: []unstructured.Unstructured{
		newRawUser("project-a", "alice", []any{"projects/p/roles/owner|grp-1"}),
		newRawUser("project-b", "bob", []any{"projects/q/roles/owner|grp-2"}),
	}})
	if err == nil {
		t.Fatalf("legacy managed groups accepted, want startup block")
	}
	for _, want := range []string{"project-a/alice", "project-b/bob", "legacy status.managedGroups"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}

	err = checkNoLegacyUserManagedGroups(ctx, legacyManagedGroupsReader{users: []unstructured.Unstructured{
		newRawUser("project-a", "bad", []any{7}),
	}})
	if err == nil || !strings.Contains(err.Error(), "project-a/bad") {
		t.Fatalf("malformed legacy status error = %v, want object-specific error", err)
	}
}

func TestUserReconcileCreateAndLink(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	const ns = "kc-user-create"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bob"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:       "bob@example.com",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{
				Alias:  "corp-oidc",
				UserID: "upstream-sub-123",
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, recorder := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)

	got := getUser(t, ctx, key)
	status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
	if !ok || status != metav1.ConditionTrue || reason != ReasonCreated {
		t.Errorf("Ready = (%v, %v, %v), want (True, %s)", status, reason, ok, ReasonCreated)
	}
	if !got.Status.Created {
		t.Errorf("status.Created = false, want true")
	}
	if got.Status.UserID == "" {
		t.Errorf("status.UserID not recorded")
	}
	if !fake.userExists("bob@example.com") {
		t.Errorf("user was not created in Keycloak")
	}
	if !fake.federated(got.Status.UserID, "corp-oidc") {
		t.Errorf("IdP federated-identity link was not created; calls = %v", fake.calls)
	}
	if got.Status.LastValidatedTime == nil {
		t.Errorf("lastValidatedTime not set on successful reconcile")
	}
	if got.Status.LastMutatedTime == nil || got.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want time with %q", got.Status.LastMutatedTime, got.Status.LastMutationReason, keycloakv1alpha1.MutationReasonSpecChange)
	}
	firstValidated := got.Status.LastValidatedTime.DeepCopy()
	firstMutated := got.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	result, err := reconcileUser(ctx, r, key)
	if err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if result.RequeueAfter != keycloakExternalResourceResync {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, keycloakExternalResourceResync)
	}
	got = getUser(t, ctx, key)
	if !got.Status.LastValidatedTime.After(firstValidated.Time) {
		t.Errorf("lastValidatedTime did not advance: first=%v second=%v", firstValidated, got.Status.LastValidatedTime)
	}
	if !got.Status.LastMutatedTime.Equal(firstMutated) {
		t.Errorf("lastMutatedTime changed on steady validation: first=%v second=%v", firstMutated, got.Status.LastMutatedTime)
	}
	assertEvent(t, recorder, ReasonCreated)
}

func TestUserReconcileEmailOnlyLinkSkipsAdminAPI(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-emaillink"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "emaillink"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:       "emaillink@example.com",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			// userId omitted: email-only auto-link, left to the realm flow.
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)

	got := getUser(t, ctx, key)
	if fake.federated(got.Status.UserID, "corp-oidc") {
		t.Errorf("email-only link must not pre-create an Admin-API federated identity; calls = %v", fake.calls)
	}
	if got.Status.ManagedIdentityProvider != "" {
		t.Errorf("email-only link must not record a managed IdP provider, got %q", got.Status.ManagedIdentityProvider)
	}
	status, _, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionTrue {
		t.Errorf("Ready = %v, want True", status)
	}
}

func TestUserReconcileReusePresentNoDuplicate(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-reuse"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "alice"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:       "alice@example.com",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient()
	fake.seedUser("alice@example.com") // pre-existing user
	r, _ := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)

	got := getUser(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionTrue || reason != ReasonAdopted {
		t.Errorf("Ready = (%v, %v), want (True, %s)", status, reason, ReasonAdopted)
	}
	if got.Status.Created {
		t.Errorf("status.Created = true, want false (adopted, not created)")
	}
	if !got.Status.Adopted {
		t.Errorf("status.Adopted = false, want true")
	}
	if fake.createUserCount != 0 {
		t.Errorf("CreateUser called %d times for a present user; want 0 (no duplicate)", fake.createUserCount)
	}
}

func TestUserReconcileConflictWithoutAdopt(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-conflict"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "carol"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:       "carol@example.com",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			// Adopt defaults false.
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient()
	fake.seedUser("carol@example.com") // pre-existing, foreign user
	r, _ := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)

	got := getUser(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonConflict {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonConflict)
	}
	if got.Status.Created || got.Status.Adopted {
		t.Errorf("conflict must not claim the user: Created=%v Adopted=%v", got.Status.Created, got.Status.Adopted)
	}
	if fake.createUserCount != 0 {
		t.Errorf("CreateUser called for an unadopted conflicting user; want 0")
	}
}

func TestUserReconcileIdentityProviderLinkRemovalPrunes(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-idp-prune"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "eve"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:                "eve@example.com",
			InstanceRef:          keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-eve"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)

	got := getUser(t, ctx, key)
	if !fake.federated(got.Status.UserID, "corp-oidc") {
		t.Fatalf("IdP link not created initially")
	}
	if got.Status.ManagedIdentityProvider == "" {
		t.Fatalf("managed IdP provider not recorded, got %q", got.Status.ManagedIdentityProvider)
	}

	// Remove the IdP link from the spec; the next reconcile must delete the stale
	// federated identity and clear the managed status.
	got.Spec.IdentityProviderLink = nil
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update user spec: %v", err)
	}
	if _, err := reconcileUser(ctx, r, key); err != nil {
		t.Fatalf("reconcile (prune link): %v", err)
	}
	if fake.federated(got.Status.UserID, "corp-oidc") {
		t.Errorf("stale IdP link was not pruned after removal from spec")
	}
	after := getUser(t, ctx, key)
	if after.Status.ManagedIdentityProvider != "" {
		t.Errorf("managed IdP provider not cleared, got %q", after.Status.ManagedIdentityProvider)
	}
}

func TestUserReconcileIdentityProviderLinkSubjectVerifiedPrune(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-idp-subject"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "grace"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:                "grace@example.com",
			InstanceRef:          keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-grace"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)
	got := getUser(t, ctx, key)
	userID := got.Status.UserID
	if !fake.federated(userID, "corp-oidc") {
		t.Fatalf("IdP link not created")
	}

	// Simulate the link being recreated out of band to a DIFFERENT upstream subject.
	fake.setFederatedSubject(userID, "corp-oidc", "sub-someone-else")

	// Remove the link from spec; the prune must NOT delete the foreign link because
	// its subject no longer matches the one this CR created.
	got.Spec.IdentityProviderLink = nil
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update user spec: %v", err)
	}
	if _, err := reconcileUser(ctx, r, key); err != nil {
		t.Fatalf("reconcile (prune link): %v", err)
	}
	if !fake.federated(userID, "corp-oidc") {
		t.Errorf("subject-verified prune deleted a link recreated out of band to a different subject")
	}
	after := getUser(t, ctx, key)
	if after.Status.ManagedIdentityProvider != "" {
		t.Errorf("managed IdP provider not cleared after switch, got %q", after.Status.ManagedIdentityProvider)
	}
}

func TestUserReconcileReferenceGrant(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const userNS = "kc-user-xns-from"
	const instNS = "kc-user-xns-to"
	makeNamespace(t, ctx, userNS)
	makeNamespace(t, ctx, instNS)
	createIgnoreExists(t, ctx, newCredentialSecret(userNS, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, instNS, "kc")

	newUser := func(name string) *keycloakv1alpha1.KeycloakUser {
		u := &keycloakv1alpha1.KeycloakUser{
			ObjectMeta: metav1.ObjectMeta{Namespace: userNS, Name: name},
			Spec: keycloakv1alpha1.KeycloakUserSpec{
				Email:       name + "@example.com",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc", Namespace: instNS},
			},
		}
		if err := shared.k8sClient.Create(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		return u
	}

	t.Run("denied without a grant", func(t *testing.T) {
		user := newUser("denied")
		fake := newFakeKeycloakClient()
		r, _ := newUserReconciler(fake, userNS)
		key := client.ObjectKeyFromObject(user)
		_, _ = reconcileUser(ctx, r, key) // finalizer
		if _, err := reconcileUser(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getUser(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonReferenceNotGranted {
			t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonReferenceNotGranted)
		}
		if fake.createUserCount != 0 {
			t.Errorf("denied reference must not reach Keycloak")
		}
	})

	t.Run("allowed with a grant", func(t *testing.T) {
		grant := &securityv1alpha1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Namespace: instNS, Name: "allow-keycloakuser"},
			Spec: securityv1alpha1.ReferenceGrantSpec{
				From: []securityv1alpha1.ReferenceGrantFrom{{
					Group:     keycloakv1alpha1.GroupVersion.Group,
					Kind:      "KeycloakUser",
					Namespace: userNS,
				}},
				To: []securityv1alpha1.ReferenceGrantTo{{
					Group: keycloakv1alpha1.GroupVersion.Group,
					Kind:  "KeycloakInstance",
				}},
			},
		}
		createIgnoreExists(t, ctx, grant)

		user := newUser("allowed")
		fake := newFakeKeycloakClient()
		r, _ := newUserReconciler(fake, userNS)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		got := getUser(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionTrue || reason != ReasonCreated {
			t.Errorf("Ready = (%v, %v), want (True, %s)", status, reason, ReasonCreated)
		}
		if !fake.userExists("allowed@example.com") {
			t.Errorf("granted reference should have provisioned the user")
		}
	})
}

func TestUserDelete(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-delete"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	t.Run("created user is deleted in Keycloak", func(t *testing.T) {
		user := &keycloakv1alpha1.KeycloakUser{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-created"},
			Spec: keycloakv1alpha1.KeycloakUserSpec{
				Email:       "del-created@example.com",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient()
		r, _ := newUserReconciler(fake, ns)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		if !fake.userExists("del-created@example.com") {
			t.Fatalf("user not provisioned before delete")
		}

		if err := shared.k8sClient.Delete(ctx, getUser(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileUser(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if fake.userExists("del-created@example.com") {
			t.Errorf("created user was not deleted in Keycloak on finalize")
		}
	})

	t.Run("adopted user is released not deleted", func(t *testing.T) {
		user := &keycloakv1alpha1.KeycloakUser{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted"},
			Spec: keycloakv1alpha1.KeycloakUserSpec{
				Email:                "del-adopted@example.com",
				InstanceRef:          keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				Adopt:                true,
				IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-abc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient()
		userID := fake.seedUser("del-adopted@example.com")
		r, _ := newUserReconciler(fake, ns)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		if !fake.federated(userID, "corp-oidc") {
			t.Fatalf("adopted user not linked to the declared IdP")
		}

		if err := shared.k8sClient.Delete(ctx, getUser(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileUser(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if !fake.userExists("del-adopted@example.com") {
			t.Errorf("adopted user must not be deleted on release")
		}
		if fake.federated(userID, "corp-oidc") {
			t.Errorf("controller-added IdP link was not pruned on release")
		}
	})
}

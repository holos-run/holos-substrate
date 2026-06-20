package keycloak

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
)

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

func TestUserReconcileCreateAndMembershipAndLink(t *testing.T) {
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
			Groups:      []string{"projects/my-project/roles/owner"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{
				Alias:  "corp-oidc",
				UserID: "upstream-sub-123",
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient("projects/my-project/roles/owner")
	groupID := fake.groups[normPath("projects/my-project/roles/owner")]
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
	if !fake.memberOf(got.Status.UserID, groupID) {
		t.Errorf("user was not added to the declared group; calls = %v", fake.calls)
	}
	if !fake.federated(got.Status.UserID, "corp-oidc") {
		t.Errorf("IdP federated-identity link was not created; calls = %v", fake.calls)
	}
	if got.Status.ManagedGroups == nil {
		t.Errorf("status.ManagedGroups not recorded")
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

func TestUserReconcileMembershipPrune(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-prune"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	user := &keycloakv1alpha1.KeycloakUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "dan"},
		Spec: keycloakv1alpha1.KeycloakUserSpec{
			Email:       "dan@example.com",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Groups:      []string{"projects/p/roles/owner", "projects/p/roles/editor"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeKeycloakClient("projects/p/roles/owner", "projects/p/roles/editor")
	ownerID := fake.groups[normPath("projects/p/roles/owner")]
	editorID := fake.groups[normPath("projects/p/roles/editor")]
	r, _ := newUserReconciler(fake, ns)
	key := client.ObjectKeyFromObject(user)
	reconcileUserToSteady(t, ctx, r, key)

	got := getUser(t, ctx, key)
	if !fake.memberOf(got.Status.UserID, ownerID) || !fake.memberOf(got.Status.UserID, editorID) {
		t.Fatalf("user not in both declared groups initially")
	}

	// Drop editor from the spec; the next reconcile must prune that membership.
	got.Spec.Groups = []string{"projects/p/roles/owner"}
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update user spec: %v", err)
	}
	if _, err := reconcileUser(ctx, r, key); err != nil {
		t.Fatalf("reconcile (prune): %v", err)
	}

	if !fake.memberOf(got.Status.UserID, ownerID) {
		t.Errorf("owner membership was incorrectly pruned")
	}
	if fake.memberOf(got.Status.UserID, editorID) {
		t.Errorf("editor membership was not pruned after removal from spec.groups")
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
	if got.Status.ManagedIdentityProvider != "corp-oidc" {
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
				Groups:               []string{"projects/p/roles/owner"},
				IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-abc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/p/roles/owner")
		groupID := fake.groups[normPath("projects/p/roles/owner")]
		userID := fake.seedUser("del-adopted@example.com")
		r, _ := newUserReconciler(fake, ns)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		if !fake.memberOf(userID, groupID) {
			t.Fatalf("adopted user not added to declared group")
		}
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
		if fake.memberOf(userID, groupID) {
			t.Errorf("controller-added membership was not pruned on release")
		}
		if fake.federated(userID, "corp-oidc") {
			t.Errorf("controller-added IdP link was not pruned on release")
		}
	})
}

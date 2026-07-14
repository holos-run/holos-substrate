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

	keycloakv1alpha1 "github.com/holos-run/holos-substrate/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-substrate/api/security/v1alpha1"
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

func getUser(t *testing.T, ctx context.Context, key client.ObjectKey) *keycloakv1alpha1.User {
	t.Helper()
	got := &keycloakv1alpha1.User{}
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
			"kind":       "User",
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

	user := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bob"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:       "bob@example.com",
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{
				Alias:  "corp-oidc",
				UserID: "upstream-sub-123",
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeClient()
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

	user := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "emaillink"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:       "emaillink@example.com",
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			// userId omitted: email-only auto-link, left to the realm flow.
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeClient()
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

	user := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "alice"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:       "alice@example.com",
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeClient()
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

	user := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "carol"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:       "carol@example.com",
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			// Adopt defaults false.
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeClient()
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

	user := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "eve"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:                "eve@example.com",
			InstanceRef:          keycloakv1alpha1.InstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-eve"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeClient()
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

	user := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "grace"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:                "grace@example.com",
			InstanceRef:          keycloakv1alpha1.InstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-grace"},
		},
	}
	if err := shared.k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	fake := newFakeClient()
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

	newUser := func(name string) *keycloakv1alpha1.User {
		u := &keycloakv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Namespace: userNS, Name: name},
			Spec: keycloakv1alpha1.UserSpec{
				Email:       name + "@example.com",
				InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc", Namespace: instNS},
			},
		}
		if err := shared.k8sClient.Create(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		return u
	}

	t.Run("denied without a grant", func(t *testing.T) {
		user := newUser("denied")
		fake := newFakeClient()
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
					Kind:      "User",
					Namespace: userNS,
				}},
				To: []securityv1alpha1.ReferenceGrantTo{{
					Group: keycloakv1alpha1.GroupVersion.Group,
					Kind:  "Instance",
				}},
			},
		}
		createIgnoreExists(t, ctx, grant)

		user := newUser("allowed")
		fake := newFakeClient()
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

func TestUserRenameTransferFlow(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-user-transfer"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	old := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "old-user"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:       "transfer@example.com",
			Username:    "transfer",
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{
				Alias:  "corp-oidc",
				UserID: "sub-transfer",
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, old); err != nil {
		t.Fatalf("create old user: %v", err)
	}
	oldKey := client.ObjectKeyFromObject(old)

	fake := newFakeClient()
	r, _ := newUserReconciler(fake, ns)
	reconcileUserToSteady(t, ctx, r, oldKey)
	old = getUser(t, ctx, oldKey)
	if !old.Status.Created || old.Status.UserID == "" {
		t.Fatalf("old status = %+v, want created with UserID", old.Status)
	}
	oldID := old.Status.UserID
	if !fake.federated(oldID, "corp-oidc") {
		t.Fatalf("old user should have the declared IdP link before transfer")
	}

	old.Spec.DeletionPolicy = keycloakv1alpha1.DeletionPolicyOrphan
	if err := shared.k8sClient.Update(ctx, old); err != nil {
		t.Fatalf("setting old deletionPolicy: %v", err)
	}
	fake.resetCalls()
	if err := shared.k8sClient.Delete(ctx, old); err != nil {
		t.Fatalf("delete old user: %v", err)
	}
	if _, err := reconcileUser(ctx, r, oldKey); err != nil {
		t.Fatalf("reconcile old orphan: %v", err)
	}
	if gotCalls := fake.callCount(); gotCalls != 0 {
		t.Fatalf("orphan transfer should not call Keycloak; calls were %v", fake.calls)
	}
	if !fake.userExists("transfer@example.com") {
		t.Fatal("orphaned user should remain in Keycloak")
	}
	if !fake.federated(oldID, "corp-oidc") {
		t.Fatal("orphaned user should keep its existing IdP link")
	}

	replacement := &keycloakv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "new-user"},
		Spec: keycloakv1alpha1.UserSpec{
			Email:       "transfer@example.com",
			Username:    "transfer",
			InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			Adopt:       true,
			IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{
				Alias:  "corp-oidc",
				UserID: "sub-transfer",
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, replacement); err != nil {
		t.Fatalf("create replacement user: %v", err)
	}
	newKey := client.ObjectKeyFromObject(replacement)
	reconcileUserToSteady(t, ctx, r, newKey)
	replacement = getUser(t, ctx, newKey)
	status, reason, _ := conditionStatus(replacement.Status.Conditions, ConditionReady)
	if status != metav1.ConditionTrue || reason != ReasonAdopted {
		t.Fatalf("replacement Ready = (%v, %v), want (True, %s)", status, reason, ReasonAdopted)
	}
	if replacement.Status.Created || !replacement.Status.Adopted {
		t.Fatalf("replacement Created=%v Adopted=%v, want adopted provenance", replacement.Status.Created, replacement.Status.Adopted)
	}
	if replacement.Status.UserID != oldID {
		t.Fatalf("replacement UserID = %q, want transferred ID %q", replacement.Status.UserID, oldID)
	}
	if !fake.federated(oldID, "corp-oidc") {
		t.Fatal("replacement should preserve the declared IdP link")
	}

	replacement.Spec.DeletionPolicy = keycloakv1alpha1.DeletionPolicyDelete
	if err := shared.k8sClient.Update(ctx, replacement); err != nil {
		t.Fatalf("setting replacement deletionPolicy: %v", err)
	}
	fake.resetCalls()
	if err := shared.k8sClient.Delete(ctx, replacement); err != nil {
		t.Fatalf("delete replacement user: %v", err)
	}
	if _, err := reconcileUser(ctx, r, newKey); err != nil {
		t.Fatalf("reconcile replacement delete: %v", err)
	}
	if fake.userExists("transfer@example.com") {
		t.Fatal("transferred user should be deleted after explicit Delete")
	}
	if !fake.callsContain("DeleteUser:" + oldID) {
		t.Fatalf("explicit Delete should delete transferred user by pinned UUID; calls were %v", fake.calls)
	}
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
		user := &keycloakv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-created"},
			Spec: keycloakv1alpha1.UserSpec{
				Email:       "del-created@example.com",
				InstanceRef: keycloakv1alpha1.InstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
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
		user := &keycloakv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted"},
			Spec: keycloakv1alpha1.UserSpec{
				Email:                "del-adopted@example.com",
				InstanceRef:          keycloakv1alpha1.InstanceReference{Name: "kc"},
				Adopt:                true,
				IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-abc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
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

	t.Run("orphan leaves created user untouched without Keycloak calls", func(t *testing.T) {
		user := &keycloakv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-orphan"},
			Spec: keycloakv1alpha1.UserSpec{
				Email:          "del-orphan@example.com",
				InstanceRef:    keycloakv1alpha1.InstanceReference{Name: "kc"},
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyOrphan,
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
		r, _ := newUserReconciler(fake, ns)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		if !fake.userExists("del-orphan@example.com") {
			t.Fatalf("precondition: user should exist")
		}
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getUser(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileUser(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if gotCalls := fake.callCount(); gotCalls != 0 {
			t.Errorf("deletionPolicy Orphan made Keycloak calls: %v", fake.calls)
		}
		if !fake.userExists("del-orphan@example.com") {
			t.Errorf("orphaned user should remain in Keycloak")
		}
	})

	t.Run("adopted user with explicit delete is deleted by pinned UUID", func(t *testing.T) {
		user := &keycloakv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted-explicit"},
			Spec: keycloakv1alpha1.UserSpec{
				Email:                "del-adopted-explicit@example.com",
				InstanceRef:          keycloakv1alpha1.InstanceReference{Name: "kc"},
				Adopt:                true,
				IdentityProviderLink: &keycloakv1alpha1.IdentityProviderLink{Alias: "corp-oidc", UserID: "sub-explicit"},
				DeletionPolicy:       keycloakv1alpha1.DeletionPolicyDelete,
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
		userID := fake.seedUser("del-adopted-explicit@example.com")
		r, _ := newUserReconciler(fake, ns)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		if !getUser(t, ctx, key).Status.Adopted {
			t.Fatalf("expected adopted user")
		}
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getUser(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileUser(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if fake.userExists("del-adopted-explicit@example.com") {
			t.Errorf("adopted user with deletionPolicy Delete should be deleted")
		}
		if !fake.callsContain("DeleteUser:" + userID) {
			t.Errorf("expected delete by pinned user UUID %q; calls = %v", userID, fake.calls)
		}
		if fake.callsContain("FederatedUnlink:" + userID + "/corp-oidc") {
			t.Errorf("explicit user delete should not separately prune the IdP link; calls = %v", fake.calls)
		}
	})

	t.Run("adopted user explicit delete releases out-of-band replacement", func(t *testing.T) {
		const email = "del-adopted-replaced@example.com"
		user := &keycloakv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted-replaced"},
			Spec: keycloakv1alpha1.UserSpec{
				Email:          email,
				InstanceRef:    keycloakv1alpha1.InstanceReference{Name: "kc"},
				Adopt:          true,
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyDelete,
			},
		}
		if err := shared.k8sClient.Create(ctx, user); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeClient()
		claimedID := fake.seedUser(email)
		r, _ := newUserReconciler(fake, ns)
		key := client.ObjectKeyFromObject(user)
		reconcileUserToSteady(t, ctx, r, key)
		if gotID := getUser(t, ctx, key).Status.UserID; gotID != claimedID {
			t.Fatalf("status.userID = %q, want claimed %q", gotID, claimedID)
		}

		renamed := fake.users[email]
		delete(fake.users, email)
		renamed.Email = "renamed-" + email
		fake.users[renamed.Email] = renamed
		replacementID := fake.seedUser(email)
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getUser(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileUser(ctx, r, key); err != nil {
			t.Fatalf("reconcile (delete): %v", err)
		}
		if !fake.userExists(renamed.Email) {
			t.Errorf("renamed originally-adopted user must not be deleted")
		}
		if !fake.userExists(email) {
			t.Errorf("foreign replacement user must not be deleted")
		}
		if fake.callsContain("DeleteUser:"+claimedID) || fake.callsContain("DeleteUser:"+replacementID) {
			t.Errorf("explicit delete must release on UUID mismatch, not delete either user; calls = %v", fake.calls)
		}
	})
}

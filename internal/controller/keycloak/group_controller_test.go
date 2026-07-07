package keycloak

import (
	"context"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
)

// readyInstance creates a KeycloakInstance in ns with a Ready=True status so a
// KeycloakGroup referencing it can proceed past the dependency gate.
func readyInstance(t *testing.T, ctx context.Context, ns, name string) *keycloakv1alpha1.KeycloakInstance {
	t.Helper()
	inst := &keycloakv1alpha1.KeycloakInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: keycloakv1alpha1.KeycloakInstanceSpec{
			URL:   "https://keycloak.example.test",
			Realm: "holos",
		},
	}
	createIgnoreExists(t, ctx, inst)
	got := &keycloakv1alpha1.KeycloakInstance{}
	if err := shared.k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	markReady(&got.Status.Conditions, ReasonReconciled, "ready", got.Generation)
	got.Status.ObservedGeneration = got.Generation
	if err := shared.k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("setting instance ready: %v", err)
	}
	return got
}

func getGroup(t *testing.T, ctx context.Context, key client.ObjectKey) *keycloakv1alpha1.KeycloakGroup {
	t.Helper()
	got := &keycloakv1alpha1.KeycloakGroup{}
	if err := shared.k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get group: %v", err)
	}
	return got
}

func TestGroupReconcileCreate(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	const ns = "kc-group-create"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	// A KeycloakClient the group confers a role from.
	kclient := &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "consumer"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://app.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	createIgnoreExists(t, ctx, kclient)

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "owner"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/my-project/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{
				{ClientRef: "consumer", Role: "my-project-owner"},
			},
			Custodians: []keycloakv1alpha1.CustodianReference{
				{Path: "projects/my-project/custodians/owner"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}

	fake := newFakeKeycloakClient("projects/my-project/custodians/owner")
	fake.seedClient("https://app.holos.internal", "client-uuid")
	fake.seedClientRole("client-uuid", "my-project-owner", "role-uuid")
	fake.seedClient(adminPermissionsClientID, "perm-client-uuid")
	r, recorder := newGroupReconciler(fake, ns)

	key := client.ObjectKeyFromObject(group)
	// First pass adds the finalizer and requeues.
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	// Second pass provisions.
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile (provision): %v", err)
	}

	got := getGroup(t, ctx, key)
	status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
	if !ok || status != metav1.ConditionTrue || reason != ReasonCreated {
		t.Errorf("Ready = (%v, %v, %v), want (True, %s)", status, reason, ok, ReasonCreated)
	}
	if !got.Status.Created {
		t.Errorf("status.Created = false, want true")
	}
	if !fake.groupExists("projects/my-project/roles/owner") {
		t.Errorf("group was not created in Keycloak")
	}
	if !fake.roleAssigned("grp-2", "client-uuid", "my-project-owner") {
		// grp-1 is the seeded custodian; grp-2 is the role group created here.
		t.Errorf("client role was not conferred; calls = %v", fake.calls)
	}
	if len(fake.fgapResources) == 0 || len(fake.fgapPolicies) == 0 || len(fake.fgapPermissions) == 0 {
		t.Errorf("custodian FGAP wiring incomplete: resources=%v policies=%v perms=%v", fake.fgapResources, fake.fgapPolicies, fake.fgapPermissions)
	}
	if got.Status.LastValidatedTime == nil {
		t.Errorf("lastValidatedTime not set on successful reconcile")
	}
	if got.Status.LastMutatedTime == nil || got.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want time with %q", got.Status.LastMutatedTime, got.Status.LastMutationReason, keycloakv1alpha1.MutationReasonSpecChange)
	}
	assertEvent(t, recorder, ReasonCreated)
}

func TestGroupSteadyStateRefreshesValidationOnly(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-observe"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "viewer"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/observe/roles/viewer",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}

	fake := newFakeKeycloakClient()
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	first := getGroup(t, ctx, key)
	if first.Status.LastValidatedTime == nil || first.Status.LastMutatedTime == nil {
		t.Fatalf("initial timestamps not set: validated=%v mutated=%v", first.Status.LastValidatedTime, first.Status.LastMutatedTime)
	}
	firstValidated := first.Status.LastValidatedTime.DeepCopy()
	firstMutated := first.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	result, err := reconcileGroup(ctx, r, key)
	if err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if result.RequeueAfter != keycloakExternalResourceResync {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, keycloakExternalResourceResync)
	}
	second := getGroup(t, ctx, key)
	if !second.Status.LastValidatedTime.After(firstValidated.Time) {
		t.Errorf("lastValidatedTime did not advance: first=%v second=%v", firstValidated, second.Status.LastValidatedTime)
	}
	if !second.Status.LastMutatedTime.Equal(firstMutated) {
		t.Errorf("lastMutatedTime changed on steady validation: first=%v second=%v", firstMutated, second.Status.LastMutatedTime)
	}
	if second.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("lastMutationReason = %q, want %q", second.Status.LastMutationReason, keycloakv1alpha1.MutationReasonSpecChange)
	}
}

func TestGroupReconcileAdoptAndConflict(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-adopt"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	t.Run("conflict when pre-existing and adopt false", func(t *testing.T) {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "conflict"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:        "projects/foreign/roles/owner",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/foreign/roles/owner") // pre-exists, not ours
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getGroup(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonConflict {
			t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonConflict)
		}
		if got.Status.Created {
			t.Errorf("must not claim ownership of a conflicting group")
		}
	})

	t.Run("adopt when pre-existing and adopt true", func(t *testing.T) {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adopt"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:        "projects/foreign/roles/editor",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				Adopt:       true,
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/foreign/roles/editor")
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getGroup(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionTrue || reason != ReasonAdopted {
			t.Errorf("Ready = (%v, %v), want (True, %s)", status, reason, ReasonAdopted)
		}
		if got.Status.Created || !got.Status.Adopted {
			t.Errorf("Created=%v Adopted=%v, want Created=false Adopted=true", got.Status.Created, got.Status.Adopted)
		}
	})
}

// TestGroupPreviouslyReservedPathsNowReconcile asserts the transparent contract
// (HOL-1421): group paths the controller used to refuse on org-policy grounds
// (the platform-* prefix, the bare "platform"/"authenticated" names, and the
// Keycloak built-in role groups) are now provisioned verbatim — the group is
// created at exactly the declared spec.path and the resource reports Ready/Created,
// with no reserved-name rejection.
func TestGroupPreviouslyReservedPathsNowReconcile(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-formerly-reserved"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	for i, path := range []string{"platform-owner", "platform", "authenticated", "realm_roles", "default-roles-holos"} {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "formerly-reserved-" + strconv.Itoa(i)},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:        path,
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create %q: %v", path, err)
		}
		fake := newFakeKeycloakClient()
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile %q: %v", path, err)
		}
		got := getGroup(t, ctx, key)
		status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
		if !ok || status != metav1.ConditionTrue || reason != ReasonCreated {
			t.Errorf("path %q: Ready = (%v, %v, %v), want (True, %s)", path, status, reason, ok, ReasonCreated)
		}
		if !fake.groupExists(path) {
			t.Errorf("formerly-reserved path %q must now be created verbatim in Keycloak; calls = %v", path, fake.calls)
		}
	}
}

func TestGroupReferenceGrant(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const groupNS = "kc-group-xns-from"
	const instNS = "kc-group-xns-to"
	makeNamespace(t, ctx, groupNS)
	makeNamespace(t, ctx, instNS)
	createIgnoreExists(t, ctx, newCredentialSecret(groupNS, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, instNS, "kc")

	newGroup := func(name string) *keycloakv1alpha1.KeycloakGroup {
		g := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: name},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:        "projects/xns-" + name + "/roles/owner",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc", Namespace: instNS},
			},
		}
		if err := shared.k8sClient.Create(ctx, g); err != nil {
			t.Fatalf("create group: %v", err)
		}
		return g
	}

	t.Run("denied without a grant", func(t *testing.T) {
		group := newGroup("denied")
		fake := newFakeKeycloakClient()
		r, _ := newGroupReconciler(fake, groupNS)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getGroup(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonReferenceNotGranted {
			t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonReferenceNotGranted)
		}
		if fake.callsContain("Get:/projects/xns-denied/roles/owner") {
			t.Errorf("denied reference must not reach Keycloak")
		}
	})

	t.Run("allowed with a grant", func(t *testing.T) {
		grant := &securityv1alpha1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Namespace: instNS, Name: "allow-keycloakgroup"},
			Spec: securityv1alpha1.ReferenceGrantSpec{
				From: []securityv1alpha1.ReferenceGrantFrom{{
					Group:     keycloakv1alpha1.GroupVersion.Group,
					Kind:      "KeycloakGroup",
					Namespace: groupNS,
				}},
				To: []securityv1alpha1.ReferenceGrantTo{{
					Group: keycloakv1alpha1.GroupVersion.Group,
					Kind:  "KeycloakInstance",
				}},
			},
		}
		createIgnoreExists(t, ctx, grant)

		group := newGroup("allowed")
		fake := newFakeKeycloakClient()
		r, _ := newGroupReconciler(fake, groupNS)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getGroup(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionTrue || reason != ReasonCreated {
			t.Errorf("Ready = (%v, %v), want (True, %s)", status, reason, ReasonCreated)
		}
		if !fake.groupExists("projects/xns-allowed/roles/owner") {
			t.Errorf("granted reference should have provisioned the group")
		}
	})
}

func TestGroupDelete(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-delete"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	t.Run("created group is deleted in Keycloak", func(t *testing.T) {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-created"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:        "projects/del/roles/owner",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient()
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !fake.groupExists("projects/del/roles/owner") {
			t.Fatalf("precondition: group should exist")
		}
		groupID := getGroup(t, ctx, key).Status.GroupID
		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if fake.groupExists("projects/del/roles/owner") {
			t.Errorf("created group should be deleted in Keycloak on CR removal")
		}
		if !fake.callsContain("DeleteGroup:" + groupID) {
			t.Errorf("expected a Keycloak delete call; calls = %v", fake.calls)
		}
	})

	t.Run("adopted group is released not deleted", func(t *testing.T) {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:        "projects/del/roles/editor",
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				Adopt:       true,
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/del/roles/editor") // pre-exists → adopted
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		groupID := getGroup(t, ctx, key).Status.GroupID
		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if !fake.groupExists("projects/del/roles/editor") {
			t.Errorf("adopted group must NOT be deleted in Keycloak on CR removal")
		}
		if fake.callsContain("DeleteGroup:" + groupID) {
			t.Errorf("adopted group must be released, not deleted")
		}
	})

	t.Run("orphan leaves created group and side effects untouched without Keycloak calls", func(t *testing.T) {
		createIgnoreExists(t, ctx, &keycloakv1alpha1.KeycloakClient{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "consumer-orphan"},
			Spec: keycloakv1alpha1.KeycloakClientSpec{
				ClientID:    "https://orphan-app.holos.internal",
				Type:        keycloakv1alpha1.KeycloakClientTypePublic,
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			},
		})
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-orphan"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:           "projects/del/roles/orphan",
				InstanceRef:    keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				ClientRoles:    []keycloakv1alpha1.ClientRoleReference{{ClientRef: "consumer-orphan", Role: "del-orphan"}},
				Custodians:     []keycloakv1alpha1.CustodianReference{{Path: "projects/del/custodians/orphan"}},
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyOrphan,
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/del/custodians/orphan")
		fake.seedClient("https://orphan-app.holos.internal", "orphan-client-uuid")
		fake.seedClientRole("orphan-client-uuid", "del-orphan", "orphan-role-uuid")
		fake.seedClient(adminPermissionsClientID, "perm-client-uuid")
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getGroup(t, ctx, key)
		gid := got.Status.GroupID
		if gid == "" || !fake.roleAssigned(gid, "orphan-client-uuid", "del-orphan") {
			t.Fatalf("precondition: role should be assigned on created group; calls = %v", fake.calls)
		}
		fake.resetCalls()

		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if gotCalls := fake.callCount(); gotCalls != 0 {
			t.Errorf("deletionPolicy Orphan made Keycloak calls: %v", fake.calls)
		}
		if !fake.groupExists("projects/del/roles/orphan") {
			t.Errorf("orphaned group should remain in Keycloak")
		}
		if !fake.roleAssigned(gid, "orphan-client-uuid", "del-orphan") {
			t.Errorf("orphaned group role assignment should remain")
		}
		if len(fake.fgapDeletes) != 0 {
			t.Errorf("orphaned group custodian FGAP should not be pruned")
		}
	})

	t.Run("adopted group with explicit delete is deleted after UUID verification", func(t *testing.T) {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted-explicit"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:           "projects/del/roles/adopted-explicit",
				InstanceRef:    keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				Adopt:          true,
				Custodians:     []keycloakv1alpha1.CustodianReference{{Path: "projects/del/custodians/adopted-explicit"}},
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyDelete,
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/del/roles/adopted-explicit", "projects/del/custodians/adopted-explicit")
		fake.seedClient(adminPermissionsClientID, "perm-client-uuid")
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !getGroup(t, ctx, key).Status.Adopted {
			t.Fatalf("expected adopted group")
		}
		groupID := getGroup(t, ctx, key).Status.GroupID

		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if fake.groupExists("projects/del/roles/adopted-explicit") {
			t.Errorf("adopted group with deletionPolicy Delete should be deleted")
		}
		if !fake.callsContain("DeleteGroup:" + groupID) {
			t.Errorf("expected delete call for adopted group with deletionPolicy Delete; calls = %v", fake.calls)
		}
		if len(fake.fgapDeletes) == 0 {
			t.Errorf("custodian FGAP should be pruned before explicit adopted delete")
		}
	})

	t.Run("adopted group explicit delete releases out-of-band replacement", func(t *testing.T) {
		group := &keycloakv1alpha1.KeycloakGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "del-adopted-replaced"},
			Spec: keycloakv1alpha1.KeycloakGroupSpec{
				Path:           "projects/del/roles/adopted-replaced",
				InstanceRef:    keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				Adopt:          true,
				DeletionPolicy: keycloakv1alpha1.DeletionPolicyDelete,
			},
		}
		if err := shared.k8sClient.Create(ctx, group); err != nil {
			t.Fatalf("create: %v", err)
		}
		fake := newFakeKeycloakClient("projects/del/roles/adopted-replaced")
		r, _ := newGroupReconciler(fake, ns)
		key := client.ObjectKeyFromObject(group)
		_, _ = reconcileGroup(ctx, r, key) // finalizer
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		claimedID := getGroup(t, ctx, key).Status.GroupID
		delete(fake.groups, normPath("projects/del/roles/adopted-replaced"))
		replacementID := fake.addGroup("projects/del/roles/adopted-replaced")
		if replacementID == claimedID {
			t.Fatalf("replacement should have a new UUID")
		}

		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if !fake.groupExists("projects/del/roles/adopted-replaced") {
			t.Errorf("foreign replacement group must not be deleted")
		}
	})
}

func TestGroupClientRolePrune(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-roleprune"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	createIgnoreExists(t, ctx, &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "consumer"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://app.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	})

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "roleprune"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/rp/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{
				{ClientRef: "consumer", Role: "rp-owner"},
				{ClientRef: "consumer", Role: "rp-extra"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient()
	fake.seedClient("https://app.holos.internal", "client-uuid")
	fake.seedClientRole("client-uuid", "rp-owner", "role-owner")
	fake.seedClientRole("client-uuid", "rp-extra", "role-extra")
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	gid := getGroup(t, ctx, key).Status.GroupID
	if !fake.roleAssigned(gid, "client-uuid", "rp-owner") || !fake.roleAssigned(gid, "client-uuid", "rp-extra") {
		t.Fatalf("both roles should be assigned initially; calls = %v", fake.calls)
	}

	// Drop rp-extra from the spec; the next reconcile must unassign it.
	got := getGroup(t, ctx, key)
	got.Spec.ClientRoles = []keycloakv1alpha1.ClientRoleReference{{ClientRef: "consumer", Role: "rp-owner"}}
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile after drop: %v", err)
	}
	if fake.roleAssigned(gid, "client-uuid", "rp-extra") {
		t.Errorf("rp-extra should have been pruned; calls = %v", fake.calls)
	}
	if !fake.roleAssigned(gid, "client-uuid", "rp-owner") {
		t.Errorf("rp-owner should remain assigned")
	}
	if mcr := getGroup(t, ctx, key).Status.ManagedClientRoles; len(mcr) != 1 || mcr[0] != managedRoleKey("https://app.holos.internal", "rp-owner") {
		t.Errorf("status.managedClientRoles = %v, want only the owner role", mcr)
	}
}

func TestGroupCustodianPrune(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-custprune"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "custprune"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/cp/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Custodians: []keycloakv1alpha1.CustodianReference{
				{Path: "projects/cp/custodians/owner"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient("projects/cp/custodians/owner")
	fake.seedClient(adminPermissionsClientID, "perm-client-uuid")
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(getGroup(t, ctx, key).Status.ManagedCustodians) != 1 {
		t.Fatalf("expected one managed custodian; got %v", getGroup(t, ctx, key).Status.ManagedCustodians)
	}

	// Drop all custodians; the next reconcile must delete the FGAP policy + perm.
	got := getGroup(t, ctx, key)
	got.Spec.Custodians = nil
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile after drop: %v", err)
	}
	if len(fake.fgapDeletes) == 0 {
		t.Errorf("dropped custodian should have triggered FGAP deletes; calls = %v", fake.calls)
	}
	if mc := getGroup(t, ctx, key).Status.ManagedCustodians; len(mc) != 0 {
		t.Errorf("status.managedCustodians = %v, want empty after prune", mc)
	}
}

func TestGroupReplacedOutOfBand(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-replaced"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "replaced"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/rep/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient()
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	origID := getGroup(t, ctx, key).Status.GroupID
	if origID == "" {
		t.Fatalf("expected a recorded GroupID")
	}

	// Simulate an out-of-band replace: delete and recreate the group at the same
	// path so it gets a new UUID.
	if err := fake.DeleteGroupByPathIfExists(ctx, "projects/rep/roles/owner"); err != nil {
		t.Fatalf("seed delete: %v", err)
	}
	newID := fake.addGroup("projects/rep/roles/owner")
	if newID == origID {
		t.Fatalf("recreated group should have a different UUID")
	}

	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile after replace: %v", err)
	}
	got := getGroup(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonConflict {
		t.Errorf("Ready = (%v, %v), want (False, %s) for an out-of-band replacement", status, reason, ReasonConflict)
	}

	// On delete, the replaced (foreign) group must be released, not deleted.
	if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if !fake.groupExists("projects/rep/roles/owner") {
		t.Errorf("the foreign replacement group must not be deleted")
	}
}

func TestGroupAdoptedReplacedOutOfBand(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-adopt-replaced"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adopt-replaced"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/areplaced/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient("projects/areplaced/roles/owner") // pre-exists → adopted
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if g := getGroup(t, ctx, key); !g.Status.Adopted || g.Status.GroupID == "" {
		t.Fatalf("expected adopted with a recorded GroupID; got %+v", g.Status)
	}

	// Replace the adopted group out of band (new UUID at the same path).
	if err := fake.DeleteGroupByPathIfExists(ctx, "projects/areplaced/roles/owner"); err != nil {
		t.Fatalf("seed delete: %v", err)
	}
	fake.addGroup("projects/areplaced/roles/owner")

	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile after replace: %v", err)
	}
	got := getGroup(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonConflict {
		t.Errorf("Ready = (%v, %v), want (False, %s) for an out-of-band replacement of an adopted group", status, reason, ReasonConflict)
	}
}

func TestGroupGroupIDBackfillPersisted(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-backfill"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "backfill"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/bf/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed the group as already existing in Keycloak and mark the CR as Created with
	// an EMPTY GroupID and Ready already True — the steady-state object whose
	// ownership id needs backfilling.
	fake := newFakeKeycloakClient("projects/bf/roles/owner")
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	got := getGroup(t, ctx, key)
	markReady(&got.Status.Conditions, ReasonCreated, "preexisting", got.Generation)
	got.Status.ObservedGeneration = got.Generation
	got.Status.Created = true
	got.Status.GroupID = ""
	if err := shared.k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gid := getGroup(t, ctx, key).Status.GroupID; gid == "" {
		t.Errorf("GroupID should have been backfilled and persisted on a steady-state reconcile")
	}
}

func TestGroupAdoptedNoSideEffectsDeleteDropsFinalizer(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-adoptnoop"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adoptnoop"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/an/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Adopt:       true,
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient("projects/an/roles/owner") // pre-exists → adopted, no roles/custodians
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !getGroup(t, ctx, key).Status.Adopted {
		t.Fatalf("expected adopted")
	}

	// Now make the instance unresolvable (delete it), then delete the CR. Release is
	// a no-op (no managed side effects), so the finalizer must drop without needing
	// the instance/credential.
	inst := &keycloakv1alpha1.KeycloakInstance{}
	_ = shared.k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kc"}, inst)
	_ = shared.k8sClient.Delete(ctx, inst)

	if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	got := &keycloakv1alpha1.KeycloakGroup{}
	if err := shared.k8sClient.Get(ctx, key, got); err == nil {
		t.Errorf("adopted no-op CR should be gone, still has finalizers %v", got.Finalizers)
	}
}

func TestGroupCreatedDeletePrunesCustodianFGAP(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-createddelfgap"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "createddelfgap"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/cdf/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Custodians:  []keycloakv1alpha1.CustodianReference{{Path: "projects/cdf/custodians/owner"}},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient("projects/cdf/custodians/owner")
	fake.seedClient(adminPermissionsClientID, "perm-client-uuid")
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !getGroup(t, ctx, key).Status.Created {
		t.Fatalf("group should be created-owned")
	}

	// Delete the created group: the group delete cascades its role mappings, but the
	// FGAP custodian objects (on the admin-permissions client) are NOT cascaded and
	// must be pruned explicitly.
	if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if fake.groupExists("projects/cdf/roles/owner") {
		t.Errorf("created group should be deleted")
	}
	if len(fake.fgapDeletes) == 0 {
		t.Errorf("created group's custodian FGAP delegation must be pruned on delete (not cascaded by group delete); calls = %v", fake.calls)
	}
}

func TestGroupDeleteUnprovisionedDropsFinalizer(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-stuckfinalizer"
	makeNamespace(t, ctx, ns)
	// Deliberately NO credential Secret and NO instance: the group can never be
	// provisioned, so deletion must still succeed (no side effects to clean up).

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "never-provisioned"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/np/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "absent"},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient()
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // adds finalizer
	_, _ = reconcileGroup(ctx, r, key) // InstanceNotReady (never Created/Adopted)

	if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The delete reconcile must drop the finalizer without needing an instance or
	// credential, so the CR is actually removed rather than stuck forever.
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	got := &keycloakv1alpha1.KeycloakGroup{}
	err := shared.k8sClient.Get(ctx, key, got)
	if err == nil {
		t.Errorf("CR should be gone (finalizer dropped), but it still exists with finalizers %v", got.Finalizers)
	}
}

func TestGroupClientRolePartialFailureTracked(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-partialrole"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	createIgnoreExists(t, ctx, &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "consumer"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://app.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	})

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "partialrole"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/pf/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{
				{ClientRef: "consumer", Role: "pf-a"},
				{ClientRef: "consumer", Role: "pf-fail"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient()
	fake.seedClient("https://app.holos.internal", "client-uuid")
	fake.seedClientRole("client-uuid", "pf-a", "role-a")
	fake.seedClientRole("client-uuid", "pf-fail", "role-fail")
	// Fail the assignment of the second role only.
	fake.assignRoleErrFor = map[string]bool{"client-uuid/pf-fail": true}
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err == nil {
		t.Fatalf("expected the second role assignment to fail")
	}
	// The first (successful) assignment must be recorded in status even though the
	// reconcile failed, so a later release prunes it rather than leaking it.
	got := getGroup(t, ctx, key)
	found := false
	for _, m := range got.Status.ManagedClientRoles {
		if m == managedRoleKey("https://app.holos.internal", "pf-a") {
			found = true
		}
	}
	if !found {
		t.Errorf("status.managedClientRoles = %v, want it to include the successfully-assigned pf-a", got.Status.ManagedClientRoles)
	}
}

func TestGroupCreateRaceConflict(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-race"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "race"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/race/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The group does NOT exist at the initial GET, but a concurrent actor created it
	// just before our ensure: the fake reports it pre-existing (created=false).
	fake := newFakeKeycloakClient("projects/race/roles/owner")
	// Force the initial GetGroupByPath to report NotFound so reconcileCreate runs,
	// then EnsureGroupByPathCreated finds it already present (created=false).
	fake.groupGetNotFoundOnce = map[string]bool{"/projects/race/roles/owner": true}
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGroup(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonConflict {
		t.Errorf("Ready = (%v, %v), want (False, %s) for a lost create race with adopt=false", status, reason, ReasonConflict)
	}
	if got.Status.Created {
		t.Errorf("must not claim ownership of a group won by a create race")
	}
}

func TestGroupAdoptedDeletePrunesManaged(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-adoptprune"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	createIgnoreExists(t, ctx, &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "consumer"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://app.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	})

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "adoptprune"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/ap/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			Adopt:       true,
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{{ClientRef: "consumer", Role: "ap-owner"}},
			Custodians:  []keycloakv1alpha1.CustodianReference{{Path: "projects/ap/custodians/owner"}},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient("projects/ap/roles/owner", "projects/ap/custodians/owner") // role group pre-exists → adopted
	fake.seedClient("https://app.holos.internal", "client-uuid")
	fake.seedClientRole("client-uuid", "ap-owner", "role-owner")
	fake.seedClient(adminPermissionsClientID, "perm-client-uuid")
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGroup(t, ctx, key)
	if !got.Status.Adopted {
		t.Fatalf("expected adopted; got %+v", got.Status)
	}
	gid := got.Status.GroupID
	if !fake.roleAssigned(gid, "client-uuid", "ap-owner") {
		t.Fatalf("role should be assigned on the adopted group; calls = %v", fake.calls)
	}

	// Delete the CR: the adopted group must survive, but its controller-added role
	// and custodian delegation must be pruned.
	if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if !fake.groupExists("projects/ap/roles/owner") {
		t.Errorf("adopted group must not be deleted")
	}
	if fake.roleAssigned(gid, "client-uuid", "ap-owner") {
		t.Errorf("controller-added role must be revoked on adopted release; calls = %v", fake.calls)
	}
	if len(fake.fgapDeletes) == 0 {
		t.Errorf("controller-added custodian delegation must be pruned on adopted release")
	}
}

func TestGroupInstanceNotReady(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-instnotready"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	// Note: no instance created → InstanceNotReady.

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "no-instance"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/x/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "absent"},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := newFakeKeycloakClient()
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGroup(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonInstanceNotReady {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonInstanceNotReady)
	}
}

// TestGroupConfersArbitraryRoleByClientID is the transparency/round-trip test
// required by HOL-1421: a KeycloakGroup with an arbitrary spec.path plus a
// clientRoles[] entry naming a client by clientId with an arbitrary role name
// produces a Keycloak group at exactly that path with exactly that role conferred —
// identical names, no prefix added/stripped, no allowlist, no project==namespace
// check, no reserved-role refusal. It supersedes the former Quay-only direct-path
// test (HOL-1350) now that the direct clientId path is fully general.
func TestGroupConfersArbitraryRoleByClientID(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	// A namespace that deliberately does NOT equal the path's project segment, and a
	// clientId/role that the removed guards would have refused (a built-in client,
	// an arbitrary role name unrelated to the path). Transparency means none of that
	// matters.
	const ns = "kc-group-transparent"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	const (
		path     = "projects/other-project/roles/owner"
		clientID = "realm-management"
		roleName = "arbitrary-role-name"
	)
	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "transparent"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        path,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{
				{ClientID: clientID, Role: roleName},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}

	fake := newFakeKeycloakClient()
	// The named client exists, but the role does NOT — the direct clientId path
	// creates it before assigning (no KeycloakClient CR is responsible for it).
	fake.seedClient(clientID, "target-uuid")
	r, recorder := newGroupReconciler(fake, ns)

	key := client.ObjectKeyFromObject(group)
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile (provision): %v", err)
	}

	got := getGroup(t, ctx, key)
	status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
	if !ok || status != metav1.ConditionTrue || reason != ReasonCreated {
		t.Errorf("Ready = (%v, %v, %v), want (True, %s)", status, reason, ok, ReasonCreated)
	}
	// Round-trip: the group exists at EXACTLY the declared path (no rewriting).
	if !fake.groupExists(path) {
		t.Errorf("group was not created at the verbatim path %q; calls = %v", path, fake.calls)
	}
	// The role group is the only group created here, so it is grp-1. The EXACT role
	// name was created on and assigned to the named client (no prefix, no rewrite).
	if !fake.callsContain("CreateClientRole:target-uuid/" + roleName) {
		t.Errorf("reconciler did not ensure the verbatim role %q on client %q; calls = %v", roleName, clientID, fake.calls)
	}
	if !fake.roleAssigned("grp-1", "target-uuid", roleName) {
		t.Errorf("verbatim role %q was not conferred on client %q; calls = %v", roleName, clientID, fake.calls)
	}
	// status.managedClientRoles records it keyed by the verbatim clientId/role
	// (NUL-separated so an arbitrary role name round-trips unambiguously). Decoding
	// the recorded entry must recover the verbatim clientId and role.
	mcr := got.Status.ManagedClientRoles
	if len(mcr) != 1 {
		t.Fatalf("status.managedClientRoles = %v, want exactly one entry", mcr)
	}
	gotClientID, gotRole, ok := splitManagedRole(mcr[0])
	if !ok || gotClientID != clientID || gotRole != roleName {
		t.Errorf("decoded managedClientRoles[0] = (%q, %q, %v), want (%q, %q, true)", gotClientID, gotRole, ok, clientID, roleName)
	}
	assertEvent(t, recorder, ReasonCreated)
}

// TestManagedRoleKeyRoundTrip is a pure unit test (no envtest) of the
// status.managedClientRoles encoding (HOL-1421 review round 1): the transparent
// reconciler allows arbitrary role names, so the encoding must round-trip a role or
// clientId containing slashes unambiguously, and must still decode the legacy
// "<clientId>/<role>" form written before the fix. A mis-decode resolves the wrong
// role on prune/release and leaks a stale Keycloak role mapping.
func TestManagedRoleKeyRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		clientID string
		role     string
	}{
		{"plain", "https://app.holos.internal", "owner"},
		{"slash in role", "https://app.holos.internal", "team/owner"},
		{"slash in role and url client", "https://quay.holos.internal", "a/b/c"},
		{"bare client id", "realm-management", "manage/users"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := managedRoleKey(tc.clientID, tc.role)
			gotClient, gotRole, ok := splitManagedRole(key)
			if !ok || gotClient != tc.clientID || gotRole != tc.role {
				t.Errorf("round-trip = (%q, %q, %v), want (%q, %q, true)", gotClient, gotRole, ok, tc.clientID, tc.role)
			}
		})
	}

	// Legacy "<clientId>/<role>" entries (slash-free role names, as the removed
	// guard required) must still decode via the LastIndex fallback.
	gotClient, gotRole, ok := splitManagedRole("https://quay.holos.internal/my-project-owner")
	if !ok || gotClient != "https://quay.holos.internal" || gotRole != "my-project-owner" {
		t.Errorf("legacy decode = (%q, %q, %v), want (https://quay.holos.internal, my-project-owner, true)", gotClient, gotRole, ok)
	}

	// Malformed keys are rejected.
	for _, bad := range []string{"", "noseparator", "\x00role", "client\x00"} {
		if _, _, ok := splitManagedRole(bad); ok {
			t.Errorf("splitManagedRole(%q) = ok, want false", bad)
		}
	}
}

// TestGroupLegacyManagedRoleNotPrunedOnUpgrade is the round-2 regression test
// (HOL-1421): an in-cluster CR whose status.managedClientRoles was written by an
// older controller in the legacy "<clientId>/<role>" encoding must NOT have a
// still-desired role revoked after upgrade. The reconciler canonicalizes the legacy
// entry to the NUL encoding before the prune comparison, so the desired role matches
// and is left assigned; status is rewritten in canonical form.
func TestGroupLegacyManagedRoleNotPrunedOnUpgrade(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-legacy-upgrade"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	createIgnoreExists(t, ctx, &keycloakv1alpha1.KeycloakClient{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "consumer"},
		Spec: keycloakv1alpha1.KeycloakClientSpec{
			ClientID:    "https://app.holos.internal",
			Type:        keycloakv1alpha1.KeycloakClientTypePublic,
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
		},
	})

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "legacy"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "projects/lg/roles/owner",
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			ClientRoles: []keycloakv1alpha1.ClientRoleReference{
				{ClientRef: "consumer", Role: "lg-owner"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, group); err != nil {
		t.Fatalf("create: %v", err)
	}

	fake := newFakeKeycloakClient()
	fake.seedClient("https://app.holos.internal", "client-uuid")
	fake.seedClientRole("client-uuid", "lg-owner", "role-owner")
	r, _ := newGroupReconciler(fake, ns)
	key := client.ObjectKeyFromObject(group)
	_, _ = reconcileGroup(ctx, r, key) // finalizer + provision (claims ownership)
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	gid := getGroup(t, ctx, key).Status.GroupID
	if !fake.roleAssigned(gid, "client-uuid", "lg-owner") {
		t.Fatalf("lg-owner should be assigned initially; calls = %v", fake.calls)
	}

	// Simulate a pre-upgrade controller's status: overwrite managedClientRoles with
	// the legacy "<clientId>/<role>" encoding for the still-desired role.
	got := getGroup(t, ctx, key)
	got.Status.ManagedClientRoles = []string{"https://app.holos.internal/lg-owner"}
	if err := shared.k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("seed legacy status: %v", err)
	}

	// Reconcile again with the spec UNCHANGED. The legacy entry must canonicalize and
	// match the desired role, so the role is not revoked.
	if _, err := reconcileGroup(ctx, r, key); err != nil {
		t.Fatalf("reconcile after legacy seed: %v", err)
	}
	if !fake.roleAssigned(gid, "client-uuid", "lg-owner") {
		t.Errorf("still-desired lg-owner must NOT be revoked on upgrade; calls = %v", fake.calls)
	}
	if mcr := getGroup(t, ctx, key).Status.ManagedClientRoles; len(mcr) != 1 || mcr[0] != managedRoleKey("https://app.holos.internal", "lg-owner") {
		t.Errorf("status.managedClientRoles = %v, want canonical [%q]", mcr, managedRoleKey("https://app.holos.internal", "lg-owner"))
	}
}

package keycloak

import (
	"context"
	"testing"

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
			ClientID:    "https://app.holos.localhost",
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
	fake.seedClient("https://app.holos.localhost", "client-uuid")
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
	assertEvent(t, recorder, ReasonCreated)
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

func TestGroupReservedName(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-group-reserved"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")

	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "reserved"},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        "platform-owner",
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
	got := getGroup(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonReserved {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonReserved)
	}
	if fake.callsContain("Get:/platform-owner") {
		t.Errorf("reserved guard must reject before any Keycloak call")
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
		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if fake.groupExists("projects/del/roles/owner") {
			t.Errorf("created group should be deleted in Keycloak on CR removal")
		}
		if !fake.callsContain("Delete:/projects/del/roles/owner") {
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
		if err := shared.k8sClient.Delete(ctx, getGroup(t, ctx, key)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := reconcileGroup(ctx, r, key); err != nil {
			t.Fatalf("reconcile delete: %v", err)
		}
		if !fake.groupExists("projects/del/roles/editor") {
			t.Errorf("adopted group must NOT be deleted in Keycloak on CR removal")
		}
		if fake.callsContain("Delete:/projects/del/roles/editor") {
			t.Errorf("adopted group must be released, not deleted")
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
			ClientID:    "https://app.holos.localhost",
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
	fake.seedClient("https://app.holos.localhost", "client-uuid")
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
	if mcr := getGroup(t, ctx, key).Status.ManagedClientRoles; len(mcr) != 1 || mcr[0] != "https://app.holos.localhost/rp-owner" {
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
			ClientID:    "https://app.holos.localhost",
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
	fake.seedClient("https://app.holos.localhost", "client-uuid")
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
		if m == "https://app.holos.localhost/pf-a" {
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
			ClientID:    "https://app.holos.localhost",
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
	fake.seedClient("https://app.holos.localhost", "client-uuid")
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

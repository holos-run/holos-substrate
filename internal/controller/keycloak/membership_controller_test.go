package keycloak

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
)

func getMembership(t *testing.T, ctx context.Context, key client.ObjectKey) *keycloakv1alpha1.KeycloakGroupMembership {
	t.Helper()
	got := &keycloakv1alpha1.KeycloakGroupMembership{}
	if err := shared.k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get membership: %v", err)
	}
	return got
}

func readyGroup(t *testing.T, ctx context.Context, ns, name, path string, instanceRef keycloakv1alpha1.KeycloakInstanceReference) *keycloakv1alpha1.KeycloakGroup {
	t.Helper()
	group := &keycloakv1alpha1.KeycloakGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: keycloakv1alpha1.KeycloakGroupSpec{
			Path:        path,
			InstanceRef: instanceRef,
		},
	}
	createIgnoreExists(t, ctx, group)
	got := getGroup(t, ctx, client.ObjectKeyFromObject(group))
	markReady(&got.Status.Conditions, ReasonReconciled, "ready", got.Generation)
	got.Status.ObservedGeneration = got.Generation
	got.Status.GroupID = "group-cr-id"
	if err := shared.k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("setting group ready: %v", err)
	}
	return got
}

func reconcileMembershipToSteady(t *testing.T, ctx context.Context, r *MembershipReconciler, key client.ObjectKey) ctrl.Result {
	t.Helper()
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	result, err := reconcileMembership(ctx, r, key)
	if err != nil {
		t.Fatalf("reconcile (membership): %v", err)
	}
	return result
}

func TestMembershipReconcileSameNamespaceJoin(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned (KUBEBUILDER_ASSETS unset); run via make controller-test")
	}
	ctx := context.Background()
	const ns = "kc-membership-join"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")
	readyGroup(t, ctx, ns, "owner", "projects/p/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"})

	membership := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "owner-members"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner"},
			Members:     []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: "bob@example.com"}},
		},
	}
	if err := shared.k8sClient.Create(ctx, membership); err != nil {
		t.Fatalf("create membership: %v", err)
	}

	fake := newFakeKeycloakClient("projects/p/roles/owner")
	groupID := fake.groups[normPath("projects/p/roles/owner")]
	userID := fake.seedUser("bob@example.com")
	r, recorder := newMembershipReconciler(fake, ns)
	result := reconcileMembershipToSteady(t, ctx, r, client.ObjectKeyFromObject(membership))

	got := getMembership(t, ctx, client.ObjectKeyFromObject(membership))
	status, reason, ok := conditionStatus(got.Status.Conditions, ConditionReady)
	if !ok || status != metav1.ConditionTrue || reason != ReasonReconciled {
		t.Errorf("Ready = (%v, %v, %v), want (True, %s)", status, reason, ok, ReasonReconciled)
	}
	if !fake.memberOf(userID, groupID) {
		t.Errorf("user was not added to the group; calls = %v", fake.calls)
	}
	if got.Status.GroupID != groupID {
		t.Errorf("status.GroupID = %q, want %q", got.Status.GroupID, groupID)
	}
	if len(got.Status.ManagedMembers) != 1 || got.Status.ManagedMembers[0].Email != "bob@example.com" || got.Status.ManagedMembers[0].UserID != userID {
		t.Errorf("status.ManagedMembers = %+v", got.Status.ManagedMembers)
	}
	if got.Status.LastValidatedTime == nil || got.Status.LastMutatedTime == nil {
		t.Fatalf("validation/mutation timestamps were not recorded: %+v", got.Status)
	}
	if got.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("LastMutationReason = %q, want SpecChange", got.Status.LastMutationReason)
	}
	if got.Status.LastDriftTime != nil {
		t.Errorf("LastDriftTime = %v, want nil for initial spec-driven mutation", got.Status.LastDriftTime)
	}
	if result.RequeueAfter != membershipResync {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, membershipResync)
	}
	assertEvent(t, recorder, ReasonReconciled)
}

func TestMembershipGroupReferenceGrant(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const membershipNS = "kc-membership-xns-from"
	const groupNS = "kc-membership-xns-to"
	makeNamespace(t, ctx, membershipNS)
	makeNamespace(t, ctx, groupNS)
	createIgnoreExists(t, ctx, newCredentialSecret(membershipNS, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, membershipNS, "kc")
	readyGroup(t, ctx, groupNS, "owner", "projects/xns/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc", Namespace: membershipNS})

	newMembership := func(name string) *keycloakv1alpha1.KeycloakGroupMembership {
		m := &keycloakv1alpha1.KeycloakGroupMembership{
			ObjectMeta: metav1.ObjectMeta{Namespace: membershipNS, Name: name},
			Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
				InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
				GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner", Namespace: groupNS},
				Members:     []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: name + "@example.com"}},
			},
		}
		if err := shared.k8sClient.Create(ctx, m); err != nil {
			t.Fatalf("create membership: %v", err)
		}
		return m
	}

	t.Run("denied without grant", func(t *testing.T) {
		m := newMembership("denied")
		fake := newFakeKeycloakClient("projects/xns/roles/owner")
		fake.seedUser("denied@example.com")
		r, _ := newMembershipReconciler(fake, membershipNS)
		key := client.ObjectKeyFromObject(m)
		_, _ = reconcileMembership(ctx, r, key)
		if _, err := reconcileMembership(ctx, r, key); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getMembership(t, ctx, key)
		status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
		if status != metav1.ConditionFalse || reason != ReasonReferenceNotGranted {
			t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonReferenceNotGranted)
		}
		if fake.callsContain("Get:/projects/xns/roles/owner") {
			t.Errorf("denied groupRef must not reach Keycloak; calls = %v", fake.calls)
		}
	})

	t.Run("allowed with grant", func(t *testing.T) {
		grant := &securityv1alpha1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "allow-membership"},
			Spec: securityv1alpha1.ReferenceGrantSpec{
				From: []securityv1alpha1.ReferenceGrantFrom{{
					Group:     keycloakv1alpha1.GroupVersion.Group,
					Kind:      "KeycloakGroupMembership",
					Namespace: membershipNS,
				}},
				To: []securityv1alpha1.ReferenceGrantTo{{
					Group: keycloakv1alpha1.GroupVersion.Group,
					Kind:  "KeycloakGroup",
				}},
			},
		}
		createIgnoreExists(t, ctx, grant)

		m := newMembership("allowed")
		fake := newFakeKeycloakClient("projects/xns/roles/owner")
		userID := fake.seedUser("allowed@example.com")
		groupID := fake.groups[normPath("projects/xns/roles/owner")]
		r, _ := newMembershipReconciler(fake, membershipNS)
		reconcileMembershipToSteady(t, ctx, r, client.ObjectKeyFromObject(m))
		if !fake.memberOf(userID, groupID) {
			t.Errorf("granted groupRef should have converged membership")
		}
	})
}

func TestMembershipPruneDeleteAndUUIDPin(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-membership-prune"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")
	readyGroup(t, ctx, ns, "owner", "projects/p2/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"})

	membership := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "owner-members"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner"},
			Members: []keycloakv1alpha1.KeycloakGroupMembershipMember{
				{Email: "alice@example.com"},
				{Email: "bob@example.com"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, membership); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	fake := newFakeKeycloakClient("projects/p2/roles/owner")
	groupID := fake.groups[normPath("projects/p2/roles/owner")]
	aliceID := fake.seedUser("alice@example.com")
	bobID := fake.seedUser("bob@example.com")
	r, _ := newMembershipReconciler(fake, ns)
	key := client.ObjectKeyFromObject(membership)
	reconcileMembershipToSteady(t, ctx, r, key)

	got := getMembership(t, ctx, key)
	got.Spec.Members = []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: "alice@example.com"}}
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update membership spec: %v", err)
	}
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile prune: %v", err)
	}
	if !fake.memberOf(aliceID, groupID) {
		t.Errorf("alice should remain a member")
	}
	if fake.memberOf(bobID, groupID) {
		t.Errorf("bob should have been pruned")
	}

	// Recreate the group at the same path with a new UUID and add a foreign
	// membership to the replacement. Dropping alice from spec must release the old
	// managed status without removing the replacement group's membership.
	delete(fake.groups, normPath("projects/p2/roles/owner"))
	newGroupID := fake.addGroup("projects/p2/roles/owner")
	fake.groupMembers[aliceID+"/"+newGroupID] = true
	got = getMembership(t, ctx, key)
	got.Spec.Members = nil
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update membership spec empty: %v", err)
	}
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile uuid-pinned prune: %v", err)
	}
	if !fake.memberOf(aliceID, newGroupID) {
		t.Errorf("UUID-pinned prune removed membership from a replacement group")
	}
	if managed := getMembership(t, ctx, key).Status.ManagedMembers; len(managed) != 0 {
		t.Errorf("managed members = %+v, want empty after release", managed)
	}

	// Restore one managed member and verify finalization prunes it.
	got = getMembership(t, ctx, key)
	got.Spec.Members = []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: "alice@example.com"}}
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("restore membership spec: %v", err)
	}
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile restore: %v", err)
	}
	if err := shared.k8sClient.Delete(ctx, getMembership(t, ctx, key)); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if fake.memberOf(aliceID, newGroupID) {
		t.Errorf("finalizer did not prune managed membership")
	}
}

func TestMembershipUnauthorizedPeerDoesNotBlockPrune(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ownerNS = "kc-membership-peer-owner"
	const peerNS = "kc-membership-peer-denied"
	makeNamespace(t, ctx, ownerNS)
	makeNamespace(t, ctx, peerNS)
	createIgnoreExists(t, ctx, newCredentialSecret(ownerNS, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ownerNS, "kc")
	readyGroup(t, ctx, ownerNS, "owner", "projects/peer/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"})

	owner := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: ownerNS, Name: "owner-members"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner"},
			Members:     []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: "bob@example.com"}},
		},
	}
	if err := shared.k8sClient.Create(ctx, owner); err != nil {
		t.Fatalf("create owner membership: %v", err)
	}
	peer := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: peerNS, Name: "unauthorized-peer"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc", Namespace: ownerNS},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner", Namespace: ownerNS},
			Members:     []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: "bob@example.com"}},
		},
	}
	if err := shared.k8sClient.Create(ctx, peer); err != nil {
		t.Fatalf("create peer membership: %v", err)
	}

	fake := newFakeKeycloakClient("projects/peer/roles/owner")
	groupID := fake.groups[normPath("projects/peer/roles/owner")]
	bobID := fake.seedUser("bob@example.com")
	r, _ := newMembershipReconciler(fake, ownerNS)
	key := client.ObjectKeyFromObject(owner)
	reconcileMembershipToSteady(t, ctx, r, key)
	if !fake.memberOf(bobID, groupID) {
		t.Fatalf("bob should have been added before prune")
	}

	got := getMembership(t, ctx, key)
	got.Spec.Members = nil
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("drop owner member: %v", err)
	}
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile prune: %v", err)
	}
	if fake.memberOf(bobID, groupID) {
		t.Errorf("ungranted peer membership blocked pruning")
	}
}

func TestMembershipMissingMemberConvergesOthers(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-membership-missing"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")
	readyGroup(t, ctx, ns, "owner", "projects/missing/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"})

	membership := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "owner-members"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner"},
			Members: []keycloakv1alpha1.KeycloakGroupMembershipMember{
				{Email: "present@example.com"},
				{Email: "missing@example.com"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, membership); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	fake := newFakeKeycloakClient("projects/missing/roles/owner")
	groupID := fake.groups[normPath("projects/missing/roles/owner")]
	userID := fake.seedUser("present@example.com")
	r, _ := newMembershipReconciler(fake, ns)
	key := client.ObjectKeyFromObject(membership)
	_, _ = reconcileMembership(ctx, r, key)
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getMembership(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonMemberNotFound {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonMemberNotFound)
	}
	if !fake.memberOf(userID, groupID) {
		t.Errorf("present member should still converge")
	}
	if got.Status.LastValidatedTime != nil {
		t.Errorf("lastValidatedTime should not advance on incomplete validation")
	}
	if got.Status.LastMutatedTime == nil || got.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("completed mutation was not recorded: %+v", got.Status)
	}

	fake.seedUser("missing@example.com")
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile after seeding missing user: %v", err)
	}
	got = getMembership(t, ctx, key)
	status, reason, _ = conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionTrue || reason != ReasonReconciled {
		t.Errorf("Ready after seed = (%v, %v), want (True, %s)", status, reason, ReasonReconciled)
	}
}

func TestMembershipPartialMutationFailureStampsMutation(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-membership-partial-error"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")
	readyGroup(t, ctx, ns, "owner", "projects/partial/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"})

	membership := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "owner-members"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner"},
			Members: []keycloakv1alpha1.KeycloakGroupMembershipMember{
				{Email: "alice@example.com"},
				{Email: "bob@example.com"},
			},
		},
	}
	if err := shared.k8sClient.Create(ctx, membership); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	fake := newFakeKeycloakClient("projects/partial/roles/owner")
	groupID := fake.groups[normPath("projects/partial/roles/owner")]
	aliceID := fake.seedUser("alice@example.com")
	bobID := fake.seedUser("bob@example.com")
	fake.listUserGroupsErrFor[bobID] = errors.New("simulated later list failure")
	r, _ := newMembershipReconciler(fake, ns)
	key := client.ObjectKeyFromObject(membership)

	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	if _, err := reconcileMembership(ctx, r, key); err == nil {
		t.Fatalf("expected partial reconcile error")
	}

	got := getMembership(t, ctx, key)
	status, reason, _ := conditionStatus(got.Status.Conditions, ConditionReady)
	if status != metav1.ConditionFalse || reason != ReasonKeycloakError {
		t.Errorf("Ready = (%v, %v), want (False, %s)", status, reason, ReasonKeycloakError)
	}
	if !fake.memberOf(aliceID, groupID) {
		t.Errorf("first member mutation did not happen before later failure")
	}
	if fake.memberOf(bobID, groupID) {
		t.Errorf("failed member should not have been added")
	}
	if got.Status.LastMutatedTime == nil || got.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("partial mutation was not recorded: %+v", got.Status)
	}
	if got.Status.LastValidatedTime != nil {
		t.Errorf("failed reconcile advanced validation time: %v", got.Status.LastValidatedTime)
	}
	if got.Status.GroupID != groupID {
		t.Errorf("status.GroupID = %q, want %q after partial mutation", got.Status.GroupID, groupID)
	}
	if len(got.Status.ManagedMembers) != 1 || got.Status.ManagedMembers[0].Email != "alice@example.com" {
		t.Errorf("managed members = %+v, want only alice after partial mutation", got.Status.ManagedMembers)
	}
}

func TestMembershipDriftTimestamps(t *testing.T) {
	if shared == nil {
		t.Skip("envtest not provisioned")
	}
	ctx := context.Background()
	const ns = "kc-membership-drift"
	makeNamespace(t, ctx, ns)
	createIgnoreExists(t, ctx, newCredentialSecret(ns, keycloakv1alpha1.DefaultCredentialsSecretName))
	readyInstance(t, ctx, ns, "kc")
	readyGroup(t, ctx, ns, "owner", "projects/drift/roles/owner", keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"})

	membership := &keycloakv1alpha1.KeycloakGroupMembership{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "owner-members"},
		Spec: keycloakv1alpha1.KeycloakGroupMembershipSpec{
			InstanceRef: keycloakv1alpha1.KeycloakInstanceReference{Name: "kc"},
			GroupRef:    keycloakv1alpha1.KeycloakGroupReference{Name: "owner"},
			Members:     []keycloakv1alpha1.KeycloakGroupMembershipMember{{Email: "alice@example.com"}},
		},
	}
	if err := shared.k8sClient.Create(ctx, membership); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	fake := newFakeKeycloakClient("projects/drift/roles/owner")
	groupID := fake.groups[normPath("projects/drift/roles/owner")]
	aliceID := fake.seedUser("alice@example.com")
	r, _ := newMembershipReconciler(fake, ns)
	key := client.ObjectKeyFromObject(membership)
	reconcileMembershipToSteady(t, ctx, r, key)

	first := getMembership(t, ctx, key)
	if first.Status.LastValidatedTime == nil || first.Status.LastMutatedTime == nil {
		t.Fatalf("initial timestamps missing: %+v", first.Status)
	}
	firstValidation := first.Status.LastValidatedTime.DeepCopy()
	firstMutation := first.Status.LastMutatedTime.DeepCopy()

	time.Sleep(1100 * time.Millisecond)
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	second := getMembership(t, ctx, key)
	if !second.Status.LastValidatedTime.After(firstValidation.Time) {
		t.Errorf("lastValidatedTime did not advance on steady-state validation")
	}
	if !second.Status.LastMutatedTime.Equal(firstMutation) {
		t.Errorf("lastMutatedTime changed on no-op validation: before=%v after=%v", firstMutation, second.Status.LastMutatedTime)
	}
	if second.Status.LastDriftTime != nil {
		t.Errorf("lastDriftTime set on no-op validation")
	}

	delete(fake.groupMembers, aliceID+"/"+groupID)
	time.Sleep(1100 * time.Millisecond)
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("drift reconcile: %v", err)
	}
	drifted := getMembership(t, ctx, key)
	if drifted.Status.LastMutationReason != keycloakv1alpha1.MutationReasonDriftRemediation {
		t.Errorf("LastMutationReason = %q, want DriftRemediation", drifted.Status.LastMutationReason)
	}
	if drifted.Status.LastDriftTime == nil || !drifted.Status.LastDriftTime.Equal(drifted.Status.LastMutatedTime) {
		t.Errorf("LastDriftTime = %v, LastMutatedTime = %v; want same instant", drifted.Status.LastDriftTime, drifted.Status.LastMutatedTime)
	}
	driftTime := drifted.Status.LastDriftTime.DeepCopy()

	fake.seedUser("bob@example.com")
	got := getMembership(t, ctx, key)
	got.Spec.Members = append(got.Spec.Members, keycloakv1alpha1.KeycloakGroupMembershipMember{Email: "bob@example.com"})
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update membership spec: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("spec-change reconcile: %v", err)
	}
	specChanged := getMembership(t, ctx, key)
	if specChanged.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("LastMutationReason = %q, want SpecChange", specChanged.Status.LastMutationReason)
	}
	if specChanged.Status.LastDriftTime == nil || !specChanged.Status.LastDriftTime.Equal(driftTime) {
		t.Errorf("LastDriftTime = %v, want retained %v", specChanged.Status.LastDriftTime, driftTime)
	}

	delete(fake.groupMembers, aliceID+"/"+groupID)
	fake.seedUser("carol@example.com")
	got = getMembership(t, ctx, key)
	got.Spec.Members = append(got.Spec.Members, keycloakv1alpha1.KeycloakGroupMembershipMember{Email: "carol@example.com"})
	if err := shared.k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update membership spec for mixed drift/spec change: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := reconcileMembership(ctx, r, key); err != nil {
		t.Fatalf("mixed drift/spec reconcile: %v", err)
	}
	mixed := getMembership(t, ctx, key)
	if mixed.Status.LastMutationReason != keycloakv1alpha1.MutationReasonSpecChange {
		t.Errorf("LastMutationReason = %q, want SpecChange for mixed reconcile", mixed.Status.LastMutationReason)
	}
	if mixed.Status.LastDriftTime == nil || !mixed.Status.LastDriftTime.After(driftTime.Time) {
		t.Errorf("LastDriftTime = %v, want newer drift stamp after mixed reconcile", mixed.Status.LastDriftTime)
	}
	if mixed.Status.LastDriftTime == nil || mixed.Status.LastMutatedTime == nil || !mixed.Status.LastDriftTime.Equal(mixed.Status.LastMutatedTime) {
		t.Errorf("mixed drift/spec timestamps = drift %v mutated %v, want same instant", mixed.Status.LastDriftTime, mixed.Status.LastMutatedTime)
	}

	beforeErrorValidation := mixed.Status.LastValidatedTime.DeepCopy()
	fake.listUserGroupsErr = errors.New("simulated list failure")
	if _, err := reconcileMembership(ctx, r, key); err == nil {
		t.Fatalf("expected reconcile error")
	}
	afterError := getMembership(t, ctx, key)
	if afterError.Status.LastValidatedTime == nil || !afterError.Status.LastValidatedTime.Equal(beforeErrorValidation) {
		t.Errorf("lastValidatedTime changed on failed reconcile: before=%v after=%v", beforeErrorValidation, afterError.Status.LastValidatedTime)
	}
}

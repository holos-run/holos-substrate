package keycloak

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	keycloakv1alpha1 "github.com/holos-run/holos-substrate/api/keycloak/v1alpha1"
	ctrlshared "github.com/holos-run/holos-substrate/internal/controller/shared"
	"github.com/holos-run/holos-substrate/internal/keycloak"
	"github.com/holos-run/holos-substrate/internal/referencegrant"
)

const (
	membershipFinalizer = "membership.keycloak.holos.run/finalizer"
	membershipResync    = time.Hour
)

// MembershipClient is the seam the GroupMembership reconciler drives
// Keycloak through. The concrete *keycloak.Client satisfies it; tests inject the
// package fake.
type MembershipClient interface {
	GetGroupByPath(ctx context.Context, path string) (*keycloak.Group, error)
	FindUserByEmail(ctx context.Context, email string) (*keycloak.User, error)
	ListUserGroups(ctx context.Context, userID string) ([]keycloak.Group, error)
	AddUserToGroup(ctx context.Context, userID, groupID string) error
	RemoveUserFromGroupIfMember(ctx context.Context, userID, groupID string) error
}

type MembershipClientFactory func(cred *keycloakCredential, url, realm string, caBundle []byte) MembershipClient

func NewKeycloakMembershipClient(cred *keycloakCredential, url, realm string, caBundle []byte) MembershipClient {
	return newClient(cred, url, realm, caBundle)
}

var _ MembershipClient = (*keycloak.Client)(nil)

// MembershipReconciler reconciles one GroupMembership CR into membership
// edges on a referenced Group. It owns only the members recorded in
// status.managedMembers and leaves out-of-band or peer-owned memberships alone.
type MembershipReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  record.EventRecorder
	Namespace string
	NewClient MembershipClientFactory
}

func (r *MembershipReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)
	defer func() { recordReconcile(kindMembership, retErr) }()

	membership := &keycloakv1alpha1.GroupMembership{}
	if err := r.Get(ctx, req.NamespacedName, membership); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !membership.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, membership)
	}

	if controllerutil.AddFinalizer(membership, membershipFinalizer) {
		if err := r.Update(ctx, membership); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueImmediately}, nil
	}

	return r.reconcileNormal(ctx, logger, membership)
}

func (r *MembershipReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, membership *keycloakv1alpha1.GroupMembership) (ctrl.Result, error) {
	instance, result, err := r.resolveInstance(ctx, membership)
	if instance == nil || err != nil {
		return result, err
	}

	group, result, err := r.resolveGroup(ctx, membership)
	if group == nil || err != nil {
		return result, err
	}
	if !sameInstanceRef(membership.Namespace, membership.Spec.InstanceRef, group.Namespace, group.Spec.InstanceRef) {
		message := fmt.Sprintf("membership instanceRef does not match referenced Group %s/%s instanceRef", group.Namespace, group.Name)
		return r.reject(ctx, membership, ReasonInstanceMismatch, message)
	}

	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, membership, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		return r.fail(ctx, membership, err, false)
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)
	remoteGroup, err := kc.GetGroupByPath(ctx, group.Spec.Path)
	recordKeycloakAPI(opGetGroupByPath, err)
	if err != nil {
		return r.fail(ctx, membership, fmt.Errorf("resolving Keycloak group %q: %w", group.Spec.Path, err), false)
	}
	beforeGroupID := membership.Status.GroupID

	changed, mutation, missing, err := r.reconcileMembers(ctx, kc, membership, beforeGroupID, remoteGroup.ID)
	if err != nil {
		if changed || mutation.Mutated {
			membership.Status.GroupID = remoteGroup.ID
		}
		if mutation.Mutated {
			r.stampMutation(membership, mutation.HealedDrift)
		}
		return r.fail(ctx, membership, err, changed || mutation.Mutated)
	}
	membership.Status.GroupID = remoteGroup.ID

	if len(missing) > 0 {
		if mutation.Mutated {
			r.stampMutation(membership, mutation.HealedDrift)
		}
		message := fmt.Sprintf("Keycloak users not found for member email(s): %s", strings.Join(missing, ", "))
		return r.memberNotFound(ctx, membership, message, changed || mutation.Mutated || beforeGroupID != remoteGroup.ID)
	}

	if mutation.Mutated {
		r.stampMutation(membership, mutation.HealedDrift)
	}
	now := metav1.Now()
	membership.Status.LastValidatedTime = &now

	extraChanged := true // Persist lastValidatedTime on every successful validation.
	message := fmt.Sprintf("reconciled Keycloak group membership for %s/%s", membership.Namespace, membership.Name)
	return r.succeed(ctx, logger, membership, ReasonReconciled, message, extraChanged)
}

type membershipMutation struct {
	Mutated     bool
	HealedDrift bool
}

func (r *MembershipReconciler) reconcileMembers(ctx context.Context, kc MembershipClient, membership *keycloakv1alpha1.GroupMembership, previousGroupID, groupID string) (changed bool, mutation membershipMutation, missing []string, retErr error) {
	managed := managedMembersByEmail(membership.Status.ManagedMembers)
	defer func() {
		membership.Status.ManagedMembers = serializeManagedMembers(managed)
	}()

	desired := map[string]bool{}
	for _, member := range membership.Spec.Members {
		desired[member.Email] = true
	}

	for _, member := range membership.Spec.Members {
		user, err := kc.FindUserByEmail(ctx, member.Email)
		recordKeycloakAPI(opFindUserByEmail, err)
		if err != nil {
			return changed, mutation, missing, fmt.Errorf("looking up Keycloak user by email %q: %w", member.Email, err)
		}
		if user == nil {
			missing = append(missing, member.Email)
			continue
		}

		groups, err := kc.ListUserGroups(ctx, user.ID)
		recordKeycloakAPI(opListUserGroups, err)
		if err != nil {
			return changed, mutation, missing, fmt.Errorf("listing Keycloak groups for user %q: %w", member.Email, err)
		}
		if !containsGroupID(groups, groupID) {
			if managed[member.Email] == user.ID {
				mutation.HealedDrift = true
			}
			addErr := kc.AddUserToGroup(ctx, user.ID, groupID)
			recordKeycloakAPI(opAddUserToGroup, addErr)
			if addErr != nil {
				return changed, mutation, missing, fmt.Errorf("adding Keycloak user %q to group %q: %w", member.Email, groupID, addErr)
			}
			mutation.Mutated = true
		}
		if managed[member.Email] != user.ID {
			changed = true
		}
		managed[member.Email] = user.ID
	}

	for email, userID := range managed {
		if desired[email] {
			continue
		}
		if r.peerStillDesires(ctx, membership, email) {
			delete(managed, email)
			changed = true
			continue
		}
		if previousGroupID != "" && previousGroupID != groupID {
			delete(managed, email)
			changed = true
			continue
		}
		groups, err := kc.ListUserGroups(ctx, userID)
		recordKeycloakAPI(opListUserGroups, ignoreNotFound(err))
		if keycloak.IsNotFound(err) {
			delete(managed, email)
			changed = true
			continue
		}
		if err != nil {
			return changed, mutation, missing, fmt.Errorf("listing Keycloak groups for managed user %q: %w", email, err)
		}
		if containsGroupID(groups, groupID) {
			rmErr := kc.RemoveUserFromGroupIfMember(ctx, userID, groupID)
			recordKeycloakAPI(opRemoveUserFromGroup, rmErr)
			if rmErr != nil {
				return changed, mutation, missing, fmt.Errorf("removing Keycloak user %q from group %q: %w", email, groupID, rmErr)
			}
			mutation.Mutated = true
		}
		delete(managed, email)
		changed = true
	}

	sort.Strings(missing)
	return changed, mutation, missing, nil
}

func (r *MembershipReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, membership *keycloakv1alpha1.GroupMembership) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(membership, membershipFinalizer) {
		return ctrl.Result{}, nil
	}
	if membership.Spec.DeletionPolicy == keycloakv1alpha1.DeletionPolicyOrphan {
		r.Recorder.Event(membership, corev1.EventTypeNormal, ReasonReleased,
			fmt.Sprintf("orphaned GroupMembership %s/%s (deletionPolicy Orphan; no memberships removed from Keycloak)", membership.Namespace, membership.Name))
		logger.Info("orphaned GroupMembership", "namespace", membership.Namespace, "name", membership.Name)
		return r.removeFinalizer(ctx, membership)
	}
	if len(membership.Status.ManagedMembers) == 0 || membership.Status.GroupID == "" {
		return r.removeFinalizer(ctx, membership)
	}

	instance, result, err := r.resolveInstance(ctx, membership)
	if instance == nil {
		return result, err
	}
	cred, err := resolveCredential(ctx, r.APIReader, r.Namespace, instance.Spec.CredentialsSecretRef)
	if err != nil {
		return r.handleCredentialError(ctx, membership, err)
	}
	if err := keycloak.ValidateCABundle(instance.Spec.CABundle); err != nil {
		r.Recorder.Event(membership, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		return ctrl.Result{}, err
	}

	kc := r.NewClient(cred, instance.Spec.URL, instance.Spec.Realm, instance.Spec.CABundle)
	for _, member := range membership.Status.ManagedMembers {
		if r.peerStillDesires(ctx, membership, member.Email) {
			continue
		}
		groups, err := kc.ListUserGroups(ctx, member.UserID)
		recordKeycloakAPI(opListUserGroups, ignoreNotFound(err))
		if keycloak.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("listing Keycloak groups for managed user %q during finalization: %w", member.Email, err)
		}
		if !containsGroupID(groups, membership.Status.GroupID) {
			continue
		}
		rmErr := kc.RemoveUserFromGroupIfMember(ctx, member.UserID, membership.Status.GroupID)
		recordKeycloakAPI(opRemoveUserFromGroup, rmErr)
		if rmErr != nil {
			return ctrl.Result{}, fmt.Errorf("removing Keycloak user %q from group %q during finalization: %w", member.Email, membership.Status.GroupID, rmErr)
		}
	}

	r.Recorder.Event(membership, corev1.EventTypeNormal, ReasonReleased,
		fmt.Sprintf("released GroupMembership %s/%s", membership.Namespace, membership.Name))
	logger.Info("released GroupMembership", "namespace", membership.Namespace, "name", membership.Name)
	return r.removeFinalizer(ctx, membership)
}

func (r *MembershipReconciler) resolveInstance(ctx context.Context, membership *keycloakv1alpha1.GroupMembership) (*keycloakv1alpha1.Instance, ctrl.Result, error) {
	ref := membership.Spec.InstanceRef
	instanceNamespace := ref.Namespace
	if instanceNamespace == "" {
		instanceNamespace = membership.Namespace
	}

	if instanceNamespace != membership.Namespace {
		allowed, err := referencegrant.Allowed(ctx, r.Client,
			referencegrant.FromRef{Group: keycloakv1alpha1.GroupVersion.Group, Kind: "GroupMembership", Namespace: membership.Namespace},
			referencegrant.ToRef{Group: keycloakv1alpha1.GroupVersion.Group, Kind: "Instance", Namespace: instanceNamespace, Name: ref.Name},
		)
		if err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("checking ReferenceGrant for Instance %s/%s: %w", instanceNamespace, ref.Name, err)
		}
		if !allowed {
			message := fmt.Sprintf("cross-namespace reference to Instance %s/%s is not authorized by a security.holos.run ReferenceGrant", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, membership, ReasonReferenceNotGranted, message)
			return nil, result, rerr
		}
	}

	instance := &keycloakv1alpha1.Instance{}
	key := types.NamespacedName{Namespace: instanceNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, instance); err != nil {
		if apierrors.IsNotFound(err) {
			message := fmt.Sprintf("referenced Instance %s/%s does not exist", instanceNamespace, ref.Name)
			result, rerr := r.notReady(ctx, membership, ReasonInstanceNotReady, message)
			return nil, result, rerr
		}
		return nil, ctrl.Result{}, fmt.Errorf("resolving Instance %s/%s: %w", instanceNamespace, ref.Name, err)
	}
	if !instanceReady(instance) {
		message := fmt.Sprintf("referenced Instance %s/%s is not Ready", instanceNamespace, ref.Name)
		result, rerr := r.notReady(ctx, membership, ReasonInstanceNotReady, message)
		return nil, result, rerr
	}
	return instance, ctrl.Result{}, nil
}

func (r *MembershipReconciler) resolveGroup(ctx context.Context, membership *keycloakv1alpha1.GroupMembership) (*keycloakv1alpha1.Group, ctrl.Result, error) {
	groupNS := membership.Spec.GroupRef.Namespace
	if groupNS == "" {
		groupNS = membership.Namespace
	}
	if groupNS != membership.Namespace {
		allowed, err := referencegrant.Allowed(ctx, r.Client,
			referencegrant.FromRef{Group: keycloakv1alpha1.GroupVersion.Group, Kind: "GroupMembership", Namespace: membership.Namespace},
			referencegrant.ToRef{Group: keycloakv1alpha1.GroupVersion.Group, Kind: "Group", Namespace: groupNS, Name: membership.Spec.GroupRef.Name},
		)
		if err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("checking ReferenceGrant for Group %s/%s: %w", groupNS, membership.Spec.GroupRef.Name, err)
		}
		if !allowed {
			message := fmt.Sprintf("cross-namespace reference to Group %s/%s is not authorized by a security.holos.run ReferenceGrant", groupNS, membership.Spec.GroupRef.Name)
			result, rerr := r.notReady(ctx, membership, ReasonReferenceNotGranted, message)
			return nil, result, rerr
		}
	}

	group := &keycloakv1alpha1.Group{}
	key := types.NamespacedName{Namespace: groupNS, Name: membership.Spec.GroupRef.Name}
	if err := r.Get(ctx, key, group); err != nil {
		if apierrors.IsNotFound(err) {
			message := fmt.Sprintf("referenced Group %s/%s does not exist", groupNS, membership.Spec.GroupRef.Name)
			result, rerr := r.notReady(ctx, membership, ReasonGroupNotReady, message)
			return nil, result, rerr
		}
		return nil, ctrl.Result{}, fmt.Errorf("resolving Group %s/%s: %w", groupNS, membership.Spec.GroupRef.Name, err)
	}
	if !groupReady(group) {
		message := fmt.Sprintf("referenced Group %s/%s is not Ready", groupNS, membership.Spec.GroupRef.Name)
		result, rerr := r.notReady(ctx, membership, ReasonGroupNotReady, message)
		return nil, result, rerr
	}
	return group, ctrl.Result{}, nil
}

func groupReady(group *keycloakv1alpha1.Group) bool {
	return ctrlshared.GenerationReady(group.Status.Conditions, ConditionReady, group.Generation)
}

func sameInstanceRef(leftNS string, left keycloakv1alpha1.InstanceReference, rightNS string, right keycloakv1alpha1.InstanceReference) bool {
	lns := left.Namespace
	if lns == "" {
		lns = leftNS
	}
	rns := right.Namespace
	if rns == "" {
		rns = rightNS
	}
	return left.Name == right.Name && lns == rns
}

func managedMembersByEmail(entries []keycloakv1alpha1.ManagedGroupMember) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		if e.Email == "" || e.UserID == "" {
			continue
		}
		out[e.Email] = e.UserID
	}
	return out
}

func serializeManagedMembers(managed map[string]string) []keycloakv1alpha1.ManagedGroupMember {
	if len(managed) == 0 {
		return nil
	}
	emails := make([]string, 0, len(managed))
	for email := range managed {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	out := make([]keycloakv1alpha1.ManagedGroupMember, 0, len(emails))
	for _, email := range emails {
		out = append(out, keycloakv1alpha1.ManagedGroupMember{Email: email, UserID: managed[email]})
	}
	return out
}

func containsGroupID(groups []keycloak.Group, groupID string) bool {
	for _, g := range groups {
		if g.ID == groupID {
			return true
		}
	}
	return false
}

func (r *MembershipReconciler) peerStillDesires(ctx context.Context, membership *keycloakv1alpha1.GroupMembership, email string) bool {
	var list keycloakv1alpha1.GroupMembershipList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing peer GroupMemberships before prune")
		return true
	}
	for i := range list.Items {
		peer := &list.Items[i]
		if peer.Namespace == membership.Namespace && peer.Name == membership.Name {
			continue
		}
		if !peer.DeletionTimestamp.IsZero() {
			continue
		}
		if !sameMembershipTarget(membership, peer) {
			continue
		}
		if !r.membershipReferencesAuthorized(ctx, peer) {
			continue
		}
		for _, member := range peer.Spec.Members {
			if member.Email == email {
				return true
			}
		}
	}
	return false
}

func sameMembershipTarget(a, b *keycloakv1alpha1.GroupMembership) bool {
	return sameInstanceRef(a.Namespace, a.Spec.InstanceRef, b.Namespace, b.Spec.InstanceRef) &&
		defaultedGroupRef(a.Namespace, a.Spec.GroupRef) == defaultedGroupRef(b.Namespace, b.Spec.GroupRef)
}

func defaultedGroupRef(namespace string, ref keycloakv1alpha1.GroupReference) string {
	ns := ref.Namespace
	if ns == "" {
		ns = namespace
	}
	return ns + "/" + ref.Name
}

func (r *MembershipReconciler) membershipReferencesAuthorized(ctx context.Context, membership *keycloakv1alpha1.GroupMembership) bool {
	if !r.referenceAuthorized(ctx, membership.Namespace, "Instance", defaultedInstanceNamespace(membership.Namespace, membership.Spec.InstanceRef), membership.Spec.InstanceRef.Name) {
		return false
	}
	return r.referenceAuthorized(ctx, membership.Namespace, "Group", defaultedGroupNamespace(membership.Namespace, membership.Spec.GroupRef), membership.Spec.GroupRef.Name)
}

func (r *MembershipReconciler) referenceAuthorized(ctx context.Context, fromNamespace, toKind, toNamespace, toName string) bool {
	if toNamespace == fromNamespace {
		return true
	}
	allowed, err := referencegrant.Allowed(ctx, r.Client,
		referencegrant.FromRef{Group: keycloakv1alpha1.GroupVersion.Group, Kind: "GroupMembership", Namespace: fromNamespace},
		referencegrant.ToRef{Group: keycloakv1alpha1.GroupVersion.Group, Kind: toKind, Namespace: toNamespace, Name: toName},
	)
	if err != nil {
		log.FromContext(ctx).Error(err, "checking peer GroupMembership ReferenceGrant", "fromNamespace", fromNamespace, "toKind", toKind, "toNamespace", toNamespace, "toName", toName)
		return false
	}
	return allowed
}

func defaultedInstanceNamespace(namespace string, ref keycloakv1alpha1.InstanceReference) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return namespace
}

func defaultedGroupNamespace(namespace string, ref keycloakv1alpha1.GroupReference) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return namespace
}

func (r *MembershipReconciler) stampMutation(membership *keycloakv1alpha1.GroupMembership, healedDrift bool) {
	now, reason, drift := ctrlshared.MutationStamp(membership.Status.ObservedGeneration, membership.Generation, membershipReady(membership), healedDrift)
	membership.Status.LastMutatedTime = &now
	membership.Status.LastMutationReason = keycloakv1alpha1.MutationReason(reason)
	if drift {
		membership.Status.LastDriftTime = &now
	}
}

func membershipReady(membership *keycloakv1alpha1.GroupMembership) bool {
	return ctrlshared.GenerationReady(membership.Status.Conditions, ConditionReady, membership.Generation)
}

func (r *MembershipReconciler) succeed(ctx context.Context, logger logr.Logger, membership *keycloakv1alpha1.GroupMembership, reason, message string, extraChanged bool) (ctrl.Result, error) {
	changed := markReady(&membership.Status.Conditions, reason, message, membership.Generation)
	changed = changed || extraChanged || membership.Status.ObservedGeneration != membership.Generation
	if !changed {
		return ctrl.Result{RequeueAfter: membershipResync}, nil
	}
	r.Recorder.Event(membership, corev1.EventTypeNormal, reason, message)
	logger.Info("reconciled GroupMembership", "namespace", membership.Namespace, "name", membership.Name, "reason", reason)
	if err := r.updateStatus(ctx, membership); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: membershipResync}, nil
}

func (r *MembershipReconciler) notReady(ctx context.Context, membership *keycloakv1alpha1.GroupMembership, reason, message string) (ctrl.Result, error) {
	if changed := markNotReady(&membership.Status.Conditions, reason, message, membership.Generation); changed {
		r.Recorder.Event(membership, corev1.EventTypeWarning, reason, message)
		if err := r.updateStatus(ctx, membership); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueDependency}, nil
}

func (r *MembershipReconciler) reject(ctx context.Context, membership *keycloakv1alpha1.GroupMembership, reason, message string) (ctrl.Result, error) {
	if changed := markNotReady(&membership.Status.Conditions, reason, message, membership.Generation); changed || membership.Status.ObservedGeneration != membership.Generation {
		r.Recorder.Event(membership, corev1.EventTypeWarning, reason, message)
		if err := r.updateStatus(ctx, membership); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *MembershipReconciler) memberNotFound(ctx context.Context, membership *keycloakv1alpha1.GroupMembership, message string, extraChanged bool) (ctrl.Result, error) {
	changed := markNotReady(&membership.Status.Conditions, ReasonMemberNotFound, message, membership.Generation)
	changed = changed || extraChanged || membership.Status.ObservedGeneration != membership.Generation
	if changed {
		r.Recorder.Event(membership, corev1.EventTypeWarning, ReasonMemberNotFound, message)
		if err := r.updateStatus(ctx, membership); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueDependency}, nil
}

func (r *MembershipReconciler) handleCredentialError(ctx context.Context, membership *keycloakv1alpha1.GroupMembership, err error) (ctrl.Result, error) {
	if !isMissingCredential(err) {
		return ctrl.Result{}, err
	}
	if changed := markNotReady(&membership.Status.Conditions, ReasonCredentialsNotFound, err.Error(), membership.Generation); changed {
		r.Recorder.Event(membership, corev1.EventTypeWarning, ReasonCredentialsNotFound, err.Error())
		if statusErr := r.updateStatus(ctx, membership); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return ctrl.Result{}, err
}

func (r *MembershipReconciler) fail(ctx context.Context, membership *keycloakv1alpha1.GroupMembership, err error, extraChanged bool) (ctrl.Result, error) {
	changed := markNotReady(&membership.Status.Conditions, ReasonKeycloakError, err.Error(), membership.Generation)
	if changed || extraChanged {
		r.Recorder.Event(membership, corev1.EventTypeWarning, ReasonKeycloakError, err.Error())
		if statusErr := r.updateStatus(ctx, membership); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after Keycloak error")
		}
	}
	return ctrl.Result{}, err
}

func (r *MembershipReconciler) removeFinalizer(ctx context.Context, membership *keycloakv1alpha1.GroupMembership) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(membership, membershipFinalizer)
	if err := r.Update(ctx, membership); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *MembershipReconciler) updateStatus(ctx context.Context, membership *keycloakv1alpha1.GroupMembership) error {
	base := membership.DeepCopy()
	membership.Status.ObservedGeneration = membership.Generation
	return ctrlshared.PatchStatus(ctx, r.Client, base, membership, "GroupMembership")
}

func (r *MembershipReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keycloakgroupmembership-controller")
	}
	if r.Namespace == "" {
		r.Namespace = controllerNamespace()
	}
	if r.NewClient == nil {
		r.NewClient = NewKeycloakMembershipClient
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.GroupMembership{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&keycloakv1alpha1.Instance{}, handler.EnqueueRequestsFromMapFunc(r.membershipsForInstance)).
		Watches(&keycloakv1alpha1.Group{}, handler.EnqueueRequestsFromMapFunc(r.membershipsForGroup)).
		Complete(r)
}

func (r *MembershipReconciler) membershipsForInstance(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	instance, ok := obj.(*keycloakv1alpha1.Instance)
	if !ok {
		return nil
	}
	var memberships keycloakv1alpha1.GroupMembershipList
	if err := r.List(ctx, &memberships); err != nil {
		log.FromContext(ctx).Error(err, "listing GroupMemberships to map an Instance change")
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range memberships.Items {
		m := &memberships.Items[i]
		refNamespace := m.Spec.InstanceRef.Namespace
		if refNamespace == "" {
			refNamespace = m.Namespace
		}
		if m.Spec.InstanceRef.Name == instance.Name && refNamespace == instance.Namespace {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
		}
	}
	return requests
}

func (r *MembershipReconciler) membershipsForGroup(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	group, ok := obj.(*keycloakv1alpha1.Group)
	if !ok {
		return nil
	}
	var memberships keycloakv1alpha1.GroupMembershipList
	if err := r.List(ctx, &memberships); err != nil {
		log.FromContext(ctx).Error(err, "listing GroupMemberships to map a Group change")
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range memberships.Items {
		m := &memberships.Items[i]
		refNamespace := m.Spec.GroupRef.Namespace
		if refNamespace == "" {
			refNamespace = m.Namespace
		}
		if m.Spec.GroupRef.Name == group.Name && refNamespace == group.Namespace {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
		}
	}
	return requests
}

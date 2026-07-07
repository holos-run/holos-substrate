package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KeycloakGroupReference names the KeycloakGroup whose remote membership this
// binding manages. An omitted namespace defaults to the membership CR's
// namespace. Cross-namespace references are authorized by a security.holos.run
// ReferenceGrant in the KeycloakGroup's namespace.
type KeycloakGroupReference struct {
	// Name is the metadata.name of the KeycloakGroup resource.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the KeycloakGroup. When omitted the group is resolved in the
	// membership CR's namespace. When set to a different namespace, the reference
	// must be authorized by a security.holos.run ReferenceGrant in the group's
	// namespace.
	//
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// KeycloakGroupMembershipMember declares one Keycloak user, by email, that
// should be a member of the referenced group. Users are provisioned separately by
// KeycloakUser; this Kind never creates users.
type KeycloakGroupMembershipMember struct {
	// Email is the Keycloak user's email address.
	//
	// +kubebuilder:validation:MinLength=1
	Email string `json:"email"`
}

// ManagedKeycloakGroupMember records one remote group membership this CR owns.
// The email ties the status entry back to spec.members, while userID lets the
// reconciler safely prune only the exact Keycloak user it previously added.
type ManagedKeycloakGroupMember struct {
	// Email is the member email from spec.members.
	//
	// +kubebuilder:validation:MinLength=1
	Email string `json:"email"`

	// UserID is the immutable Keycloak UUID of the resolved user.
	//
	// +kubebuilder:validation:MinLength=1
	UserID string `json:"userID"`
}

// KeycloakGroupMembershipSpec defines the desired state of one membership
// binding: a single target KeycloakGroup plus the users this CR should ensure are
// members of that group.
type KeycloakGroupMembershipSpec struct {
	// InstanceRef references the KeycloakInstance this membership is reconciled
	// against. It is immutable. The reconciler also requires the referenced
	// KeycloakGroup's instanceRef, after namespace defaulting, to match this value
	// exactly; a mismatch is Ready=False with reason InstanceMismatch and no
	// Keycloak mutation.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceRef is immutable"
	InstanceRef KeycloakInstanceReference `json:"instanceRef"`

	// GroupRef names the KeycloakGroup whose membership this CR manages. It is
	// immutable. An omitted namespace defaults to this CR's namespace; a
	// cross-namespace reference requires a security.holos.run ReferenceGrant in
	// the group's namespace.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="groupRef is immutable"
	GroupRef KeycloakGroupReference `json:"groupRef"`

	// Members is the desired set of Keycloak users, by email, this CR should add
	// to the referenced group. A member email with no corresponding Keycloak user
	// reports Ready=False with reason MemberNotFound; users are not created here.
	//
	// +optional
	// +listType=map
	// +listMapKey=email
	Members []KeycloakGroupMembershipMember `json:"members,omitempty"`

	// DeletionPolicy controls how the controller handles the group memberships
	// this resource manages when the resource is deleted. Delete removes the
	// managed memberships unless another membership resource still declares the
	// same user for the same group. Orphan leaves all Keycloak memberships
	// untouched. When omitted, the controller removes managed memberships, matching
	// Delete.
	//
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// KeycloakGroupMembershipStatus defines the observed state of a
// KeycloakGroupMembership. It follows the Gateway-API status convention and the
// ADR-22 drift-observability timestamp contract for external-resource CRs.
type KeycloakGroupMembershipStatus struct {
	// Conditions represent the latest available observations of the membership
	// binding's state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// KeycloakGroupMembership by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// GroupID is the immutable Keycloak UUID of the referenced group at the time
	// this membership was reconciled. It lets pruning and finalization verify
	// they are still touching the same remote group, not a replacement at the same
	// path.
	//
	// +optional
	GroupID string `json:"groupID,omitempty"`

	// ManagedMembers records the remote memberships this CR has added and may
	// later prune. The list is controller-owned; users should not edit it. Entries
	// are structured so the reconciler can match desired email identity to the
	// exact remote Keycloak user UUID without delimiter parsing.
	//
	// +optional
	// +listType=map
	// +listMapKey=email
	ManagedMembers []ManagedKeycloakGroupMember `json:"managedMembers,omitempty"`

	// LastValidatedTime is the last time the controller successfully read
	// Keycloak and confirmed or restored the declared membership state. It is not
	// advanced on failed remote reads or failed verification, so stale values
	// remain visible.
	//
	// +optional
	LastValidatedTime *metav1.Time `json:"lastValidatedTime,omitempty"`

	// LastMutatedTime is the last time the controller actually changed Keycloak
	// for this membership binding, such as adding or removing a member.
	//
	// +optional
	LastMutatedTime *metav1.Time `json:"lastMutatedTime,omitempty"`

	// LastMutationReason classifies the cause of the last remote mutation. It is
	// written together with lastMutatedTime.
	//
	// +optional
	// +kubebuilder:validation:Enum=SpecChange;DriftRemediation
	LastMutationReason MutationReason `json:"lastMutationReason,omitempty"`

	// LastDriftTime is the last time the controller remediated out-of-band drift.
	// It is set with LastMutationReason=DriftRemediation and preserved across
	// later spec-driven mutations.
	//
	// +optional
	LastDriftTime *metav1.Time `json:"lastDriftTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,keycloak}
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.groupRef.name`
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Validated",type=date,priority=1,JSONPath=`.status.lastValidatedTime`

// KeycloakGroupMembership is the Schema for the keycloakgroupmemberships API.
// It manages the members this CR declares for one KeycloakGroup.
type KeycloakGroupMembership struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeycloakGroupMembershipSpec   `json:"spec,omitempty"`
	Status KeycloakGroupMembershipStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeycloakGroupMembershipList contains a list of KeycloakGroupMembership.
type KeycloakGroupMembershipList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeycloakGroupMembership `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KeycloakGroupMembership{}, &KeycloakGroupMembershipList{})
}

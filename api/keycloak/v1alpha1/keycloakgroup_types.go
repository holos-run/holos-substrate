package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClientRoleReference names a client role a group confers on its members: the
// role named by Role scoped to a Keycloak client. A member of a group carrying
// this reference holds that client role, which reaches the named client's token
// via that client's per-client client-role mapper (ADR-20). It is shared by
// KeycloakGroup (the roles a group confers) and KeycloakClient (the roles a
// client defines).
//
// The target client is named by EXACTLY ONE of ClientRef (a same-namespace
// KeycloakClient CR) or ClientID (a Keycloak clientId directly). The reconciler is
// transparent (HOL-1421): a reference may name ANY client and confer ANY role name
// — the controller reserves and refuses nothing on org-policy grounds. ClientID is
// the path for conferring a role on a client that has no same-namespace
// KeycloakClient CR (e.g. a platform client such as the in-cluster Quay client
// https://quay.holos.internal), so the role surfaces in that client's token via its
// per-client client-role mapper (ADR-20).
//
// +kubebuilder:validation:XValidation:rule="has(self.clientRef) != has(self.clientId)",message="exactly one of clientRef or clientId must be set"
type ClientRoleReference struct {
	// ClientRef is the metadata.name of the KeycloakClient resource the role is
	// scoped to — a Kubernetes object name, not the client's URL-shaped clientId.
	// The reconciler resolves the named KeycloakClient CR in the referring
	// resource's namespace and derives the Keycloak clientId from its
	// spec.clientId, mirroring how a Repository resolves its OrganizationRef to an
	// Organization's spec.name (api/quay/v1alpha1). This keeps the reference a
	// valid object name even though the underlying Keycloak clientId is a URL.
	// Mutually exclusive with ClientID.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	ClientRef string `json:"clientRef,omitempty"`

	// ClientID names the target Keycloak clientId directly (e.g.
	// https://quay.holos.internal), bypassing same-namespace KeycloakClient CR
	// resolution. It is the mechanism for conferring a role on a client that has no
	// same-namespace KeycloakClient CR (such as a platform client). It may name any
	// client and confer any role name — the controller reserves nothing (HOL-1421).
	// Mutually exclusive with ClientRef.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientId,omitempty"`

	// Role is the client role name (e.g. owner, editor, viewer — the primitive
	// triad).
	//
	// +kubebuilder:validation:MinLength=1
	Role string `json:"role"`
}

// CustodianReference names a custodian group that may manage the membership of
// the group declaring it (ADR-3's custodian-approved membership model, ADR-20).
// Members of the referenced custodian group are delegated management of the
// declaring group's membership (e.g. via Keycloak Fine-Grained Admin
// Permissions v2 manage-members/manage-membership group scope). The referent is
// a Keycloak group path; this API group does not depend on any Keycloak type.
type CustodianReference struct {
	// Path is the Keycloak group path of the custodian group whose members may
	// manage the declaring group's membership (e.g.
	// projects/my-project/custodians/owner).
	//
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
}

// KeycloakGroupSpec defines the desired state of a KeycloakGroup: a (possibly
// nested) Keycloak group, the client roles its members hold, and the custodian
// group(s) that may manage its membership (ADR-20). Keycloak realm groups are a
// single global namespace while this CR is Kubernetes-namespaced, so ownership
// is tracked via a durable claim marker on status (mirroring ADR-19's claim
// model).
type KeycloakGroupSpec struct {
	// Path is the group's full Keycloak group path (e.g.
	// projects/my-project/roles/owner). It is the group's durable identity and is
	// immutable: renaming it would target a different Keycloak group, breaking the
	// ownership claim. Use a nested path for the idiomatic
	// projects/<project>/roles/{owner,editor,viewer} layout.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="path is immutable"
	Path string `json:"path"`

	// InstanceRef references the KeycloakInstance this group is provisioned in. A
	// cross-namespace reference (Namespace set to a different namespace) is gated
	// by a security.holos.run ReferenceGrant in the instance's namespace.
	InstanceRef KeycloakInstanceReference `json:"instanceRef"`

	// ClientRoles optionally lists the client roles every member of this group
	// holds. Each entry names its target client by EXACTLY ONE of clientRef (a
	// same-namespace KeycloakClient CR) or clientId (a Keycloak clientId directly —
	// the ADR-20 "Quay use case", for the platform-reserved Quay client where no
	// tenant CR exists), plus the role name. A member of the group thereby holds the
	// named client roles, which reach the target client's token via that client's
	// per-client client-role mapper. The direct clientId path is tightly bounded
	// (only the Quay client, only this role group's own project-prefixed role) so it
	// cannot confer a foreign or platform role. Cross-service relying parties key on
	// the group name in the shared groups claim, not on these roles.
	//
	// +optional
	// +listType=atomic
	ClientRoles []ClientRoleReference `json:"clientRoles,omitempty"`

	// Custodians optionally lists the custodian group(s) whose members may manage
	// this group's membership (ADR-3's custodian model). An empty or omitted list
	// means no delegated custodian management is configured for this group.
	//
	// +optional
	// +listType=atomic
	Custodians []CustodianReference `json:"custodians,omitempty"`

	// Adopt opts in to taking ownership of a pre-existing, externally-created
	// Keycloak group at the same path (the claim model, mirroring ADR-19's
	// Organization). Default false: a group this CR did not create and does not
	// already own is a Conflict (Ready=False, reason Conflict) and is never
	// silently seized — Keycloak realm groups are a single global namespace while
	// this CR is Kubernetes-namespaced, so without this guard a namespaced tenant
	// CR could take over a platform or another tenant's group by path. Set adopt:
	// true to deliberately claim such a group. When this resource is deleted, an
	// adopted group is released unless deletionPolicy is Delete.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// DeletionPolicy controls how the controller handles the Keycloak group when
	// this resource is deleted. Delete removes the group after verifying the live
	// group still has the UUID recorded in status. Orphan leaves the group and all
	// controller-added Keycloak side effects untouched. When omitted, a group this
	// resource created is deleted, while an adopted group is released after
	// pruning the roles and custodian delegation added by this resource.
	//
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// KeycloakGroupStatus defines the observed state of a KeycloakGroup, following
// the Gateway-API status convention plus the durable ownership marker the claim
// model requires (mirroring ADR-19).
type KeycloakGroupStatus struct {
	// Conditions represent the latest available observations of the group's
	// state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// KeycloakGroup by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Created is the durable ownership marker of the claim model (mirroring
	// ADR-19): it records whether this CR created the Keycloak group (true) versus
	// adopted or has not yet provisioned one (false). It is the controller-managed
	// owner record persisted on the resource's own status so it survives
	// controller restarts; the finalizer deletes the Keycloak group only when
	// Created is true, never an adopted group.
	//
	// +optional
	Created bool `json:"created,omitempty"`

	// Adopted records whether this CR adopted a pre-existing Keycloak group at the
	// same path (spec.adopt: true) rather than creating it. An adopted group is
	// released, never deleted, on CR removal — so adoption is non-destructive to a
	// group the platform did not create. Created and Adopted are mutually
	// exclusive outcomes of the claim model.
	//
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// GroupID is the immutable Keycloak UUID of the group this CR provisioned or
	// adopted. It is the durable ownership identity the claim model verifies: a
	// Keycloak group path is reusable (a group can be deleted and a different one
	// recreated at the same path out of band), but its UUID is not, so before
	// reconciling an owned group or deleting it on finalization the controller
	// confirms the group currently at the path still carries this UUID. A mismatch
	// means the original group was replaced by a foreign group, which is treated as
	// a Conflict (and never deleted) rather than silently seized.
	//
	// +optional
	GroupID string `json:"groupID,omitempty"`

	// ManagedClientRoles records the client roles this CR has conferred on the
	// group, each an opaque controller-owned key encoding a (clientId, role) pair, so
	// a role dropped from spec.clientRoles is unassigned from the Keycloak group on
	// the next reconcile rather than left active (the conferral is reconciled to the
	// desired set, not add-only). The encoding is an internal detail and not a stable
	// API; do not parse these entries.
	//
	// +optional
	// +listType=atomic
	ManagedClientRoles []string `json:"managedClientRoles,omitempty"`

	// ManagedCustodians records the custodian group paths this CR has wired FGAP v2
	// delegation for, so a custodian dropped from spec.custodians has its
	// delegation policy and scope permission removed on the next reconcile rather
	// than left able to manage membership (the delegation is reconciled to the
	// desired set, not add-only).
	//
	// +optional
	// +listType=atomic
	ManagedCustodians []string `json:"managedCustodians,omitempty"`

	// LastValidatedTime is the last time the controller successfully read
	// Keycloak and confirmed or restored the declared group state. It is not
	// advanced on failed remote reads or failed verification, so stale values
	// remain visible.
	//
	// +optional
	LastValidatedTime *metav1.Time `json:"lastValidatedTime,omitempty"`

	// LastMutatedTime is the last time the controller actually changed Keycloak
	// for this group, such as creating the group, assigning or removing a client
	// role, or changing custodian delegation.
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
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Validated",type=date,priority=1,JSONPath=`.status.lastValidatedTime`

// KeycloakGroup is the Schema for the keycloakgroups API. It manages a (possibly
// nested) Keycloak group, the client roles its members hold, and the custodian
// group(s) that may manage its membership (ADR-20).
type KeycloakGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeycloakGroupSpec   `json:"spec,omitempty"`
	Status KeycloakGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeycloakGroupList contains a list of KeycloakGroup.
type KeycloakGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeycloakGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KeycloakGroup{}, &KeycloakGroupList{})
}

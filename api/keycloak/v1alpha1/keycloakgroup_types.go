package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClientRoleReference names a client role a group confers on its members: the
// role named by Role scoped to the KeycloakClient named by ClientRef. A member
// of a group carrying this reference holds that client role, which reaches the
// owning client's own token via that client's per-client client-role mapper
// (ADR-20). It is shared by KeycloakGroup (the roles a group confers) and
// KeycloakClient (the roles a client defines).
type ClientRoleReference struct {
	// ClientRef is the metadata.name of the KeycloakClient resource the role is
	// scoped to — a Kubernetes object name, not the client's URL-shaped clientId.
	// The reconciler resolves the named KeycloakClient CR in the referring
	// resource's namespace and derives the Keycloak clientId from its
	// spec.clientId, mirroring how a Repository resolves its OrganizationRef to an
	// Organization's spec.name (api/quay/v1alpha1). This keeps the reference a
	// valid object name even though the underlying Keycloak clientId is a URL.
	//
	// +kubebuilder:validation:MinLength=1
	ClientRef string `json:"clientRef"`

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
	// holds. Each entry is a (clientRef, role) pair. A member of the group thereby
	// holds the named client roles, which reach the owning client's own token via
	// that client's per-client client-role mapper. Cross-service relying parties
	// key on the group name in the shared groups claim, not on these roles.
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
	// adopted a pre-existing one (false). It is the controller-managed owner
	// record persisted on the resource's own status so it survives controller
	// restarts; the finalizer deletes the Keycloak group only when Created is
	// true, never an adopted group.
	//
	// +optional
	Created bool `json:"created,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,keycloak}
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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

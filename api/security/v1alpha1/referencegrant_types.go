package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReferenceGrantFrom describes trusted namespaces and kinds that may reference
// objects in the ReferenceGrant's own namespace. It mirrors the
// gateway.networking.k8s.io ReferenceGrantFrom shape: a referrer is permitted
// only if its group, kind, and namespace all match an entry here.
type ReferenceGrantFrom struct {
	// Group is the API group of the referrer. The empty string ("") matches the
	// Kubernetes core API group. The group is matched exactly.
	//
	// +kubebuilder:validation:MaxLength=253
	Group string `json:"group"`

	// Kind is the kind of the referrer (e.g. KeycloakInstance). It is matched
	// exactly and is required.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`

	// Namespace is the namespace the referrer must live in. Unlike upstream
	// Gateway API, the namespace is always required: a grant trusts a specific
	// source namespace, never "any namespace".
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
}

// ReferenceGrantTo describes the kinds of objects, in the ReferenceGrant's own
// namespace, that the trusted referrers in From may reference. It mirrors the
// gateway.networking.k8s.io ReferenceGrantTo shape: a target is permitted only
// if its group and kind match an entry here, and — when Name is set — its name
// matches too.
type ReferenceGrantTo struct {
	// Group is the API group of the target. The empty string ("") matches the
	// Kubernetes core API group. The group is matched exactly.
	//
	// +kubebuilder:validation:MaxLength=253
	Group string `json:"group"`

	// Kind is the kind of the target (e.g. Secret). It is matched exactly and is
	// required.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`

	// Name, when set, constrains the grant to a single named target object of
	// the given group/kind. When omitted (nil), the grant applies to all objects
	// of that group/kind in the namespace.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name *string `json:"name,omitempty"`
}

// ReferenceGrantSpec identifies a cross-namespace relationship that is trusted
// for the Holos PaaS. It mirrors gateway.networking.k8s.io ReferenceGrantSpec:
// it permits objects matching From, in their respective namespaces, to reference
// objects matching To in the ReferenceGrant's own namespace. A reference is
// allowed only if some grant in the target namespace pairs a matching From with
// a matching To.
type ReferenceGrantSpec struct {
	// From describes the trusted namespaces and kinds that may reference objects
	// matching To. At least one entry is required.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	From []ReferenceGrantFrom `json:"from"`

	// To describes the kinds, in this ReferenceGrant's own namespace, that the
	// trusted referrers in From may reference. At least one entry is required.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	To []ReferenceGrantTo `json:"to"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={holos}
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ReferenceGrant identifies kinds of resources in other namespaces that are
// trusted to reference the specified kinds of resources in the same namespace as
// the policy (ADR-22). It mirrors gateway.networking.k8s.io ReferenceGrant and,
// like its upstream counterpart, is statusless declarative policy: it has no
// dedicated reconciler and no status subresource — consuming controllers read it
// via the internal/referencegrant authorization helper.
type ReferenceGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of the ReferenceGrant. It is required: a
	// grant with no spec authorizes nothing, and omitting it would let an empty
	// grant bypass the from/to MinItems validation, so the API server rejects a
	// ReferenceGrant without a spec.
	//
	// +kubebuilder:validation:Required
	Spec ReferenceGrantSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ReferenceGrantList contains a list of ReferenceGrant.
type ReferenceGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReferenceGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReferenceGrant{}, &ReferenceGrantList{})
}

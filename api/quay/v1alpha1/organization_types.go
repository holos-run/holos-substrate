package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OrganizationSpec defines the desired state of a Quay Organization.
//
// The reconciler creates (or, per the ADR-19 claim model, adopts) the named
// Quay organization. The spec deliberately carries no repository list — a
// Repository is its own resource (ADR-19, AC #9) — and no Kargo/Argo CD
// coupling; the only external dependency is the Quay credential in
// CredentialsSecretRef.
type OrganizationSpec struct {
	// Name is the Quay organization name to create or adopt. It is immutable:
	// once set, the Quay org it binds to does not change. Defaults to the
	// resource's metadata.name when omitted.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// Email is the organization contact email. Quay requires every namespace
	// (user or organization) to have a unique email address.
	//
	// +kubebuilder:validation:MinLength=1
	Email string `json:"email"`

	// DisplayName is an optional human-friendly name for the organization.
	//
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// CredentialsSecretRef names the Secret holding the Quay superuser OAuth
	// Application credential the reconciler authenticates to Quay with. A
	// Secret named holos-controller-quay-creds in the holos-controller
	// namespace is the suggested convention, and the field defaults to that
	// name when omitted. The Secret carries the Quay API URL and token (keys
	// url, token, optional username). This is the resource's only
	// authentication dependency (AC #7); its material is created at runtime
	// and never committed (secret-handling guardrail).
	//
	// +optional
	// +kubebuilder:default={name: "holos-controller-quay-creds"}
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef,omitempty"`
}

// Condition types surfaced on Organization and Repository status. The vocabulary
// follows the Gateway API convention; condition semantics are built out by the
// reconcilers in later phases (HOL-1311, HOL-1312).
const (
	// ConditionReady reports whether the resource has been fully provisioned
	// in Quay.
	ConditionReady = "Ready"
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the desired state has been
	// programmed into Quay (Gateway-API Programmed).
	ConditionProgrammed = "Programmed"
)

// OrganizationStatus defines the observed state of an Organization. It follows
// the Gateway-API status convention: a slice of standard metav1.Conditions plus
// the observedGeneration the controller last reconciled.
type OrganizationStatus struct {
	// Conditions represent the latest available observations of the
	// organization's state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// Organization by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,quay}
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Organization is the Schema for the organizations API. It names and applies a
// Quay organization in the in-cluster registry (ADR-19).
type Organization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OrganizationSpec   `json:"spec,omitempty"`
	Status OrganizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OrganizationList contains a list of Organization.
type OrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Organization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Organization{}, &OrganizationList{})
}

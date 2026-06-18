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
//
// Scope note: this spec carries name, email, displayName, credentialsSecretRef,
// and adopt (the claim-model opt-in the HOL-1311 reconciler enforces). The
// remaining ADR-19 illustrative schema (access[] group→team bindings,
// allowRepositoryCreation, and the explicit organizationName-vs-metadata.name
// split) is deferred to the ADR-reconciliation phase (HOL-1314); adding it here
// would be unused surface ahead of the logic that consumes it. New fields are
// additive to this type, so deferring them causes no API break.
type OrganizationSpec struct {
	// Name is the Quay organization name to create or adopt. It is immutable:
	// once set, the Quay org it binds to does not change. It is required —
	// callers conventionally set it to the resource's metadata.name, but the
	// controller does not default it (no defaulting webhook in this scaffold).
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
	// Note: Quay 3.17.3 organizations have no display-name (or description)
	// field, so this value is accepted on the CR but is NOT programmed into Quay
	// — there is no org endpoint to apply it to. It is retained as forward-looking
	// API surface for a future Quay release (or an alternate registry) that gains
	// a display-name field. The contact Email, by contrast, IS mutable and is
	// reconciled to Quay on drift.
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

	// Adopt opts in to taking ownership of a pre-existing, externally-created
	// Quay organization of the same name (ADR-19's claim model). Default false:
	// an org this CR did not create and does not already own is a Conflict
	// (Ready=False, reason Conflict) and is never silently seized — the
	// controller's credential carries FEATURE_SUPERUSERS_FULL_ACCESS, so without
	// this guard a namespaced CR could take over another tenant's global Quay
	// org. Set adopt: true to deliberately claim such an org.
	//
	// An adopted org is released (the finalizer drops without deleting) rather
	// than deleted on CR removal, so adoption is non-destructive to a resource
	// the platform did not create.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`
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

	// Created is the durable ownership marker of ADR-19's claim model: it
	// records whether this CR created the Quay organization (true) versus
	// adopted a pre-existing one (false). It is the controller-managed owner
	// record the claim model requires, persisted on the resource's own status
	// so it survives controller restarts. The finalizer deletes the Quay org
	// only when Created is true; an adopted org is released, never deleted.
	//
	// +optional
	Created bool `json:"created,omitempty"`
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

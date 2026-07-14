package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InstanceSpec defines the desired state of an Instance: how to
// reach one Keycloak target and realm. It is the single, centrally-managed
// holder of the connection configuration the rest of the keycloak.holos.run
// Kinds reference via a InstanceReference (ADR-20). The design supports
// multiple instances per cluster (e.g. pre-prod and prod) and a target that is
// in-cluster, out-of-cluster, or in a remote cluster — each is a distinct
// Instance.
//
// Its only external coupling is the admin credential in CredentialsSecretRef
// plus the optional CABundle trust anchor — never a Keycloak client-library
// type import (ADR-20 dependency boundary).
type InstanceSpec struct {
	// URL is the Keycloak base/API URL the controller reaches the admin API at
	// (e.g. https://keycloak-service:8443 for the in-cluster instance, or an
	// external https URL for an out-of-cluster or remote-cluster target). It is
	// required and must be an absolute https URL: the controller authenticates to
	// this endpoint with the admin credential, so plaintext http is rejected at
	// admission rather than silently sending the credential in the clear.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://[^/?#\s:@]+(:[0-9]+)?(/[^\s]*)?$`
	URL string `json:"url"`

	// Realm is the Keycloak realm this instance targets (e.g. holos). It is
	// required. A Instance binds one realm; targeting a second realm on
	// the same Keycloak server is a second Instance.
	//
	// +kubebuilder:validation:MinLength=1
	Realm string `json:"realm"`

	// CredentialsSecretRef names the Secret holding the Keycloak admin credential
	// the controller authenticates to the admin API with. A Secret named
	// holos-controller-keycloak-creds in the holos-controller namespace is the
	// suggested convention, and the field defaults to that name when omitted. The
	// recommended auth is a confidential service-account client with
	// realm-management roles, or a realm user with realm-management. This is the
	// resource's only authentication dependency; its material is created at
	// runtime and never committed (secret-handling guardrail).
	//
	// +optional
	// +kubebuilder:default={name: "holos-controller-keycloak-creds"}
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef,omitempty"`

	// CABundle carries PEM-encoded x509 CA certificates the controller trusts in
	// addition to its system store when reaching the Keycloak admin API. Its
	// semantics and serialization are the shared "CABundle convention" documented
	// once in common_types.go: it follows the upstream Kubernetes caBundle
	// convention (PEM certs serialized as a single base64 string) and an empty
	// value uses the controller pod's system trust store unchanged. It is the
	// trust anchor for an in-cluster Keycloak signed by the platform's local CA.
	//
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`
}

// InstanceStatus defines the observed state of an Instance. It
// follows the Gateway-API status convention: a slice of standard
// metav1.Conditions plus the observedGeneration the controller last reconciled.
type InstanceStatus struct {
	// Conditions represent the latest available observations of the instance's
	// reachability and readiness.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// Instance by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastValidatedTime is the last time the controller successfully read
	// Keycloak and confirmed the target realm was reachable. It is not advanced
	// on failed remote reads or failed verification, so stale values remain
	// visible.
	//
	// +optional
	LastValidatedTime *metav1.Time `json:"lastValidatedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,keycloak}
// +kubebuilder:printcolumn:name="Realm",type=string,JSONPath=`.spec.realm`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Validated",type=date,priority=1,JSONPath=`.status.lastValidatedTime`

// Instance is the Schema for the instances API. It is the
// centrally-managed reference to one Keycloak target and realm the rest of the
// keycloak.holos.run resources reconcile against (ADR-20).
type Instance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InstanceSpec   `json:"spec,omitempty"`
	Status InstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InstanceList contains a list of Instance.
type InstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Instance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Instance{}, &InstanceList{})
}

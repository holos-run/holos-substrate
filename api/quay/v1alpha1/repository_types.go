package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RepositoryVisibility is the visibility of a Quay repository.
//
// +kubebuilder:validation:Enum=public;private
type RepositoryVisibility string

const (
	// RepositoryVisibilityPublic makes the repository world-readable.
	RepositoryVisibilityPublic RepositoryVisibility = "public"
	// RepositoryVisibilityPrivate restricts the repository to authorized
	// robots and users.
	RepositoryVisibilityPrivate RepositoryVisibility = "private"
)

// RepositoryWebhook configures a repo_push webhook on the Quay repository so a
// push notifies a downstream receiver (the Kargo Warehouse, ADR-16). The target
// URL is provided exactly one of two ways — inline via Url, or indirectly via
// UrlSecretRef when the URL is hard-to-guess and must not be committed. Exactly
// one of the two must be set.
//
// UrlSecretRef is distinct from the spec-level credentialsSecretRef: it points
// at a Secret in the resource's own namespace holding the webhook target URL,
// not the Quay API credential.
//
// +kubebuilder:validation:XValidation:rule="(has(self.url) ? 1 : 0) + (has(self.urlSecretRef) ? 1 : 0) == 1",message="exactly one of url or urlSecretRef must be set"
type RepositoryWebhook struct {
	// Url is the inline webhook target URL. Mutually exclusive with
	// UrlSecretRef; set exactly one. Must be non-empty when present so an
	// empty string is rejected at admission rather than failing later during
	// Quay webhook registration.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	Url *string `json:"url,omitempty"`

	// UrlSecretRef points at a Secret in the resource's namespace holding the
	// webhook target URL. Use it when the URL is hard-to-guess (e.g. the Kargo
	// receiver URL) and must not be committed. Mutually exclusive with Url; set
	// exactly one.
	//
	// +optional
	UrlSecretRef *WebhookURLSecretRef `json:"urlSecretRef,omitempty"`
}

// WebhookURLSecretRef references a Secret key in the resource's namespace that
// holds a webhook target URL.
type WebhookURLSecretRef struct {
	// Name of the Secret in the resource's namespace.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key within the Secret holding the URL value.
	//
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// RepositorySpec defines the desired state of a Quay Repository within an owning
// Organization. As with Organization, the spec's only external coupling is the
// Quay credential in CredentialsSecretRef plus the optional webhook URL — never
// a Kargo/Argo CD type import (AC #7).
type RepositorySpec struct {
	// OrganizationRef is the name of the owning Organization CR (and, through
	// it, the Quay organization) this repository is created within. The full
	// Quay path is <organization>/<name>.
	//
	// +kubebuilder:validation:MinLength=1
	OrganizationRef string `json:"organizationRef"`

	// Name is the repository name within the organization.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Visibility is the repository visibility, public or private.
	//
	// +kubebuilder:default=private
	Visibility RepositoryVisibility `json:"visibility,omitempty"`

	// Description is an optional human-friendly repository description.
	//
	// +optional
	Description string `json:"description,omitempty"`

	// CredentialsSecretRef names the Secret holding the Quay superuser OAuth
	// Application credential the reconciler authenticates to Quay with. A
	// Secret named holos-controller-quay-creds in the holos-controller
	// namespace is the suggested convention, and the field defaults to that
	// name when omitted. The Secret carries the Quay API URL and token (keys
	// url, token, optional username). This is the resource's only
	// authentication dependency (AC #7) and is distinct from the webhook
	// urlSecretRef; its material is created at runtime and never committed
	// (secret-handling guardrail).
	//
	// +optional
	// +kubebuilder:default={name: "holos-controller-quay-creds"}
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef,omitempty"`

	// Webhook optionally configures a repo_push webhook on the repository.
	//
	// +optional
	Webhook *RepositoryWebhook `json:"webhook,omitempty"`
}

// RepositoryStatus defines the observed state of a Repository, following the
// same Gateway-API status convention as Organization.
type RepositoryStatus struct {
	// Conditions represent the latest available observations of the
	// repository's state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// Repository by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,quay}
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.organizationRef`
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Repository is the Schema for the repositories API. It is a single repository
// within an owning Organization in the in-cluster Quay registry (ADR-19).
type Repository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RepositorySpec   `json:"spec,omitempty"`
	Status RepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository.
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Repository{}, &RepositoryList{})
}

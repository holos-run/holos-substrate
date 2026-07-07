package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RepositoryDescriptionMaxLength reserves space for the controller's appended
// ownership marker so the rendered Quay description never exceeds Quay's 4096
// character limit. The marker is "\n\nholos-owner: created:<uuid>" or
// "\n\nholos-owner: adopted:<uuid>".
const RepositoryDescriptionMaxLength = 4037

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

// RepositoryWebhook configures a repo_push notification on the Quay repository.
// The target URL is provided either inline or from a Secret in the Repository
// namespace. Exactly one source must be set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.url) ? 1 : 0) + (has(self.urlSecretRef) ? 1 : 0) == 1",message="exactly one of url or urlSecretRef must be set"
type RepositoryWebhook struct {
	// URL is the inline webhook target URL. It is optional, has no default, and
	// must be set only when urlSecretRef is omitted.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL *string `json:"url,omitempty"`

	// URLSecretRef selects a Secret key in the Repository namespace that contains
	// the webhook target URL. It is optional, has no default, and must be set only
	// when url is omitted.
	//
	// +optional
	URLSecretRef *WebhookURLSecretRef `json:"urlSecretRef,omitempty"`
}

// WebhookURLSecretRef references a Secret key in the resource's namespace that
// holds a webhook target URL.
type WebhookURLSecretRef struct {
	// Name is the Secret name in the Repository namespace. It is required and has
	// no default.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key is the Secret data key containing the URL. It is required and has no
	// default.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// RepositorySpec describes a Quay repository inside an Organization managed by
// this API group. The controller resolves the owning Quay organization through
// organizationRef and keeps repository visibility, description, and optional
// webhook configuration in sync.
type RepositorySpec struct {
	// OrganizationRef is the name of the owning Organization resource in this
	// namespace. It is required, immutable, and has no default. The controller
	// uses that Organization's spec.name as the Quay namespace instead of letting
	// this resource name any Quay organization directly.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="organizationRef is immutable"
	OrganizationRef string `json:"organizationRef"`

	// Name is the repository name within the resolved Quay organization. It is
	// required, immutable, and has no default.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-]+[a-z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// Visibility controls whether anonymous users may pull from the repository.
	// It is optional and defaults to private.
	//
	// +optional
	// +kubebuilder:default=private
	Visibility RepositoryVisibility `json:"visibility,omitempty"`

	// Description is a human-readable repository description shown by Quay. It is
	// optional, has no default, and does not affect access.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=4037
	Description string `json:"description,omitempty"`

	// Adopt opts in to claiming a pre-existing Quay repository with the same
	// name. It defaults to false; without this opt-in, an unowned existing
	// repository is reported as a conflict and is never silently managed. An
	// adopted repository is released, not deleted, when this resource is removed.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// DeletionPolicy controls what happens to the Quay repository when this
	// resource is deleted. Delete removes the repository from Quay after verifying
	// this resource still owns it. Orphan leaves the repository in place and
	// removes only this controller's ownership marker from the repository
	// description, keeping any recorded push webhook in place so a replacement
	// resource can adopt the repository later. When omitted, the behavior follows
	// how ownership was established: a repository this resource created is deleted,
	// and an adopted repository is released without being deleted.
	//
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// CredentialsSecretRef selects the controller-namespace Secret containing the
	// Quay API URL and OAuth token. When omitted, the controller uses
	// holos-controller-quay-creds. This is separate from webhook.urlSecretRef,
	// which points at a Secret in the Repository namespace.
	//
	// +optional
	// +kubebuilder:default={name: "holos-controller-quay-creds"}
	CredentialsSecretRef *SecretReference `json:"credentialsSecretRef,omitempty"`

	// Webhook configures an optional repo_push notification on the repository.
	// When omitted, the controller removes any webhook notification it owns.
	//
	// +optional
	Webhook *RepositoryWebhook `json:"webhook,omitempty"`

	// CABundle carries PEM-encoded x509 CA certificates the controller trusts
	// in addition to its system store when reaching the Quay API. The API server
	// serializes this byte slice as one base64 string. When omitted or empty, the
	// controller uses only its system trust store. The maximum length applies to
	// the base64-encoded API representation.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1048576
	CABundle []byte `json:"caBundle,omitempty"`
}

// RepositoryStatus reports what the controller most recently observed and
// changed in Quay for a Repository.
type RepositoryStatus struct {
	// Conditions are the current Accepted, Programmed, Ready, and
	// WebhookConfigured observations for the repository. They are omitted until
	// the controller has reconciled the resource.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the latest metadata.generation the controller has
	// reconciled. It is omitted until the first status write.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// QuayRepository is the resolved Quay repository path in the form
	// organization/name. The controller records it after provisioning so
	// finalization can delete the same remote repository even if the owning
	// Organization resource is later unavailable. It is omitted until a repository
	// has been provisioned.
	//
	// +optional
	QuayRepository string `json:"quayRepository,omitempty"`

	// Created records whether this resource created the Quay repository. True
	// means finalization may delete the repository. False means the resource
	// adopted an existing repository and finalization only releases it. When
	// omitted, the controller has not recorded ownership yet.
	//
	// +optional
	Created *bool `json:"created,omitempty"`

	// WebhookNotificationUUID is the Quay UUID of the repo_push webhook
	// notification this resource created. It is omitted when this resource has not
	// created a webhook. The controller records it as the primary deletion gate and
	// can recover it from the webhook's resource-specific title if a status write is
	// lost.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=128
	WebhookNotificationUUID string `json:"webhookNotificationUUID,omitempty"`

	// LastValidatedTime is the last time the controller successfully read Quay and
	// confirmed or restored the declared repository state. It is omitted until
	// the first successful validation.
	//
	// +optional
	LastValidatedTime *metav1.Time `json:"lastValidatedTime,omitempty"`

	// LastMutatedTime is the last time the controller actually changed Quay for
	// this repository, such as creating the repo, updating its visibility or
	// description, or reconciling its webhook. It is omitted until the first
	// remote mutation.
	//
	// +optional
	LastMutatedTime *metav1.Time `json:"lastMutatedTime,omitempty"`

	// LastMutationReason classifies why the last remote mutation happened. It is
	// written with lastMutatedTime and omitted when no mutation has been recorded.
	//
	// +optional
	LastMutationReason MutationReason `json:"lastMutationReason,omitempty"`

	// LastDriftTime is the last time the controller remediated out-of-band drift.
	// It is omitted until drift remediation occurs and is preserved across later
	// spec-driven mutations.
	//
	// +optional
	LastDriftTime *metav1.Time `json:"lastDriftTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,quay}
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.organizationRef`
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Validated",type=date,priority=1,JSONPath=`.status.lastValidatedTime`

// Repository declares a Quay repository in a managed Organization, created and
// kept in sync by the platform controller.
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

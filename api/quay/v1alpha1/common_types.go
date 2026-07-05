package v1alpha1

// DefaultCredentialsSecretName is the suggested and default name of the Secret
// holding the Quay superuser OAuth Application credential. When a spec's
// credentialsSecretRef is omitted, the controller resolves a Secret of this
// name in its own holos-controller namespace.
const DefaultCredentialsSecretName = "holos-controller-quay-creds"

// SecretReference selects the Secret the controller uses to authenticate to
// Quay. The Secret is resolved in the controller namespace and normally contains
// url, token, and optional username entries. When the whole reference is
// omitted, the controller uses holos-controller-quay-creds.
type SecretReference struct {
	// Name is the Secret name in the controller namespace. When omitted, it
	// defaults to holos-controller-quay-creds, the platform credential Secret
	// created at runtime.
	//
	// +optional
	// +kubebuilder:default=holos-controller-quay-creds
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`

	// Key overrides which Secret entry contains the Quay OAuth token. When
	// omitted, the controller reads token. The url and optional username entries
	// are always read from keys named url and username.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`
}

// MutationReason explains why the controller last changed Quay. It is optional
// on status because a resource may have been validated without any remote
// mutation.
//
// +kubebuilder:validation:Enum=SpecChange;DriftRemediation
type MutationReason string

const (
	// MutationReasonSpecChange means the controller changed Quay to match a new
	// desired spec generation.
	MutationReasonSpecChange MutationReason = "SpecChange"
	// MutationReasonDriftRemediation means the controller corrected out-of-band
	// drift while the CR's desired spec generation was unchanged.
	MutationReasonDriftRemediation MutationReason = "DriftRemediation"
)

// Condition types used by quay.holos.run resources. They follow the
// Gateway-API vocabulary so operators can interpret all resources consistently.
const (
	// ConditionAccepted reports whether the controller accepted the spec for
	// reconciliation.
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the requested Quay state has been
	// written.
	ConditionProgrammed = "Programmed"
	// ConditionReady reports whether the Quay resource is provisioned and usable.
	ConditionReady = "Ready"
)

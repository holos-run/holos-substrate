package v1alpha1

// DefaultCredentialsSecretName is the suggested and default name of the Secret
// holding the Quay superuser OAuth Application credential. When a spec's
// credentialsSecretRef is omitted, the controller resolves a Secret of this
// name in its own holos-controller namespace.
const DefaultCredentialsSecretName = "holos-controller-quay-creds"

// SecretReference names the Secret holding the Quay superuser OAuth Application
// credential (keys url, token, optional username). The controller resolves it
// in its own holos-controller namespace. Suggested/default name:
// holos-controller-quay-creds.
//
// This is the resource's only authentication dependency (ADR-19, AC #7): the
// custom resources reach Quay solely through the credential this Secret holds,
// never by importing a Quay (or Kargo/Argo CD) client type into the API group.
// It is distinct from the Repository webhook urlSecretRef, which points at a
// Secret holding a webhook target URL in the resource's own namespace.
type SecretReference struct {
	// Name of the Secret holding the Quay superuser OAuth credential. When
	// omitted it defaults to holos-controller-quay-creds, resolved in the
	// controller's holos-controller namespace.
	//
	// +optional
	// +kubebuilder:default=holos-controller-quay-creds
	Name string `json:"name,omitempty"`

	// Key within the Secret to read the credential from. The Secret carries
	// the Quay API URL and token under keys url, token, and an optional
	// username; key narrows resolution to a specific entry when set. When
	// omitted the controller reads the conventional url/token/username keys.
	//
	// +optional
	Key string `json:"key,omitempty"`
}

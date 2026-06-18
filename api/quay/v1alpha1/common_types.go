package v1alpha1

// CABundle convention (shared across all quay.holos.run Kinds).
//
// Every Kind in this API group (Organization, Repository) carries a CABundle
// []byte spec field with JSON tag `caBundle,omitempty`. Its semantics and
// serialization are standardized here, once, and each spec's field godoc refers
// back to this block rather than re-describing the format — so the field is
// generally re-used across Kinds.
//
// CABundle is a PEM-encoded set of x509 CA certificates the controller trusts
// in addition to its system store when establishing TLS to the Quay API. It
// follows the upstream Kubernetes caBundle convention: one or more PEM blocks
// concatenated, serialized as a single base64 string in JSON (the Go `[]byte`
// type marshals to a base64 string, and the generated CRD property is
// `type: string, format: byte`). An empty CABundle means use the controller
// pod's system trust store unchanged — the historical behavior — so the field
// is purely additive.
//
// It is the trust anchor for the in-cluster Quay registry, whose serving
// certificate is signed by the platform's local CA rather than a public root:
// without it the reconcilers hit `x509: certificate signed by unknown
// authority`. The bundle is configuration carried on the spec, not a
// credential — the Quay API token lives in the credential Secret
// (CredentialsSecretRef), the CA bundle does not.

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

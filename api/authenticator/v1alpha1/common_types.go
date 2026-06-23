package v1alpha1

// CABundle convention (shared across all authenticator.holos.run Kinds).
//
// Both the Backend's spec.server and spec.oidc carry a CABundle []byte field
// with JSON tag `caBundle,omitempty`. Its semantics and serialization are
// standardized here, once, and each field's godoc refers back to this block
// rather than re-describing the format — so the convention is re-used across
// fields and (future) Kinds. It mirrors the identical CABundle convention in the
// quay.holos.run API group (ADR-19).
//
// CABundle is a PEM-encoded set of x509 CA certificates the authorizer trusts in
// addition to its system store when establishing TLS to the referent endpoint
// (the upstream API server for spec.server, the OIDC issuer for spec.oidc). It
// follows the upstream Kubernetes caBundle convention: one or more PEM blocks
// concatenated, serialized as a single base64 string in JSON (the Go `[]byte`
// type marshals to a base64 string, and the generated CRD property is
// `type: string, format: byte`). An empty CABundle means use the authorizer
// pod's system trust store unchanged — so the field is purely additive.
//
// It is the trust anchor for an endpoint whose serving certificate is signed by
// a private CA rather than a public root (e.g. an in-cluster API server signed by
// the platform's local CA): without it the authorizer hits `x509: certificate
// signed by unknown authority`. The bundle is configuration carried on the spec,
// not a credential — the privileged API server credential lives in the
// credential Secret (CredentialsSecretRef), the CA bundle does not.

// DefaultCredentialsSecretName is the suggested and default name of the Secret
// holding the backend's privileged Kubernetes credential — the impersonator
// identity the authorizer authenticates to the upstream API server with. When a
// Backend's spec.credentialsSecretRef is omitted, the authorizer resolves a
// Secret of this name in the authorizer's own namespace.
const DefaultCredentialsSecretName = "holos-authenticator-backend-creds"

// SecretReference names the Secret holding the backend's privileged Kubernetes
// API server credential — the impersonator identity the authorizer authenticates
// to the upstream API server with after validating the end user's OIDC token and
// mapping its claims to groups. The authorizer resolves it in its own namespace.
// Suggested/default name: holos-authenticator-backend-creds.
//
// This is the resource's only authentication dependency: the Backend reaches the
// upstream API server solely through the credential this Secret holds, never by
// importing a Kubernetes client type into the API group. Per the Runtime Secret
// Handling guardrail, the Secret's material is created at runtime and never
// committed.
type SecretReference struct {
	// Name of the Secret holding the backend's privileged credential. When
	// omitted it defaults to holos-authenticator-backend-creds, resolved in the
	// authorizer's own namespace. MinLength=1 rejects an explicit empty string,
	// which would otherwise bypass the default and leave the resolver with a blank
	// Secret name.
	//
	// +optional
	// +kubebuilder:default=holos-authenticator-backend-creds
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name,omitempty"`

	// Key within the Secret to read the credential from. When omitted the
	// authorizer reads the conventional key(s) the credential is stored under.
	//
	// +optional
	Key string `json:"key,omitempty"`
}

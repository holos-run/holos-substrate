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

// DefaultImpersonatorServiceAccountName is the suggested and default name of the
// ServiceAccount a Backend's spec.serviceAccountRef mints a token for — the
// shipped impersonator identity. Like the credential Secret, the ServiceAccount
// is resolved in the authorizer's own namespace, never the Backend's namespace.
// When a Backend's spec.serviceAccountRef.name is omitted, the authorizer mints a
// token for a ServiceAccount of this name.
const DefaultImpersonatorServiceAccountName = "holos-authenticator-impersonator"

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

// ServiceAccountReference names a Kubernetes ServiceAccount the authorizer mints
// a short-lived token for (via the TokenRequest API) to obtain the backend's
// privileged credential — the impersonator identity it authenticates to the
// upstream API server with — as an alternative to a long-lived credential Secret
// (SecretReference). It is mutually exclusive with the Backend's
// credentialsSecretRef (enforced by a CRD CEL validation rule on BackendSpec).
//
// Like the credential Secret, the ServiceAccount is resolved in the authorizer's
// **own namespace**, never the Backend's namespace — the reference names only the
// ServiceAccount, not a namespace, mirroring the credentialsSecretRef resolution
// rule in internal/authenticator/credentials.go (ADR-23, mirroring ADR-19). The
// minted token is a Secret-equivalent credential created at runtime and never
// committed (Runtime Secret Handling guardrail).
//
// Phase note (HOL-1399): this phase ships the API type, defaults, and validation
// only. The authorizer still resolves the credential exclusively from
// credentialsSecretRef; the TokenRequest minting/caching/rotation that consumes
// serviceAccountRef lands in HOL-1400. The field is additive and
// backward-compatible.
type ServiceAccountReference struct {
	// Name of the ServiceAccount in the authorizer's own namespace the authorizer
	// mints a token for. When omitted it defaults to
	// holos-authenticator-impersonator (the shipped impersonator SA). MinLength=1
	// rejects an explicit empty string, which would otherwise bypass the default
	// and leave the resolver with a blank ServiceAccount name.
	//
	// +optional
	// +kubebuilder:default=holos-authenticator-impersonator
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name,omitempty"`

	// Audience requested for the minted token (the TokenRequest spec.audiences).
	// When empty the authorizer requests the API server's default audience (the
	// audience of the cluster the authorizer runs in), which is the common case for
	// impersonating against the local API server. Set it to target a token bound to
	// a specific audience.
	//
	// +optional
	Audience string `json:"audience,omitempty"`

	// ExpirationSeconds is the requested lifetime of the minted token (the
	// TokenRequest spec.expirationSeconds). The authorizer caches and rotates the
	// token within this lifetime. When omitted it defaults to 3600 (one hour). The
	// Minimum of 600 (ten minutes) rejects an impractically short lifetime that
	// would force near-continuous re-minting; the API server may still clamp the
	// effective expiry to its own configured bounds.
	//
	// +optional
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=600
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty"`
}

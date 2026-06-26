package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServerConfig describes the upstream Kubernetes API server a Backend fronts.
// The server may be in-cluster or external; the URL is matched to a request by
// the Backend's spec.host.
type ServerConfig struct {
	// URL is the upstream Kubernetes API server endpoint the authorizer forwards
	// authenticated requests to (e.g. https://api.example.com:6443). It may be
	// external to the cluster. Required.
	//
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// CABundle carries PEM-encoded x509 CA certificates the authorizer trusts in
	// addition to its system store when establishing TLS to the upstream API
	// server. Its semantics and serialization are the shared "CABundle
	// convention" documented once in common_types.go: PEM certs serialized as a
	// single base64 string, an empty value uses the pod's system trust store
	// unchanged. It is the trust anchor for an API server signed by a private CA
	// (e.g. the platform's local CA) rather than a public root.
	//
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`
}

// OIDCConfig is the single OIDC client a Backend validates end-user tokens with.
// There is exactly one OIDC client per backend.
type OIDCConfig struct {
	// IssuerURL is the OIDC issuer (the `iss` claim and OIDC discovery base URL,
	// e.g. https://keycloak.holos.internal/realms/holos). The authorizer fetches
	// the issuer's discovery document and JWKS to validate token signatures.
	// When JWKS is set, IssuerURL is instead the expected `iss` claim value only
	// and no discovery document/JWKS is fetched: token signatures are validated
	// against the static JWKS. Required.
	//
	// +kubebuilder:validation:MinLength=1
	IssuerURL string `json:"issuerURL"`

	// ClientID is the OAuth2 client ID that is also the expected token audience
	// (`aud` claim): a token is accepted only when its audience includes this
	// value. Required.
	//
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`

	// CABundle carries PEM-encoded x509 CA certificates the authorizer trusts in
	// addition to its system store when reaching the OIDC issuer (discovery and
	// JWKS endpoints). Its semantics and serialization are the shared "CABundle
	// convention" documented once in common_types.go: an empty value uses the
	// pod's system trust store unchanged. It is the trust anchor for an issuer
	// signed by a private CA (e.g. an in-cluster Keycloak signed by the
	// platform's local CA).
	//
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`

	// JWKS carries a static copy of the issuer's JSON Web Key Set (the literal
	// {"keys":[...]} document) used to validate token signatures **offline**. When
	// set, the authorizer does NOT perform OIDC discovery or fetch the issuer's
	// JWKS over HTTP: it verifies the token signature against these keys and still
	// enforces iss (== IssuerURL), aud (== ClientID), and exp/nbf. This is the
	// mechanism for a token issuer that is unreachable from this cluster — e.g. a
	// remote cluster's Kubernetes API server signing service-account ID tokens.
	// When empty, the authorizer performs OIDC discovery as before (the default,
	// backward-compatible behavior). Serialized as a single base64 string per the
	// shared CABundle convention in common_types.go. oidc.caBundle is unused when
	// JWKS is set (there is no HTTP fetch to establish trust for).
	//
	// +optional
	JWKS []byte `json:"jwks,omitempty"`

	// UsernameClaim is the token claim the authorizer reads the user identity
	// from, used as the Kubernetes impersonated username. Defaults to "sub".
	// MinLength=1 rejects an explicit empty string, which would otherwise bypass
	// the default and persist an invalid (blank) claim name.
	//
	// +optional
	// +kubebuilder:default=sub
	// +kubebuilder:validation:MinLength=1
	UsernameClaim string `json:"usernameClaim,omitempty"`

	// GroupsClaim is the token claim the authorizer reads the user's groups from.
	// It is the source of truth for which claim carries the groups (the default
	// group mapping reads it directly when spec.groupMapping.celExpression is
	// empty). Defaults to "groups". MinLength=1 rejects an explicit empty string,
	// which would otherwise bypass the default and persist an invalid claim name.
	//
	// +optional
	// +kubebuilder:default=groups
	// +kubebuilder:validation:MinLength=1
	GroupsClaim string `json:"groupsClaim,omitempty"`
}

// GroupMapping configures how validated token claims are mapped to the
// Kubernetes groups the authorizer impersonates.
type GroupMapping struct {
	// CELExpression is a Common Expression Language expression evaluated against
	// the validated token claims (exposed as the `claims` variable) that returns
	// the list of Kubernetes groups to impersonate. When omitted (empty) the
	// authorizer maps the OIDC groups claim directly — the claim named by
	// spec.oidc.groupsClaim, which is the single source of truth for which claim
	// carries the groups. The field is intentionally left unset by default rather
	// than defaulted to a static `claims.groups` string, so that configuring a
	// non-default groupsClaim is honored without also having to override this
	// expression. Set it only to transform the groups (e.g. prefix or filter).
	//
	// +optional
	CELExpression string `json:"celExpression,omitempty"`
}

// BackendSpec defines the desired state of a Backend: one Kubernetes API server
// backend the authorizer fronts, with its OIDC client, claims→groups mapping,
// and privileged credential.
//
// Phase note (HOL-1386): this phase ships the API types only. The authorizer
// logic that consumes the spec — matching the request Host, validating OIDC
// tokens, evaluating the CEL mapping, and emitting impersonation headers — lands
// in later phases (HOL-1387, HOL-1388). Setting a Backend has no runtime effect
// until then. The fields are additive and backward-compatible.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.credentialsSecretRef) && has(self.serviceAccountRef))",message="credentialsSecretRef and serviceAccountRef are mutually exclusive; set at most one"
type BackendSpec struct {
	// Host is the request :authority/Host header value this Backend matches. The
	// authorizer routes an inbound request to this Backend when the request's
	// Host equals this value. Required.
	//
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Server describes the upstream Kubernetes API server this Backend fronts,
	// including its URL (possibly external) and an optional trusted CA bundle.
	// Required.
	Server ServerConfig `json:"server"`

	// OIDC is the single OIDC client this Backend validates end-user tokens with
	// (one OIDC client per backend). Required.
	OIDC OIDCConfig `json:"oidc"`

	// GroupMapping configures the optional CEL expression that maps validated
	// token claims to the Kubernetes groups the authorizer impersonates. When
	// omitted (the default), the authorizer maps the OIDC groups claim directly —
	// the claim named by spec.oidc.groupsClaim.
	//
	// +optional
	// +kubebuilder:default={}
	GroupMapping GroupMapping `json:"groupMapping,omitempty"`

	// CredentialsSecretRef names the Secret holding the backend's privileged
	// Kubernetes credential — the impersonator identity the authorizer
	// authenticates to the upstream API server with. A Secret named
	// holos-authenticator-backend-creds in the authorizer's own namespace is the
	// suggested convention; a nil ref (the field omitted) resolves a Secret of that
	// default name. It is mutually exclusive with serviceAccountRef (enforced by a
	// CEL validation rule on this spec). This is the resource's only authentication
	// dependency when set; its material is created at runtime and never committed
	// (secret-handling guardrail).
	//
	// It is a pointer (rather than a value) so the API server can distinguish an
	// omitted credentialsSecretRef from an explicitly-set one — the CEL
	// mutual-exclusion rule's has() check relies on that distinction. A nil pointer
	// means "use the default Secret"; the field is intentionally not defaulted so a
	// Backend setting serviceAccountRef instead leaves credentialsSecretRef nil.
	//
	// +optional
	CredentialsSecretRef *SecretReference `json:"credentialsSecretRef,omitempty"`

	// ServiceAccountRef names a ServiceAccount in the authorizer's own namespace
	// the authorizer mints a short-lived token for (via the TokenRequest API) to
	// obtain the backend's privileged impersonator credential, as an alternative to
	// a long-lived credentialsSecretRef. It is mutually exclusive with
	// credentialsSecretRef (enforced by a CEL validation rule on this spec). When
	// omitted, the authorizer uses credentialsSecretRef. See ServiceAccountReference
	// in common_types.go.
	//
	// Phase note (HOL-1399): this phase ships the field, defaults, and validation
	// only; the controller still resolves the credential exclusively from
	// credentialsSecretRef. The TokenRequest minting/caching/rotation that consumes
	// serviceAccountRef lands in HOL-1400.
	//
	// +optional
	ServiceAccountRef *ServiceAccountReference `json:"serviceAccountRef,omitempty"`
}

// Condition types surfaced on Backend status. The vocabulary follows the Gateway
// API convention; condition semantics are built out by the authorizer in a later
// phase (HOL-1387).
const (
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the desired state has been programmed
	// (the backend's OIDC client and upstream are configured and discoverable).
	ConditionProgrammed = "Programmed"
	// ConditionReady reports whether the backend is fully configured and usable
	// (Gateway-API Ready).
	ConditionReady = "Ready"
)

// BackendStatus defines the observed state of a Backend. It follows the
// Gateway-API status convention: a slice of standard metav1.Conditions plus the
// observedGeneration the authorizer last reconciled.
type BackendStatus struct {
	// Conditions represent the latest available observations of the backend's
	// state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this Backend
	// by the authorizer.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,authenticator}
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Backend is the Schema for the backends API. It configures one Kubernetes API
// server backend the Holos Authenticator fronts (ADR-23).
type Backend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackendSpec   `json:"spec,omitempty"`
	Status BackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackendList contains a list of Backend.
type BackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Backend `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Backend{}, &BackendList{})
}

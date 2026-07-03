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

// ImpersonationConfig gates and shapes delegated impersonation for a Backend —
// the "kubectl --as passthrough" mode. It is entirely opt-in: a Backend with a
// nil spec.impersonation runs in self impersonation only, the current
// fail-closed behavior (delegated impersonation disabled), byte-for-byte
// unchanged.
//
// Two impersonation modes.
//
//   - Self impersonation (the default, delegated impersonation disabled): the
//     authorizer validates the end user's OIDC token and impersonates *that
//     user* on the upstream API server — the identity is derived solely from the
//     validated token (spec.oidc.usernameClaim/groupsClaim/…), and any inbound
//     Impersonate-* headers on the request are stripped fail-closed. This is the
//     only mode when spec.impersonation is nil.
//
//   - Delegated impersonation ("kubectl --as passthrough", enabled by a non-nil
//     spec.impersonation): the validated token identifies an *actor* who is then
//     permitted to impersonate a *different* target identity carried on the
//     request's inbound Impersonate-* headers — exactly like `kubectl --as`. The
//     actor is trusted to name the target, but only if the actor's own mapped
//     Kubernetes groups are on the Groups allowlist below; the target the actor
//     names then flows through to the upstream API server. This is the mechanism
//     that lets a privileged operator front `kubectl --as <someone-else>` through
//     the authorizer without the authorizer holding a per-user credential.
//
// Status (HOL-1433): spec.impersonation is ACTIVE. The reconciler validates it
// and carries it into the Store Entry (HOL-1432), and the authorizer Check path
// consumes it (HOL-1433): a non-nil spec.impersonation authorizes delegated
// Impersonate-* passthrough for an actor whose mapped groups are on the Groups
// allowlist below, and its Extra claims are emitted as Impersonate-Extra-<key>
// headers in delegated mode only. Inbound Impersonate-Extra-* headers are denied
// fail-closed in both modes; the authorizer never trusts client-supplied extras.
// The field remains additive and backward-compatible — a Backend that omits
// spec.impersonation runs self-impersonation only, byte-for-byte unchanged
// (inbound Impersonate-* denied fail-closed) — but it is no longer inert: setting
// it changes request-path behavior, so treat it as the security-sensitive opt-in
// it is.
type ImpersonationConfig struct {
	// Groups is the allowlist of Kubernetes groups that gates delegated
	// impersonation: delegated impersonation ("kubectl --as passthrough") is
	// permitted for a request only when the actor's **mapped** Kubernetes groups —
	// the groups the authorizer computes for the validated actor token via the
	// default groups-claim mapping or spec.groupMapping.celExpression, *not* the raw
	// token claim — intersect this list. An actor whose mapped groups do not match
	// any entry here is denied the ability to impersonate a different target
	// identity (fail-closed), falling back to self impersonation semantics.
	//
	// It is opt-in: an omitted or empty Groups permits no delegated impersonation
	// at all (no group is allowlisted), so a spec.impersonation present but with an
	// empty Groups still leaves delegated impersonation effectively disabled. The
	// list is a set (listType=set), so the API server rejects duplicate entries at
	// admission, and each entry MinLength=1 rejects an empty group name (an empty
	// string would never match a real mapped group and only muddies the allowlist).
	//
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	Groups []string `json:"groups,omitempty"`

	// Extra maps token claims to Kubernetes Impersonate-Extra-<key>
	// impersonation headers describing the **actor** — the authenticated identity
	// that performs a delegated impersonation. It is emitted only in delegated mode,
	// after the actor's mapped groups intersect Groups. Self mode ignores
	// spec.impersonation.extra and emits only spec.oidc.extra.
	//
	// The per-entry claim-read semantics are identical to spec.oidc.extra: a
	// missing or null claim skips the entry, a present string is emitted verbatim,
	// and a present non-string denies the delegated request fail-closed. That
	// misconfiguration does not affect self-mode requests because
	// spec.impersonation.extra is resolved only on the delegated branch.
	//
	// Inbound Impersonate-Extra-* headers are always rejected fail-closed in both
	// self and delegated mode, regardless of key. The authorizer never forwards or
	// trusts client-supplied extras; it stamps extras only from validated token
	// claims. Because spec.oidc.extra and spec.impersonation.extra are emitted in
	// different modes, their keys may overlap without ambiguity. Each key's
	// canonicality, like spec.oidc.extra, is still validated by the reconciler
	// (Accepted=False on violation) rather than by an admission-time CEL rule.
	//
	// The list is a map keyed by Key (listType=map / listMapKey=key), so the API
	// server rejects duplicate keys at admission — exactly like spec.oidc.extra.
	//
	// Before HOL-1448 this field used the older actor-extra JSON name. The v1alpha1
	// API has no conversion webhook: after the CRD schema changes, stored values
	// using the old name are pruned by the structural schema until operators
	// re-apply their Backends with spec.impersonation.extra.
	// HOL-1448 is one phase of the HOL-1447 series and is not intended to ship on
	// its own; promote it only with the rest of the series so the transitional
	// pruning window is never exposed as a standalone platform release.
	//
	// +optional
	// +listType=map
	// +listMapKey=key
	Extra []ExtraMapping `json:"extra,omitempty"`
}

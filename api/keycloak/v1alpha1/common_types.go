package v1alpha1

// CABundle convention (shared across all keycloak.holos.run Kinds).
//
// Kinds in this API group that reach a Keycloak target (KeycloakInstance, and
// KeycloakClient for its own client TLS) carry a CABundle []byte spec field with
// JSON tag `caBundle,omitempty`. Its semantics and serialization are
// standardized here, once, and each spec's field godoc refers back to this block
// rather than re-describing the format — so the field is generally re-used
// across Kinds. It is the controller-wide cross-Kind convention (ADR-18 Rev 3 /
// ADR-19 Rev 5), shared verbatim in spirit with api/quay/v1alpha1.
//
// CABundle is a PEM-encoded set of x509 CA certificates the controller trusts
// in addition to its system store when establishing TLS to the Keycloak admin
// API. It follows the upstream Kubernetes caBundle convention: one or more PEM
// blocks concatenated, serialized as a single base64 string in JSON (the Go
// `[]byte` type marshals to a base64 string, and the generated CRD property is
// `type: string, format: byte`). An empty CABundle means use the controller
// pod's system trust store unchanged — the historical behavior — so the field
// is purely additive.
//
// It is the trust anchor for an in-cluster Keycloak instance whose serving
// certificate is signed by the platform's local CA rather than a public root:
// without it the reconcilers hit `x509: certificate signed by unknown
// authority`. The bundle is configuration carried on the spec, not a credential
// — the Keycloak admin credential lives in the credential Secret
// (CredentialsSecretRef), the CA bundle does not.

// DefaultCredentialsSecretName is the suggested and default name of the Secret
// holding the Keycloak admin credential the controller authenticates to the
// Keycloak admin API with. When a KeycloakInstance's credentialsSecretRef is
// omitted, the controller resolves a Secret of this name in its own
// holos-controller namespace.
const DefaultCredentialsSecretName = "holos-controller-keycloak-creds"

// SecretReference names the Secret holding the Keycloak admin credential the
// controller authenticates to the Keycloak admin API with. The controller
// resolves it in its own holos-controller namespace. Suggested/default name:
// holos-controller-keycloak-creds.
//
// This is the resource's only authentication dependency (ADR-20 dependency
// boundary): the custom resources reach Keycloak solely through the credential
// this Secret holds (the recommended auth is a confidential service-account
// client with realm-management roles, or a realm user with realm-management),
// never by importing a Keycloak client type into the API group. Its material is
// created at runtime and never committed (secret-handling guardrail).
type SecretReference struct {
	// Name of the Secret holding the Keycloak admin credential. When omitted it
	// defaults to holos-controller-keycloak-creds, resolved in the controller's
	// holos-controller namespace.
	//
	// +optional
	// +kubebuilder:default=holos-controller-keycloak-creds
	Name string `json:"name,omitempty"`

	// Key within the Secret to read the credential from. When omitted the
	// controller reads the conventional credential keys. Set it to narrow
	// resolution to a specific entry.
	//
	// +optional
	Key string `json:"key,omitempty"`
}

// KeycloakInstanceReference references the KeycloakInstance a keycloak.holos.run
// resource targets. Every Kind in this group references a KeycloakInstance: it
// is the single, centrally-managed holder of how to reach one Keycloak target
// and realm (ADR-20). The reconciler resolves the referenced instance and uses
// its url, realm, credential, and caBundle to perform the resource's Keycloak
// admin-API operations.
//
// Cross-namespace reference gating: when Namespace is set and differs from the
// referring resource's namespace, the reference is authorized by a
// security.holos.run ReferenceGrant in the KeycloakInstance's namespace (ADR-22,
// the Gateway-API ReferenceGrant convention). An unset Namespace means the
// KeycloakInstance is resolved in the referring resource's own namespace and no
// grant is required. The grant itself is defined by the security.holos.run API
// group and read at reconcile time by the authorization helper in
// internal/referencegrant — this API group neither imports nor redefines it.
type KeycloakInstanceReference struct {
	// Name of the KeycloakInstance resource to target.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the KeycloakInstance. When omitted the instance is resolved
	// in the referring resource's own namespace. When set to a different
	// namespace, the cross-namespace reference must be authorized by a
	// security.holos.run ReferenceGrant in the target namespace.
	//
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// Condition types surfaced on every keycloak.holos.run Kind's status. The
// vocabulary follows the Gateway API convention (mirroring quay.holos.run);
// condition semantics are built out by the reconcilers in later phases
// (HOL-1346, HOL-1347).
const (
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the desired state has been programmed
	// into Keycloak (Gateway-API Programmed).
	ConditionProgrammed = "Programmed"
	// ConditionReady reports whether the resource has been fully provisioned in
	// Keycloak.
	ConditionReady = "Ready"
)

// Condition reasons surfaced on keycloak.holos.run Kind status conditions. They
// are the shared reason vocabulary the reconcilers (HOL-1346, HOL-1347) set on
// the conditions above; declared here so the reasons stay consistent across
// Kinds and are catchable as constants rather than literal strings.
const (
	// ReasonCreated indicates the controller created the Keycloak object this CR
	// owns (the claim-model "created" outcome, mirroring ADR-19).
	ReasonCreated = "Created"
	// ReasonAdopted indicates the controller adopted a pre-existing Keycloak
	// object of the same identity (the claim-model "adopted" outcome).
	ReasonAdopted = "Adopted"
	// ReasonConflict indicates the targeted Keycloak object exists but was not
	// created by, and is not claimed by, this CR — it is never silently seized
	// (the claim-model "conflict" outcome).
	ReasonConflict = "Conflict"
	// ReasonCredentialsNotFound indicates the credential Secret the resource
	// references could not be resolved.
	ReasonCredentialsNotFound = "CredentialsNotFound"
	// ReasonReferenceNotGranted indicates a cross-namespace KeycloakInstance
	// reference is not authorized by a security.holos.run ReferenceGrant.
	ReasonReferenceNotGranted = "ReferenceNotGranted"
	// ReasonKeycloakError indicates a Keycloak admin-API call failed.
	ReasonKeycloakError = "KeycloakError"
	// ReasonReconciled indicates the resource's desired state was successfully
	// reconciled into Keycloak.
	ReasonReconciled = "Reconciled"
)

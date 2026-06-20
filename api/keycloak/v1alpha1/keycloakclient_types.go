package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KeycloakClientType is the OIDC client type: a public client (SPA/CLI, no
// secret, PKCE) or a confidential client (authenticated by a delivered secret).
// It mirrors the public (argocd/kargo) vs confidential (quay) distinction in
// the platform realm (keycloak-clients.md).
//
// +kubebuilder:validation:Enum=public;confidential
type KeycloakClientType string

const (
	// KeycloakClientTypePublic is a public OIDC client (no client secret, PKCE).
	KeycloakClientTypePublic KeycloakClientType = "public"
	// KeycloakClientTypeConfidential is a confidential OIDC client authenticated
	// by a delivered client secret.
	KeycloakClientTypeConfidential KeycloakClientType = "confidential"
)

// ClientSecretReference names where a confidential client's generated client
// secret is delivered. The reconciler writes a generate-once, create-if-absent
// Secret in the resource's own namespace per the secret-handling guardrail — it
// is never committed, mirroring the platform's quay-oidc bootstrap. It is
// distinct from the spec-level credentialsSecretRef (the admin credential): this
// points at the per-client delivered secret.
type ClientSecretReference struct {
	// Name of the Secret in the resource's namespace to deliver the client secret
	// into.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key within the Secret to write the client secret under.
	//
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// KeycloakClientSpec defines the desired state of a KeycloakClient: one project
// OIDC client named by its URL, its redirect/web-origin configuration, the
// client roles it defines, the group→groups-claim mapping (via client roles),
// and — for a confidential client — where to deliver the generated secret
// (ADR-20).
//
// The group→groups-claim mechanism (ADR-20): because Keycloak's Group
// Membership mapper cannot synthesize an arbitrary claim value from a path, a
// role group carries a client-role assignment (see ClientRoles) and the existing
// oidc-usermodel-client-role-mapper emits the role name into the shared groups
// claim (repo precedent in holos/components/keycloak/realm-config/buildplan.cue).
//
// +kubebuilder:validation:XValidation:rule="self.type == 'confidential' ? has(self.secretRef) : !has(self.secretRef)",message="secretRef is required for a confidential client and forbidden for a public client"
type KeycloakClientSpec struct {
	// ClientID is the Keycloak client ID, named by its URL (e.g.
	// https://quay.holos.localhost). It is immutable: it is the client's durable
	// identity in the realm's global client namespace, so the ownership claim and
	// the finalizer always target exactly the client this CR provisioned.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clientId is immutable"
	ClientID string `json:"clientId"`

	// Type is the OIDC client type, public or confidential. It is immutable: a
	// public<->confidential transition would strand the per-client artifacts keyed
	// to the prior type (a confidential->public edit would orphan the delivered
	// client-secret Secret; a public->confidential edit would need a freshly
	// generated secret and a redirect/PKCE re-evaluation), so the type is fixed for
	// the client's life — recreate the resource to change it.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type KeycloakClientType `json:"type"`

	// InstanceRef references the KeycloakInstance this client is provisioned in. A
	// cross-namespace reference (Namespace set to a different namespace) is gated
	// by a security.holos.run ReferenceGrant in the instance's namespace. It is
	// immutable: retargeting a provisioned client to another instance would create
	// a second OIDC client in the new realm and orphan the original (the finalizer
	// can no longer reach it), so the target realm is fixed for the client's life.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceRef is immutable"
	InstanceRef KeycloakInstanceReference `json:"instanceRef"`

	// RedirectURIs are the allowed OAuth2 redirect URIs for the client.
	//
	// +optional
	// +listType=set
	RedirectURIs []string `json:"redirectUris,omitempty"`

	// WebOrigins are the allowed CORS web origins for the client.
	//
	// +optional
	// +listType=set
	WebOrigins []string `json:"webOrigins,omitempty"`

	// ClientRoles optionally lists the client roles defined on this client — the
	// primitive owner/editor/viewer triad scoped to this one client. A role group
	// (KeycloakGroup) assigns one of these, and the per-client
	// oidc-usermodel-client-role-mapper emits the role name into the shared groups
	// claim (the group→claim mechanism, ADR-20). Each entry's ClientRef is this
	// KeycloakClient's own metadata.name (the object name, not the URL-shaped
	// clientId — see ClientRoleReference).
	//
	// +optional
	// +listType=atomic
	ClientRoles []ClientRoleReference `json:"clientRoles,omitempty"`

	// SecretRef names where a confidential client's generated secret is delivered
	// (a generate-once Secret in this resource's namespace). It is required for a
	// confidential client and forbidden for a public client (a public client
	// carries no secret) — enforced by a CEL validation on this spec, so the
	// type/secretRef pair is always consistent at admission.
	//
	// +optional
	SecretRef *ClientSecretReference `json:"secretRef,omitempty"`

	// CABundle carries PEM-encoded x509 CA certificates the controller trusts in
	// addition to its system store when reaching the Keycloak admin API for this
	// client. Its semantics and serialization are the shared "CABundle convention"
	// documented once in common_types.go: an empty value uses the controller pod's
	// system trust store unchanged. It is the trust anchor for an in-cluster
	// Keycloak signed by the platform's local CA.
	//
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`

	// Adopt opts in to taking ownership of a pre-existing Keycloak client of the
	// same clientId (the claim model, mirroring ADR-19's Organization). Default
	// false: because a Keycloak client lives in the realm's single global client
	// namespace while this CR is Kubernetes-namespaced, a client this CR did not
	// create and does not already own is a Conflict (Ready=False, reason Conflict)
	// and is never silently seized or reconfigured. Set adopt: true to deliberately
	// claim and converge such a client. An adopted client is released, never
	// deleted, on CR removal.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`
}

// KeycloakClientStatus defines the observed state of a KeycloakClient, following
// the Gateway-API status convention.
type KeycloakClientStatus struct {
	// Conditions represent the latest available observations of the client's
	// state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// KeycloakClient by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Created records whether this CR created the Keycloak client (true) versus
	// adopted a pre-existing client of the same clientId (false). The finalizer
	// deletes the Keycloak client only when Created is true; an adopted client is
	// released, never deleted.
	//
	// +optional
	Created bool `json:"created,omitempty"`

	// Adopted records whether this CR adopted a pre-existing Keycloak client of
	// the same clientId rather than creating it. An adopted client is released,
	// never deleted, on CR removal.
	//
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// ClientUUID is the Keycloak UUID of the client this CR owns or adopted. It is
	// the immutable handle the reconciler converges roles/mapper/secret against and
	// the finalizer verifies before deleting — recorded so a re-run targets exactly
	// the client this CR provisioned, and a UUID mismatch (the client was replaced
	// out of band at the same clientId) is a Conflict rather than a silent seizure.
	//
	// +optional
	ClientUUID string `json:"clientUUID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,keycloak}
// +kubebuilder:printcolumn:name="ClientID",type=string,JSONPath=`.spec.clientId`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KeycloakClient is the Schema for the keycloakclients API. It manages one
// project OIDC client named by its URL and the group→groups-claim mapping via
// client roles (ADR-20).
type KeycloakClient struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeycloakClientSpec   `json:"spec,omitempty"`
	Status KeycloakClientStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeycloakClientList contains a list of KeycloakClient.
type KeycloakClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeycloakClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KeycloakClient{}, &KeycloakClientList{})
}

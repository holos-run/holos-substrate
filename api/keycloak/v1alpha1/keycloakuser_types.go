package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentityProviderLink configures the federated-identity link the controller
// sets on a pre-provisioned user so first federated login auto-links the
// existing record rather than creating a duplicate (ADR-20). It pairs an
// Admin-API pre-create with the realm's first-broker-login flow (Detect Existing
// Broker User + Automatically Set Existing User + Trust Email), the realm/IdP
// half of which is owned by the platform realm config (keycloak-config-cli), not
// this CR.
//
// Security tradeoff (ADR-20): email-based auto-link trusts the IdP's asserted
// email, so it is only safe when the IdP verifies email. The CR owns the user
// record and its IdP link; the realm must be configured to trust the email.
type IdentityProviderLink struct {
	// Alias is the Keycloak identity-provider alias to link the user to (the IdP
	// configured on the realm, e.g. an upstream OIDC/SAML broker).
	//
	// +kubebuilder:validation:MinLength=1
	Alias string `json:"alias"`

	// UserID is the user identifier at the upstream identity provider (the
	// federated identity's subject). When omitted the link is keyed by email via
	// the first-broker-login auto-link flow rather than a pre-known subject.
	//
	// +optional
	UserID string `json:"userId,omitempty"`

	// UserName is the username at the upstream identity provider, recorded on the
	// federated-identity link. Optional; informational when the subject (UserID)
	// is the authoritative key.
	//
	// +optional
	UserName string `json:"userName,omitempty"`
}

// KeycloakUserSpec defines the desired state of a KeycloakUser: a user
// pre-provisioned by email (only when necessary), its group memberships, and the
// IdP link that lets first federated login auto-link the existing record
// (ADR-20).
type KeycloakUserSpec struct {
	// Email is the user's email address (e.g. bob@example.com). It is required
	// and is the key the first-login auto-link matches on, so it must be the
	// email the IdP asserts. It is immutable: email is the user's durable
	// identity, so changing it would retarget reconciliation and finalization to a
	// different Keycloak user, risking cross-user mutation or deletion.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="email is immutable"
	Email string `json:"email"`

	// Username optionally sets the Keycloak username. When omitted the controller
	// derives one (conventionally the email), so set it only to pin a specific
	// username.
	//
	// +optional
	Username string `json:"username,omitempty"`

	// InstanceRef references the KeycloakInstance this user is provisioned in. A
	// cross-namespace reference (Namespace set to a different namespace) is gated
	// by a security.holos.run ReferenceGrant in the instance's namespace.
	InstanceRef KeycloakInstanceReference `json:"instanceRef"`

	// Groups optionally lists the Keycloak group paths the user joins (e.g.
	// projects/my-project/roles/editor). An empty or omitted list means the user
	// is provisioned with no managed group memberships.
	//
	// +optional
	// +listType=set
	Groups []string `json:"groups,omitempty"`

	// IdentityProviderLink optionally configures the federated-identity link so
	// first federated login auto-links this pre-created record instead of creating
	// a duplicate. When omitted the user is provisioned without a managed IdP
	// link.
	//
	// +optional
	IdentityProviderLink *IdentityProviderLink `json:"identityProviderLink,omitempty"`

	// Adopt opts in to taking ownership of a pre-existing Keycloak user of the
	// same email (the claim model, mirroring ADR-19's Organization). Default
	// false: a user this CR did not create and does not already own is a Conflict
	// (Ready=False, reason Conflict) and is never silently seized — Keycloak realm
	// users are a single global namespace while this CR is Kubernetes-namespaced.
	// Set adopt: true to deliberately claim such a user. An adopted user is
	// released, never deleted, on CR removal.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`
}

// KeycloakUserStatus defines the observed state of a KeycloakUser, following the
// Gateway-API status convention plus the durable ownership markers the claim
// model requires (mirroring ADR-19).
type KeycloakUserStatus struct {
	// Conditions represent the latest available observations of the user's state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// KeycloakUser by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Created records whether this CR created the Keycloak user (true) versus
	// adopted a pre-existing record of the same email (false). It is the
	// controller-managed owner record persisted on status; the finalizer deletes
	// the Keycloak user only when Created is true.
	//
	// +optional
	Created bool `json:"created,omitempty"`

	// Adopted records whether this CR adopted a pre-existing Keycloak user of the
	// same email rather than creating it. An adopted user is released, never
	// deleted, on CR removal — so adoption is non-destructive to a record the
	// platform did not create.
	//
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// UserID is the Keycloak UUID of the user this CR owns or adopted. It is the
	// durable handle the reconciler resolves group memberships and the IdP link
	// against, and the finalizer deletes (when Created) — recorded so a re-run
	// targets exactly the user this CR provisioned even if the email lookup were
	// to drift.
	//
	// +optional
	UserID string `json:"userID,omitempty"`

	// ManagedGroups records the Keycloak group paths this CR has joined the user
	// to, so a membership removed from spec.groups is actively revoked on the next
	// reconcile (reconcile-to-desired-set rather than add-only) and an adopted
	// user's release prunes exactly the memberships this CR added.
	//
	// +optional
	// +listType=set
	ManagedGroups []string `json:"managedGroups,omitempty"`

	// ManagedIdentityProvider records the IdP alias this CR created a
	// federated-identity link to (spec.identityProviderLink.alias), so on CR
	// removal an adopted user's release prunes exactly the link this CR added
	// rather than leaving a stale federation behind. Empty when no managed link
	// exists (no spec.identityProviderLink, or an email-only auto-link entry that
	// is realm-flow-driven and not an Admin-API link this CR owns).
	//
	// +optional
	ManagedIdentityProvider string `json:"managedIdentityProvider,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,keycloak}
// +kubebuilder:printcolumn:name="Email",type=string,JSONPath=`.spec.email`
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KeycloakUser is the Schema for the keycloakusers API. It pre-provisions a user
// by email, assigns group membership, and configures the IdP link for first-login
// auto-link (ADR-20).
type KeycloakUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeycloakUserSpec   `json:"spec,omitempty"`
	Status KeycloakUserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KeycloakUserList contains a list of KeycloakUser.
type KeycloakUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeycloakUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KeycloakUser{}, &KeycloakUserList{})
}

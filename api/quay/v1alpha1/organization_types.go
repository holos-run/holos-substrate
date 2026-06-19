package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OrganizationTeamRole is the org-level role a Quay team holds within its
// organization. It maps directly to the Quay team "role" (the role field of
// PUT /api/v1/organization/{org}/team/{team}). It is distinct from
// RepositoryRole, which is a per-repository permission rather than an org role.
//
//   - admin   — full administrative control of the organization.
//   - creator — may create repositories in the organization.
//   - member  — plain membership with no creation or admin rights.
//
// +kubebuilder:validation:Enum=admin;creator;member
type OrganizationTeamRole string

const (
	// OrganizationTeamRoleAdmin grants full administrative control of the org.
	OrganizationTeamRoleAdmin OrganizationTeamRole = "admin"
	// OrganizationTeamRoleCreator allows repository creation in the org.
	OrganizationTeamRoleCreator OrganizationTeamRole = "creator"
	// OrganizationTeamRoleMember is plain membership with no creation or admin
	// rights.
	OrganizationTeamRoleMember OrganizationTeamRole = "member"
)

// RepositoryRole is a Quay repository permission level — the role granted on a
// repository (e.g. via an organization default permission / prototype). It is
// the repo-permission enum and is deliberately distinct from
// RepositoryVisibility in repository_types.go, which controls public/private
// visibility rather than an access role.
//
//   - read  — pull access.
//   - write — pull and push access.
//   - admin — full control of the repository.
//
// +kubebuilder:validation:Enum=read;write;admin
type RepositoryRole string

const (
	// RepositoryRoleRead grants pull access to a repository.
	RepositoryRoleRead RepositoryRole = "read"
	// RepositoryRoleWrite grants pull and push access to a repository.
	RepositoryRoleWrite RepositoryRole = "write"
	// RepositoryRoleAdmin grants full control of a repository.
	RepositoryRoleAdmin RepositoryRole = "admin"
)

// SyncedTeam declares a Quay team whose membership is OIDC-synced from a group.
// The reconciler upserts the named team, binds it to the OIDC group, sets its
// org role, and (optionally) grants it an org default repository permission.
//
// OIDC group reference, not a Keycloak coupling: OIDCGroup is a plain
// group-name string — the value of the OIDC groups claim. The quay.holos.run
// API group deliberately does not depend on Keycloak (or any other identity
// provider) type or import (ADR-19 dependency boundary, AC #2/#7): the group is
// referenced only by name, so the team binding works against whatever OIDC
// provider Quay is configured with, not specifically Keycloak.
//
// Adoption boundary (forward-compatibility): adoption of a pre-existing Quay
// team this CR did not create is currently unsupported and is surfaced as a
// reconcile error rather than a silent takeover (mirroring the Organization
// claim model). Per-team adoption is reserved for a future optional Adopt bool
// field on this struct; the schema is intentionally free of any required field
// or validation that would have to change to add it backwards-compatibly
// (AC #6).
type SyncedTeam struct {
	// Name is the Quay team name to create and manage within the organization.
	// It is the +listMapKey for the spec.syncedTeams list, so it is unique per
	// Organization.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// OIDCGroup is the OIDC groups-claim value this team's membership is synced
	// from. It is a plain group-name reference: the quay.holos.run group does
	// not depend on Keycloak (or any IdP) type or import — the group is named by
	// string only (ADR-19 dependency boundary, AC #2). The reconciler enables
	// Quay team syncing bound to this group so membership tracks the claim.
	//
	// +kubebuilder:validation:MinLength=1
	OIDCGroup string `json:"oidcGroup"`

	// Role is the team's org-level Quay role. It is required with no default so
	// the intent (admin vs. creator vs. member) is always explicit rather than
	// inherited from a default.
	Role OrganizationTeamRole `json:"role"`

	// RepositoryPermission optionally grants this team an organization default
	// repository permission (a Quay prototype): a repo role applied across all
	// repositories in the organization. A nil pointer means no default
	// permission is managed for this team. When set, the reconciler maintains an
	// org default-permission prototype delegating the given repo role to this
	// team.
	//
	// +optional
	RepositoryPermission *RepositoryRole `json:"repositoryPermission,omitempty"`
}

// OrganizationSpec defines the desired state of a Quay Organization.
//
// The reconciler creates (or, per the ADR-19 claim model, adopts) the named
// Quay organization. The spec deliberately carries no repository list — a
// Repository is its own resource (ADR-19, AC #9) — and no Kargo/Argo CD
// coupling; the only external dependency is the Quay credential in
// CredentialsSecretRef.
//
// Scope note: this spec carries name, email, credentialsSecretRef, and adopt
// (the claim-model opt-in the HOL-1311 reconciler enforces). The
// remaining ADR-19 illustrative schema (access[] group→team bindings,
// allowRepositoryCreation, and the explicit organizationName-vs-metadata.name
// split) is deferred to the ADR-reconciliation phase (HOL-1314); adding it here
// would be unused surface ahead of the logic that consumes it. New fields are
// additive to this type, so deferring them causes no API break.
type OrganizationSpec struct {
	// Name is the Quay organization name to create or adopt. It is immutable:
	// once set, the Quay org it binds to does not change. It is required —
	// callers conventionally set it to the resource's metadata.name, but the
	// controller does not default it (no defaulting webhook in this scaffold).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// Email is the organization contact email. Quay requires every namespace
	// (user or organization) to have a unique email address. It is mutable and
	// is reconciled to Quay on drift.
	//
	// +kubebuilder:validation:MinLength=1
	Email string `json:"email"`

	// CredentialsSecretRef names the Secret holding the Quay superuser OAuth
	// Application credential the reconciler authenticates to Quay with. A
	// Secret named holos-controller-quay-creds in the holos-controller
	// namespace is the suggested convention, and the field defaults to that
	// name when omitted. The Secret carries the Quay API URL and token (keys
	// url, token, optional username). This is the resource's only
	// authentication dependency (AC #7); its material is created at runtime
	// and never committed (secret-handling guardrail).
	//
	// +optional
	// +kubebuilder:default={name: "holos-controller-quay-creds"}
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef,omitempty"`

	// Adopt opts in to taking ownership of a pre-existing, externally-created
	// Quay organization of the same name (ADR-19's claim model). Default false:
	// an org this CR did not create and does not already own is a Conflict
	// (Ready=False, reason Conflict) and is never silently seized — the
	// controller's credential carries FEATURE_SUPERUSERS_FULL_ACCESS, so without
	// this guard a namespaced CR could take over another tenant's global Quay
	// org. Set adopt: true to deliberately claim such an org.
	//
	// An adopted org is released (the finalizer drops without deleting) rather
	// than deleted on CR removal, so adoption is non-destructive to a resource
	// the platform did not create.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// CABundle carries PEM-encoded x509 CA certificates the controller trusts
	// in addition to its system store when reaching the Quay API. Its semantics
	// and serialization are the shared "CABundle convention" documented once in
	// common_types.go: it follows the upstream Kubernetes caBundle convention
	// (PEM certs serialized as a single base64 string) and an empty value uses
	// the controller pod's system trust store unchanged. It is the trust anchor
	// for the in-cluster Quay registry signed by the platform's local CA.
	//
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`

	// SyncedTeams declares the Quay teams whose membership is OIDC-synced from a
	// group, each with an org role and an optional org default repository
	// permission. Zero or more entries; an empty or omitted list means the
	// organization manages no synced teams. It is a keyed list (listMapKey
	// name), so server-side apply merges entries by team name and the reconciler
	// can add, update, or remove individual teams without clobbering peers.
	//
	// Management is non-exclusive: the controller manages only the teams it
	// creates (tracked in status.managedTeams) and leaves teams created by other
	// identities alone — adopting a pre-existing team is a reconcile error, not a
	// takeover (see SyncedTeam).
	//
	// Phase note: this field is the API surface only. The reconciler that
	// consumes it — creating/syncing Quay teams and org default permissions and
	// recording them in status.managedTeams — lands in a later phase, so setting
	// it has no effect until then. The field is additive and backward-compatible,
	// so declaring it ahead of the reconciler does not change existing behavior.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	SyncedTeams []SyncedTeam `json:"syncedTeams,omitempty"`
}

// Condition types surfaced on Organization and Repository status. The vocabulary
// follows the Gateway API convention; condition semantics are built out by the
// reconcilers in later phases (HOL-1311, HOL-1312).
const (
	// ConditionReady reports whether the resource has been fully provisioned
	// in Quay.
	ConditionReady = "Ready"
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the desired state has been
	// programmed into Quay (Gateway-API Programmed).
	ConditionProgrammed = "Programmed"
)

// OrganizationStatus defines the observed state of an Organization. It follows
// the Gateway-API status convention: a slice of standard metav1.Conditions plus
// the observedGeneration the controller last reconciled.
type OrganizationStatus struct {
	// Conditions represent the latest available observations of the
	// organization's state.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the most recent generation observed for this
	// Organization by the controller.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Created is the durable ownership marker of ADR-19's claim model: it
	// records whether this CR created the Quay organization (true) versus
	// adopted a pre-existing one (false). It is the controller-managed owner
	// record the claim model requires, persisted on the resource's own status
	// so it survives controller restarts. The finalizer deletes the Quay org
	// only when Created is true; an adopted org is released, never deleted.
	//
	// +optional
	Created bool `json:"created,omitempty"`

	// ManagedTeams records the Quay team names this CR created and manages. It is
	// the controller-managed owner record for synced teams, the team-level analog
	// of Created: it lists only the teams this resource provisioned, never teams
	// created by other identities.
	//
	// It underpins two reconcile behaviors. Non-exclusive management (AC #5): the
	// reconciler manages exactly these teams and ignores the rest, so a team
	// dropped from spec.syncedTeams is deleted only if it appears here (this CR
	// created it), never a foreign team of the same name. Adoption-is-an-error
	// (AC #6): a spec team that already exists in Quay but is absent from this
	// list was not created by this CR, so the reconciler reports a conflict
	// rather than silently adopting it.
	//
	// +optional
	ManagedTeams []string `json:"managedTeams,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,quay}
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Organization is the Schema for the organizations API. It names and applies a
// Quay organization in the in-cluster registry (ADR-19).
type Organization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OrganizationSpec   `json:"spec,omitempty"`
	Status OrganizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OrganizationList contains a list of Organization.
type OrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Organization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Organization{}, &OrganizationList{})
}

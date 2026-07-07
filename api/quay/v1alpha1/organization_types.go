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

// SyncedTeam declares a Quay team whose membership is synchronized from an OIDC
// group. The controller manages the team role, optional default repository
// permission, and sync binding. A pre-existing team not managed by this resource
// is reported as a conflict unless Adopt is true.
type SyncedTeam struct {
	// Name is the Quay team name to manage inside the organization. It is
	// required, must be unique within syncedTeams, must be between 2 and 255
	// characters, and has no default.
	//
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*$`
	Name string `json:"name"`

	// OIDCGroup is the groups-claim value whose members populate this Quay team.
	// It is required, has no default, and is a plain string so the API can work
	// with any OIDC provider Quay is configured to trust.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	OIDCGroup string `json:"oidcGroup"`

	// Role is the team's organization-level Quay role. It is required with no
	// default so administrative intent is explicit.
	Role OrganizationTeamRole `json:"role"`

	// Adopt opts in to claiming a pre-existing Quay team with the same name. It
	// defaults to false; without this opt-in, an unmanaged existing team is
	// reported as a conflict. An adopted team is managed the same way as a team
	// this resource created: its membership syncs from OIDC, its role and default
	// repository permission are reconciled, and it is removed when this resource
	// deletes the Quay organization. If this resource releases an adopted
	// organization without deleting it, managed teams in that organization are left
	// in place.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// RepositoryPermission grants an organization default repository permission
	// to this team. When omitted, the controller manages no default repository
	// permission for the team.
	//
	// +optional
	RepositoryPermission *RepositoryRole `json:"repositoryPermission,omitempty"`
}

// OrganizationSpec describes the Quay organization the platform controller
// creates, adopts, and keeps in sync. Repositories are declared separately with
// Repository resources. The only authentication input is credentialsSecretRef,
// which points at a runtime Secret read by the controller.
type OrganizationSpec struct {
	// Name is the Quay organization name to create or adopt. It is required,
	// immutable, must be between 2 and 255 characters, and has no default;
	// callers usually set it to metadata.name.
	//
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// Email is the organization contact email Quay stores for the namespace. It
	// is required, must look like an address with a non-empty local part and a
	// DNS-style domain with at least two labels, has no default, and is
	// reconciled if it drifts.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`
	Email string `json:"email"`

	// CredentialsSecretRef selects the controller-namespace Secret containing the
	// Quay API URL and OAuth token. When omitted, the controller uses
	// holos-controller-quay-creds. The Secret material is created at runtime and
	// is not stored in this resource.
	//
	// +optional
	// +kubebuilder:default={name: "holos-controller-quay-creds"}
	CredentialsSecretRef *SecretReference `json:"credentialsSecretRef,omitempty"`

	// Adopt opts in to claiming a pre-existing Quay organization with the same
	// name. It defaults to false; without this opt-in, an unowned existing
	// organization is reported as a conflict. An adopted organization is released,
	// not deleted, when this resource is removed.
	//
	// +optional
	Adopt bool `json:"adopt,omitempty"`

	// DeletionPolicy controls what happens to the Quay organization when this
	// resource is deleted. Delete removes the organization from Quay after
	// verifying this resource still owns it. Orphan leaves the organization in
	// place and removes only this controller's ownership marker, so a replacement
	// resource can adopt the organization later. When omitted, the behavior follows
	// how ownership was established: an organization this resource created is
	// deleted, and an adopted organization is released without being deleted.
	//
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// CABundle carries PEM-encoded x509 CA certificates the controller trusts
	// in addition to its system store when reaching the Quay API. The API server
	// serializes this byte slice as one base64 string. When omitted or empty, the
	// controller uses only its system trust store. The maximum length applies to
	// the base64-encoded API representation.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1048576
	CABundle []byte `json:"caBundle,omitempty"`

	// SyncedTeams is the set of Quay teams the controller manages for this
	// organization. Each entry binds team membership to an OIDC group and may add
	// an organization default repository permission. When omitted or empty, no
	// teams are managed.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	SyncedTeams []SyncedTeam `json:"syncedTeams,omitempty"`
}

// OrganizationStatus reports what the controller most recently observed and
// changed in Quay for an Organization.
type OrganizationStatus struct {
	// Conditions are the current Accepted, Programmed, and Ready observations for
	// the organization. They are omitted until the controller has reconciled the
	// resource.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the latest metadata.generation the controller has
	// reconciled. It is omitted until the first status write.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Created records whether this resource created the Quay organization. True
	// means finalization may delete the organization. False means the resource
	// adopted an existing organization and finalization only releases it. When
	// omitted, the controller has not recorded ownership yet.
	//
	// +optional
	Created *bool `json:"created,omitempty"`

	// ManagedTeams records the Quay team names this resource created or adopted
	// and owns. The controller uses it to delete only teams it owns and to report
	// a conflict when spec.syncedTeams names an existing foreign team. It is
	// omitted when no managed teams have been recorded.
	//
	// +optional
	// +listType=set
	ManagedTeams []string `json:"managedTeams,omitempty"`

	// LastValidatedTime is the last time the controller successfully read Quay and
	// confirmed or restored the declared organization state. It is omitted until
	// the first successful validation.
	//
	// +optional
	LastValidatedTime *metav1.Time `json:"lastValidatedTime,omitempty"`

	// LastMutatedTime is the last time the controller actually changed Quay for
	// this organization, such as creating the org, updating its email, or
	// reconciling synced teams. It is omitted until the first remote mutation.
	//
	// +optional
	LastMutatedTime *metav1.Time `json:"lastMutatedTime,omitempty"`

	// LastMutationReason classifies why the last remote mutation happened. It is
	// written with lastMutatedTime and omitted when no mutation has been recorded.
	//
	// +optional
	LastMutationReason MutationReason `json:"lastMutationReason,omitempty"`

	// LastDriftTime is the last time the controller remediated out-of-band drift.
	// It is omitted until drift remediation occurs and is preserved across later
	// spec-driven mutations.
	//
	// +optional
	LastDriftTime *metav1.Time `json:"lastDriftTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={holos,quay}
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Validated",type=date,priority=1,JSONPath=`.status.lastValidatedTime`

// Organization declares a Quay organization in the platform registry, created
// and kept in sync by the platform controller.
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

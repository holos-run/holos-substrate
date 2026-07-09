// Package v1alpha1 contains the API types for the keycloak.holos.run API group,
// version v1alpha1.
//
// The group models the per-project, tenant-facing Keycloak identity primitives
// the platform provisions on a project's behalf — KeycloakInstance (a
// centrally-managed reference to one Keycloak target), KeycloakGroup,
// KeycloakGroupMembership, KeycloakUser, and KeycloakClient — as Kubernetes
// custom resources reconciled by the holos-controller (ADR-18, ADR-20). It is
// the second API group the controller owns, alongside quay.holos.run (ADR-19).
//
// The resources express their only external coupling to Keycloak as a
// KeycloakInstance plus a credential secretRef (see SecretReference): reaching
// the Keycloak admin API is a runtime concern of the reconciler, never an
// API-type import. Consequently this package depends only on k8s.io/api*,
// k8s.io/apimachinery, and sigs.k8s.io/controller-runtime — never on Quay,
// Kargo, Argo CD, or any Keycloak client-library type (ADR-20 dependency
// boundary). OIDC group names consumed cross-group (e.g. Quay's syncedTeams)
// remain data referenced by name, preserving the ADR-19 boundary in reverse.
//
// +kubebuilder:object:generate=true
// +groupName=keycloak.holos.run
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "keycloak.holos.run", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Package v1alpha1 contains the API types for the security.holos.run API group,
// version v1alpha1.
//
// The group models cross-namespace authorization policy for the platform
// (ADR-22). Its sole Kind is ReferenceGrant, a Gateway-API-style declarative
// policy that permits an object in one namespace to be referenced by an object
// in another. Like upstream gateway.networking.k8s.io ReferenceGrant, it has no
// dedicated reconciler — it is policy read by other controllers (e.g. the
// Keycloak reconcilers in later phases) through the authorization helper in
// internal/referencegrant.
//
// The package depends only on k8s.io/apimachinery and
// sigs.k8s.io/controller-runtime — never on any other API group, identity
// provider, or external client type.
//
// +kubebuilder:object:generate=true
// +groupName=security.holos.run
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "security.holos.run", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

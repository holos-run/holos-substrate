// Package v1alpha1 contains the API types for quay.holos.run/v1alpha1.
//
// The group models the in-cluster Quay registry data plane the platform
// provisions on a project's behalf as Kubernetes custom resources reconciled by
// the holos-controller. The API currently exposes Organizations and
// Repositories. Resources express their only external coupling to Quay through a
// credential Secret reference; reaching Quay is a runtime concern of the
// controller, not an API-type import. Consequently this package depends only on
// k8s.io/api*, k8s.io/apimachinery, and sigs.k8s.io/controller-runtime, never on
// Kargo or Argo CD types.
//
// +kubebuilder:object:generate=true
// +groupName=quay.holos.run
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "quay.holos.run", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

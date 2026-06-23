// Package v1alpha1 contains the API types for the authenticator.holos.run API
// group, version v1alpha1.
//
// The group models the configuration of the Holos Authenticator (ADR-23): an
// Istio/Envoy gRPC external authorizer that fronts one or more Kubernetes API
// server backends — in-cluster or external — authenticating end users via OIDC,
// mapping token claims to Kubernetes groups via a CEL expression, and returning
// Kubernetes impersonation headers. A Backend custom resource describes one such
// API server backend: the request Host it matches, the upstream server URL (with
// an optional trusted CA bundle), the OIDC client that validates tokens, the
// claims→groups CEL mapping, and a privileged credential secretRef. Reaching the
// API server or the OIDC issuer is a runtime concern of the authorizer, never an
// API-type import: this package depends only on k8s.io/api*,
// k8s.io/apimachinery, and sigs.k8s.io/controller-runtime — mirroring the
// quay.holos.run dependency boundary (ADR-19).
//
// +kubebuilder:object:generate=true
// +groupName=authenticator.holos.run
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "authenticator.holos.run", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

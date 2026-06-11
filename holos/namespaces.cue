package holos

import (
	corev1 "k8s.io/api/core/v1"
)

// namespaces is the central registry of every platform namespace, rendered by
// the namespaces component (components/namespaces/) as one manifest file per
// Namespace resource.  This file lives at the holos root so it is a CUE
// ancestor of every component instance: register a namespace here, not inline
// in a component.
//
// Mesh enrollment is the registry's main policy surface: platform namespaces
// carrying workloads MUST carry the istio.io/dataplane-mode=ambient label per
// holos/docs/mesh-enrollment.md; the exceptions below document why they are
// exempt.
//
// The kubernetes.io/metadata.name label is NOT declared here: the repo's
// corev1.#Namespace overlay (cue.mod/usr/k8s.io/api/core/v1/namespace.cue)
// forces it onto every Namespace, matching the value the API server sets
// automatically, so the rendered manifests carry it without any entry
// declaring it.
namespaces: [NAME=string]: corev1.#Namespace & {
	apiVersion: "v1"
	kind:       "Namespace"
	metadata: name: NAME
}

namespaces: {
	// istio-system hosts the mesh dataplane and control plane themselves:
	// istiod, istio-cni, and ztunnel.  It is deliberately NOT enrolled in
	// ambient (no istio.io/dataplane-mode=ambient label): ztunnel is the node
	// proxy that implements enrollment; redirecting its own traffic (or the
	// control plane it synchronizes with) through itself is circular and
	// unsupported.  The mesh infrastructure secures its own control-plane
	// connections natively.  See holos/docs/mesh-enrollment.md.
	//
	// Keep this name in sync with IstioNamespace in
	// components/istio/istio.cue: that file is an ancestor only of the istio
	// leaf components, so it cannot be referenced from here — the two literal
	// values must match.
	"istio-system": _

	// istio-gateways hosts the auto-provisioned shared Gateway pods.  It is
	// deliberately NOT enrolled in ambient (no istio.io/dataplane-mode=ambient
	// label): the gateway pods are Envoy proxies themselves and terminate mesh
	// traffic natively, so redirecting them through ztunnel adds nothing.  See
	// holos/docs/mesh-enrollment.md.
	"istio-gateways": _

	// echo is the permanent Layer 0 smoke-test namespace.  Enroll every
	// workload in this namespace in the Istio ambient mesh; ztunnel captures
	// their traffic over HBONE.  See holos/docs/mesh-enrollment.md.
	echo: metadata: labels: "istio.io/dataplane-mode": "ambient"
}

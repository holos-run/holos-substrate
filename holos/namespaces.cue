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
// holos/docs/mesh-enrollment.md.  Every entry declares enrollment
// deliberately through the required _ambient field — rendering fails until it
// is set — so an exemption is a reviewable `_ambient: false` with a rationale
// comment, never a silent omission.
//
// The kubernetes.io/metadata.name label is NOT declared here: the repo's
// corev1.#Namespace overlay (cue.mod/usr/k8s.io/api/core/v1/namespace.cue)
// forces it onto every Namespace, matching the value the API server sets
// automatically, so the rendered manifests carry it without any entry
// declaring it.
namespaces: [NAME=string]: corev1.#Namespace & {
	apiVersion: "v1"
	kind:       "Namespace"
	// Namespace names must be RFC 1123 DNS labels — the rule the API server
	// enforces — and NAME flows into the rendered artifact's file path
	// (components/namespaces/buildplan.cue), so reject anything else at
	// render time before it can produce an invalid manifest or escape the
	// deploy tree.
	metadata: name: NAME & =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"

	// _ambient declares whether the namespace's workloads enroll in the
	// Istio ambient mesh; true derives the enrollment label below.  No
	// default: every entry must take a position.
	_ambient: bool
	if _ambient {
		// Enroll every workload in this namespace in the Istio ambient
		// mesh; ztunnel captures their traffic over HBONE.  See
		// holos/docs/mesh-enrollment.md.
		metadata: labels: "istio.io/dataplane-mode": "ambient"
	}
}

// #RegisteredNamespace is the disjunction of every registered namespace
// name.  Components unify their namespace literal with it so silent drift
// between the literal and the registry entry becomes a render failure
// instead of an apply-time NotFound error.  This file is a CUE ancestor of
// every component instance, so components reference this definition rather
// than cloning the comprehension.
#RegisteredNamespace: or([for NAME, _ in namespaces {NAME}])

namespaces: {
	// istio-system hosts the mesh dataplane and control plane themselves:
	// istiod, istio-cni, and ztunnel.  It is deliberately NOT enrolled in
	// ambient: ztunnel is the node proxy that implements enrollment;
	// redirecting its own traffic (or the control plane it synchronizes
	// with) through itself is circular and unsupported.  The mesh
	// infrastructure secures its own control-plane connections natively.
	// See holos/docs/mesh-enrollment.md.
	//
	// Keep this name in sync with IstioNamespace in
	// components/istio/istio.cue: that file is an ancestor only of the istio
	// leaf components, so it cannot be referenced from here.  istio.cue
	// asserts at render time that its value is registered here.
	"istio-system": _ambient: false

	// istio-gateways hosts the auto-provisioned shared Gateway pods.  It is
	// deliberately NOT enrolled in ambient: the gateway pods are Envoy
	// proxies themselves and terminate mesh traffic natively, so redirecting
	// them through ztunnel adds nothing.  See holos/docs/mesh-enrollment.md.
	"istio-gateways": _ambient: false

	// cert-manager hosts the cert-manager controller, webhook, and
	// cainjector; its workloads enroll in the ambient mesh per the platform
	// convention.  scripts/local-ca pre-creates this namespace at cluster
	// bootstrap (the local-ca Secret must exist before cert-manager
	// installs).  The script and the namespaces component both server-side
	// apply this Namespace with kubectl's default field manager, so the
	// script's manifest must carry the same labels as this entry — an apply
	// that omitted the enrollment label would silently strip it.
	//
	// Keep this name and the labels in sync with CertManagerNamespace in
	// components/cert-manager/cert-manager.cue and with the namespace
	// manifest scripts/local-ca creates: cert-manager.cue asserts at render
	// time that its value is registered here.
	"cert-manager": _ambient: true

	// cnpg-system hosts the CloudNativePG operator (the controller-manager
	// Deployment and its webhook Service); its workloads enroll in the
	// ambient mesh per the platform convention for controller namespaces,
	// like cert-manager.
	//
	// Keep this name in sync with CnpgNamespace in
	// components/cnpg/cnpg.cue: that file is an ancestor only of the cnpg
	// leaf components, so it cannot be referenced from here.  cnpg.cue
	// asserts at render time that its value is registered here.
	"cnpg-system": _ambient: true

	// echo is the permanent Layer 0 smoke-test namespace; its workloads
	// enroll in the ambient mesh per the platform convention.
	echo: _ambient: true
}

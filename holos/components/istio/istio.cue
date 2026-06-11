package holos

// This file is a CUE ancestor of the four istio leaf components (base,
// istiod, cni, ztunnel), so each component instance includes it without
// imports.  It holds the single Istio version pin and the Helm values shared
// by all four charts.

// IstioVersion pins the Istio Helm chart version for all four istio
// components.  Istio implements the Gateway API; the gateway-api component
// pins the standard channel CRDs at 1.4.1, and per the Istio 1.28 change
// notes ("Upgraded Gateway API support to v1.4") every supported Istio
// release (1.28+) supports Gateway API v1.4.  1.29 is in the support window
// per https://istio.io/latest/docs/releases/supported-releases/ (checked
// 2026-06-11; the 1.29 patch line is actively maintained).  Re-check the
// supported-releases page and the target minor's change notes for Gateway
// API compatibility before bumping.
IstioVersion: "1.29.2"

// IstioValues constrains the Helm values shared by all four istio charts.
// profile: "ambient" selects ambient mode (ztunnel data plane, no sidecars).
// Keep these minimal for the local k3d cluster — production concerns like
// resource limits, autoscaling, and node placement do not belong here.
IstioValues: {
	profile: "ambient"
}

// IstioNamespace is the control plane namespace.  Keep the chart default
// (istio-system); this platform has no environment dimension to encode in
// the namespace name.  The base component emits the Namespace resource.
//
// Keep this value in sync with the "istio-system" entry in the central
// namespaces registry (holos/namespaces.cue): this file is an ancestor only
// of the istio leaf components, so the registry at the holos root cannot
// reference it — the two literal values must match.  The registry IS an
// ancestor of the istio leaf instances, so the constraint below checks
// membership in this direction: IstioNamespace must unify with one of the
// registered namespace names, turning silent drift between the two literals
// into a render failure.
IstioNamespace: "istio-system"
IstioNamespace: or([for NAME, _ in namespaces {NAME}])

// IstioRepository is the upstream Helm chart repository for all four charts.
IstioRepository: {
	name: "istio"
	url:  "https://istio-release.storage.googleapis.com/charts"
}

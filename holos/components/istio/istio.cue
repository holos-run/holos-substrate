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
//
// The istiod component passes IstioValues verbatim as the istiod chart's Helm
// values (components/istio/istiod/buildplan.cue), so meshConfig set here flows
// into the mesh's MeshConfig.  The other three charts (base, cni, ztunnel)
// ignore meshConfig, so declaring it once here is harmless for them.
IstioValues: {
	profile: "ambient"

	// meshConfig.extensionProviders registers the Holos Authenticator (ADR-23) as
	// a named Envoy ext_authz gRPC provider.  An AuthorizationPolicy with
	// action: CUSTOM and provider.name: "holos-authenticator"
	// (components/holos-authenticator) references this provider by name to send
	// the authorization decision to the authenticator's gRPC server.  The service
	// is the holos-authenticator Service in the holos-authenticator namespace; the
	// port is the manager's gRPC bind address (:9000).  L7 ext_authz enforcement
	// in ambient mode requires a waypoint in front of the protected workload
	// (ztunnel is L4-only) — the in-cluster wiring is declared here; the full
	// waypoint/ServiceEntry egress topology for an external API-server target is a
	// deferred follow-up (holos/docs/placeholders.md, finalized next phase).
	meshConfig: extensionProviders: [{
		name: "holos-authenticator"
		envoyExtAuthzGrpc: {
			service: "holos-authenticator.holos-authenticator.svc.cluster.local"
			port:    9000
			timeout: "2s"
		}
	}]
}

// IstioNamespace is the control plane namespace.  Keep the chart default
// (istio-system); this platform has no environment dimension to encode in
// the namespace name.  The namespaces component renders the Namespace
// resource from the central registry (holos/namespaces.cue).
//
// Keep this value in sync with the "istio-system" entry in the central
// namespaces registry (holos/namespaces.cue): this file is an ancestor only
// of the istio leaf components, so the registry at the holos root cannot
// reference it — the two literal values must match.  The registry IS an
// ancestor of the istio leaf instances, so the constraint below checks
// membership in this direction: IstioNamespace must unify with
// #RegisteredNamespace (holos/namespaces.cue), turning silent drift between
// the two literals into a render failure.
IstioNamespace: "istio-system"
IstioNamespace: #RegisteredNamespace

// IstioRepository is the upstream Helm chart repository for all four charts.
IstioRepository: {
	name: "istio"
	url:  "https://istio-release.storage.googleapis.com/charts"
}

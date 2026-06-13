package holos

// coredns-custom emits the cluster-wide CoreDNS override that lets in-cluster
// workloads resolve the Keycloak issuer hostname auth.holos.localhost to the
// shared Istio ingress gateway, so the Argo CD OIDC backchannel
// (discovery/JWKS/token from argocd-server and repo-server) works without
// leaving the cluster.
//
// Why this exists: browsers reach Keycloak at https://auth.holos.localhost,
// which resolves to 127.0.0.1 on the host and enters the cluster through the
// shared Gateway (components/istio-gateway), where a DestinationRule
// re-encrypts to the Keycloak backend (components/keycloak/instance).  But
// .localhost never resolves to a routable address from inside a pod, so
// argocd-server's server-side OIDC calls to the same issuer would fail with a
// connection error.  k3s runs CoreDNS and supports an optional coredns-custom
// ConfigMap in kube-system whose *.server entries are imported into the
// Corefile (k3s CoreDNS Custom DNS feature).  The rewrite plugin below
// rewrites auth.holos.localhost to the in-cluster gateway Service FQDN, so the
// pod-side lookup lands on the same Gateway the browser path uses and the
// existing Gateway→Keycloak re-encrypt path serves
// https://auth.holos.localhost/realms/holos end-to-end — the iss claim then
// matches the issuer Argo CD is configured with (the Keycloak CR's
// hostname.hostname is https://auth.holos.localhost).  The local-CA backend
// cert on that hop is accepted via oidc.tls.insecure.skip.verify in the argocd
// controller buildplan; see its OIDC_CONFIG comment for the MVP TLS posture.
//
// This is a dedicated component rather than a generator in the argocd
// controller buildplan deliberately: the resource lives in kube-system and is
// cluster-DNS scoped (it serves any in-cluster client resolving the issuer
// hostname, not just Argo CD), and the argocd controller buildplan's Kustomize
// transformer forces namespace: argocd onto every resource it emits, which
// would rewrite this kube-system ConfigMap into the wrong namespace.  Keeping
// it standalone keeps its namespace literal and makes the apply-order
// dependency (it must apply before argocd, the consumer) explicit in
// scripts/apply.
//
// Future production work: a real cluster would resolve the issuer hostname
// through normal DNS (a public or split-horizon record for the gateway), so
// this override is local-k3d only — see the production deployment area
// placeholder in holos/docs/placeholders.md.

// The Keycloak issuer hostname (components/keycloak/instance HOSTNAME) and the
// Argo CD OIDC config issuer (components/argocd/controller OIDC_CONFIG).  In
// pod-side DNS this name has no routable address, so it is rewritten to the
// gateway Service below.
let ISSUER_HOSTNAME = "auth.holos.localhost"

// The shared Gateway is the Gateway API Gateway named "default" in
// istio-gateways (components/istio-gateway).  Istio's Gateway API deployment
// controller auto-provisions a Service named <gateway-name>-istio for it, so
// the in-cluster FQDN is default-istio.istio-gateways.svc.cluster.local.  The
// gateway terminates the *.holos.localhost listener and re-encrypts to
// Keycloak via the keycloak DestinationRule, so resolving the issuer hostname
// here routes the OIDC backchannel through the same path the browser uses.
let GATEWAY_SERVICE_FQDN = "default-istio.istio-gateways.svc.cluster.local"

// CoreDNS reads coredns-custom from kube-system on k3s.  kube-system is a
// pre-existing system namespace, not a platform-managed one, so it is NOT in
// the central namespaces registry (holos/namespaces.cue) and this ConfigMap
// does not unify its namespace with #RegisteredNamespace.
let CONFIG_MAP = {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: {
		name:      "coredns-custom"
		namespace: "kube-system"
	}
	// The *.server key names a CoreDNS server block imported into the
	// Corefile.  A single-name rewrite (exact match) maps the issuer hostname
	// to the gateway Service FQDN; CoreDNS then resolves that in-cluster name
	// normally and answers the pod with the gateway's ClusterIP.
	data: "auth.server": """
		\(ISSUER_HOSTNAME) {
		    rewrite name \(ISSUER_HOSTNAME) \(GATEWAY_SERVICE_FQDN)
		    forward . /etc/resolv.conf
		}

		"""
}

// The one-file-per-resource guardrail is satisfied CUE-natively: the single
// artifact is produced by a single Resources generator holding exactly one
// resource, so no Kustomize bundle or kubectl-slice transformer is needed (the
// local-ca precedent).
userDefinedBuildPlan: {
	metadata: name: "coredns-custom"
	spec: artifacts: manifests: {
		// configmap-<name>.yaml matches the kubectl-slice naming convention
		// used everywhere else in the deploy tree.
		"clusters/\(clusterName)/components/\(metadata.name)/configmap-\(CONFIG_MAP.metadata.name).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				// Unify with #Resources (holos/resources.cue) so the ConfigMap
				// validates against the vendored Kubernetes schemas at render
				// time.
				resources: #Resources & {
					ConfigMap: (CONFIG_MAP.metadata.name): CONFIG_MAP
				}
			}]
		}
	}
}

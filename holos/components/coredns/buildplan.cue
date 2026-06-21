package holos

// coredns makes the platform's public *.holos.internal ingress hostnames
// resolve authoritatively from inside the cluster, so in-cluster relying
// parties (Argo CD, Kargo, Quay, and any musl-linked workload) reach a
// service by the same public hostname a browser uses.
//
// Why this component exists (HOL-1364, the .internal migration):
//
//   The platform's public hostnames used to live on the .localhost TLD.
//   .localhost is reserved for loopback (RFC 6761), so resolvers — the host
//   stub resolver, musl libc inside Alpine pods, and Istio ztunnel's DNS
//   proxy for ambient-enrolled namespaces — short-circuit *.localhost to
//   127.0.0.1/::1 *in-process*, before the query ever reaches CoreDNS.  An
//   in-cluster client therefore could not resolve auth.holos.localhost to
//   anything useful (the root cause behind HOL-1360: Keycloak could not
//   resolve its own issuer/JWKS URL from a musl-linked workload).  Migrating
//   to .internal (an ICANN-reserved private-use TLD with no special resolver
//   behavior — musl, glibc, and Go all issue an ordinary DNS query) lets
//   CoreDNS answer the public hostname authoritatively.  No public-CA
//   constraint applies to .internal, so the existing local-ca ClusterIssuer
//   continues to sign *.holos.internal certificates unchanged.
//
// Mechanism: k3s deploys CoreDNS with `import /etc/coredns/custom/*.server`
// in its Corefile and mounts a ConfigMap named `coredns-custom` in
// kube-system at /etc/coredns/custom.  Each key ending in `.server` becomes
// an additional CoreDNS server block.  The holos.internal.server block below
// rewrites every *.holos.internal query to the shared Istio gateway Service
// FQDN (GATEWAY_SERVICE) and lets the in-cluster kubernetes plugin resolve
// that to the gateway's ClusterIP — tracking the Service by name so the entry
// survives ClusterIP changes.  Clients then connect to the gateway, which
// terminates TLS for *.holos.internal and routes by SNI/Host to the matching
// HTTPRoute, exactly as host-side traffic does.
//
// Relationship to the per-namespace ServiceEntries (quay/argocd/kargo): those
// remain in place in this phase (conservative scope — they predate CoreDNS
// resolution and are verified working).  With .internal resolving through
// CoreDNS they are likely redundant for ambient-enrolled pods; removing them
// is a deliberate follow-up once CoreDNS resolution is verified end-to-end.

// The shared Istio gateway Service the istio-gateway component's Gateway
// auto-provisions: "<gateway>-istio" in the istio-gateways namespace is
// Istio's gateway auto-deployment naming convention (GATEWAY_NAME "default").
let GATEWAY_SERVICE = "default-istio.istio-gateways.svc.cluster.local"

// CoreDNS rewrites every *.holos.internal query to the gateway Service FQDN
// and forwards it to the cluster's primary CoreDNS server on loopback, which
// resolves the in-cluster Service name via its kubernetes plugin.  `rewrite …
// answer auto` rewrites the response name back to the queried *.holos.internal
// name so the client sees a consistent answer.  Resolving by Service name (not
// a pinned ClusterIP) keeps the entry valid across gateway ClusterIP changes.
let COREFILE = """
	holos.internal:53 {
	    errors
	    rewrite continue {
	        name regex (.*)\\.holos\\.internal \(GATEWAY_SERVICE)
	        answer auto
	    }
	    forward . 127.0.0.1:53
	    cache 30
	}

	"""

let CONFIGMAP = {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: {
		name: "coredns-custom"
		// kube-system is the k3s control-plane namespace where CoreDNS runs
		// and reads its coredns-custom ConfigMap.  It is a pre-existing k3s
		// namespace, not one the platform creates, so it is intentionally NOT
		// drawn from the central namespaces registry (#RegisteredNamespace).
		namespace: "kube-system"
	}
	data: "holos.internal.server": COREFILE
}

userDefinedBuildPlan: {
	metadata: name: "coredns"
	spec: artifacts: manifests: {
		// One file per resource, matching the kubectl-slice naming convention
		// used across the deploy tree.  A single Resources generator holding
		// exactly one resource needs no Kustomize/kubectl-slice transformer.
		"clusters/\(clusterName)/components/\(metadata.name)/configmap-\(CONFIGMAP.metadata.name).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: #Resources & {
					ConfigMap: (CONFIGMAP.metadata.name): CONFIGMAP
				}
			}]
		}
	}
}

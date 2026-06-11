package holos

// istio-gateway emits the shared Gateway API Gateway all platform services
// attach HTTPRoutes to.  Istio's gateway controller auto-provisions the
// gateway Deployment and LoadBalancer Service in istio-gateways; on k3s,
// klipper ServiceLB binds the Service ports on the node and k3d/config.yaml
// maps host ports 80/443 to the k3d loadbalancer.
//
// The istio-gateways Namespace is registered in the central namespaces
// registry (holos/namespaces.cue), which carries the canonical rationale
// for why it is deliberately NOT enrolled in the ambient mesh.
//
// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure:
// if "istio-gateways" is ever removed or renamed in holos/namespaces.cue,
// rendering fails here instead of at apply time with a NotFound namespace
// error.
let NAMESPACE = "istio-gateways" & #RegisteredNamespace

let GATEWAY = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "Gateway"
	metadata: {
		name:      "default"
		namespace: NAMESPACE
	}
	spec: {
		gatewayClassName: "istio"
		// The HTTPS/443 listener is added by HOL-1116, the cert-manager
		// issue, which owns TLS certificate provisioning for the gateway.
		listeners: [{
			name: "http"
			port: 80
			// *.holos.localhost is the local k3d-holos cluster's domain
			// (docs/local-cluster.md).  When a production cluster is
			// registered, parameterize the hostname per cluster — see the
			// production deployment area placeholder in
			// holos/docs/placeholders.md.
			hostname: "*.holos.localhost"
			protocol: "HTTP"
			// Any platform namespace may attach HTTPRoutes to the shared
			// Gateway.  Acceptable while every namespace is platform-managed;
			// tighten to a label Selector before tenant namespaces land — see
			// the shared Gateway route-attachment policy placeholder in
			// holos/docs/placeholders.md.
			allowedRoutes: namespaces: from: "All"
		}]
	}
}

userDefinedBuildPlan: {
	metadata: name: "istio-gateway"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored resources validate against the vendored
				// Kubernetes and Gateway API schemas at render time.
				resources: #Resources & {
					Gateway: (GATEWAY.metadata.name): GATEWAY
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: resources: inputs
				},
				{
					kind: "Command"
					inputs: [transformers[0].output]
					// this output is the artifact holos writes to the deploy
					// directory, one file per resource.
					output: artifact
					command: args: ["holos", "kubectl-slice", "-f", "\(BuildContext.tempDir)/\(inputs[0])", "-o", "\(BuildContext.tempDir)/\(artifact)"]
				},
			]
		}
	}
}

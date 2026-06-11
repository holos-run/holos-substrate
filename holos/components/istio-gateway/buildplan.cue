package holos

// istio-gateway emits the shared Gateway API Gateway all platform services
// attach HTTPRoutes to, plus its istio-gateways Namespace.  Istio's gateway
// controller auto-provisions the gateway Deployment and LoadBalancer Service
// in istio-gateways; on k3s, klipper ServiceLB binds the Service ports on the
// node and k3d/config.yaml maps host ports 80/443 to the k3d loadbalancer.
//
// The istio-gateways Namespace is deliberately NOT enrolled in ambient (no
// istio.io/dataplane-mode=ambient label): the auto-provisioned gateway pods
// are Envoy proxies themselves and terminate mesh traffic natively, so
// redirecting them through ztunnel adds nothing.
let NAMESPACE = "istio-gateways"

let GATEWAY = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "Gateway"
	metadata: {
		name:      "default"
		namespace: NAMESPACE
	}
	spec: {
		gatewayClassName: "istio"
		// The HTTPS/443 listener is added by the cert-manager issue, which
		// owns TLS certificate provisioning for the gateway.
		listeners: [{
			name:     "http"
			port:     80
			protocol: "HTTP"
			hostname: "*.holos.localhost"
			// Any platform namespace may attach HTTPRoutes to the shared
			// Gateway.
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
				resources: {
					Namespace: (NAMESPACE): {
						apiVersion: "v1"
						kind:       "Namespace"
						metadata: name: NAMESPACE
					}
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

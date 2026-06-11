package holos

// echo is a permanent smoke test for the Layer 0 traffic path, not a
// throwaway: it stays registered so the full path — host 80 → k3d serverlb →
// klipper ServiceLB → shared Gateway → HTTPRoute → ambient-enrolled workload
// — is re-verifiable after any Layer 0 change.  It emits an echo Namespace
// enrolled in the ambient mesh, a Deployment running a trivial echo server, a
// Service, and an HTTPRoute attached to the shared Gateway (istio-gateway
// component).
//
// The echo Namespace carries the istio.io/dataplane-mode=ambient label per
// the platform convention documented in holos/docs/mesh-enrollment.md:
// platform namespaces carrying workloads MUST be enrolled in the ambient
// mesh.

// VERSION pins the agnhost echo image tag.  agnhost is the upstream
// Kubernetes e2e test image: multi-arch (arm64 required — the cluster is k3d
// on OrbStack/Apple silicon), dependency-light, and maintained by
// sig-testing; its netexec subcommand echoes request info over HTTP.
// Check https://github.com/kubernetes/kubernetes/tree/master/test/images/agnhost
// for the current tag before bumping; any tag works for the smoke test as
// long as it remains multi-arch.
let VERSION = "2.53"

let NAMESPACE = "echo"
let NAME = "echo"
let PORT = 8080

let METADATA = {
	name:      NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": NAME
}

userDefinedBuildPlan: {
	metadata: name: "echo"
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
					Namespace: (NAMESPACE): {
						apiVersion: "v1"
						kind:       "Namespace"
						metadata: {
							name: NAMESPACE
							// Enroll every workload in this namespace in the
							// Istio ambient mesh; ztunnel captures their
							// traffic over HBONE.  See
							// holos/docs/mesh-enrollment.md.
							labels: "istio.io/dataplane-mode": "ambient"
						}
					}

					Deployment: (NAME): {
						apiVersion: "apps/v1"
						kind:       "Deployment"
						metadata:   METADATA
						spec: {
							replicas: 1
							selector: matchLabels: METADATA.labels
							template: {
								metadata: labels: METADATA.labels
								spec: containers: [{
									name:  NAME
									image: "registry.k8s.io/e2e-test-images/agnhost:\(VERSION)"
									// netexec serves HTTP endpoints that echo
									// request info (e.g. /hostname returns the
									// pod name), proving which pod answered.
									args: ["netexec", "--http-port=\(PORT)"]
									ports: [{
										name:          "http"
										containerPort: PORT
										protocol:      "TCP"
									}]
								}]
							}
						}
					}

					Service: (NAME): {
						apiVersion: "v1"
						kind:       "Service"
						metadata:   METADATA
						spec: {
							selector: METADATA.labels
							ports: [{
								name:       "http"
								port:       PORT
								targetPort: PORT
								protocol:   "TCP"
							}]
						}
					}

					// Cross-namespace attachment to the shared Gateway is
					// allowed because its listener sets
					// allowedRoutes.namespaces.from: All (istio-gateway
					// component).  echo.holos.localhost matches the listener
					// hostname *.holos.localhost and resolves to 127.0.0.1 on
					// the host per docs/local-cluster.md.
					HTTPRoute: (NAME): {
						apiVersion: "gateway.networking.k8s.io/v1"
						kind:       "HTTPRoute"
						metadata:   METADATA
						spec: {
							parentRefs: [{
								name:      "default"
								namespace: "istio-gateways"
							}]
							hostnames: ["echo.holos.localhost"]
							rules: [{
								backendRefs: [{
									name: NAME
									port: PORT
								}]
							}]
						}
					}
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

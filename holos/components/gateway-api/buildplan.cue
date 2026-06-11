package holos

// VERSION pins the Gateway API standard channel CRDs.  Istio implements the
// Gateway API; pick a version supported by the Istio release targeted by
// HOL-1115.  Per https://istio.io/latest/docs/releases/supported-releases/
// the supported Istio releases (1.28+) all support Gateway API v1.4 — the
// Istio 1.28 change notes state "Upgraded Gateway API support to v1.4."
// Re-check the chosen Istio minor's release notes before bumping.
let VERSION = "1.4.1"

userDefinedBuildPlan: {
	metadata: name: "gateway-api"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind: "Command"
				command: {
					// read-thru-cache fetches the standard channel CRDs once and
					// caches them in manifests/bundle.<VERSION>.yaml for offline
					// reproducible rendering.  The path derives from BuildContext
					// so it tracks the component directory regardless of the
					// command working directory or a metadata.name override.
					args: ["\(BuildContext.rootDir)/\(BuildContext.leafDir)/read-thru-cache", VERSION]
					isStdoutOutput: true
				}
				output: "crds-bundle.yaml"
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

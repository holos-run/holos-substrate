package holos

// argocd-crds renders the Argo CD CRDs (applications, applicationsets,
// appprojects in group argoproj.io) from the upstream source tree at the
// pinned app version.  CRDs are isolated from the controller component per
// the component guidelines and labeled crds: "true" at the registration
// site so they apply before the controllers that depend on them.  The
// controller component disables the chart's CRD templates
// (crds.install: false) so these manifests are the only source of the
// Argo CD CRDs.  The version pin lives in ../argocd.cue, shared with the
// controller component.
userDefinedBuildPlan: {
	metadata: name: "argocd-crds"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind: "Command"
				command: {
					// read-thru-cache fetches the CRD manifests once and
					// caches them in manifests/bundle.<VERSION>.yaml for
					// offline reproducible rendering.  The path derives from
					// BuildContext so it tracks the component directory
					// regardless of the command working directory or a
					// metadata.name override.
					args: ["\(BuildContext.rootDir)/\(BuildContext.leafDir)/read-thru-cache", ArgoCDAppVersion]
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

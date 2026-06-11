package holos

// istio-base renders the Istio base chart: the Istio CRDs and the
// validation webhook configuration.  The istio-system Namespace the istiod,
// cni, and ztunnel charts assume exists is registered in the central
// namespaces registry (holos/namespaces.cue) and rendered by the namespaces
// component.  This component is labeled crds: "true" at the registration
// site so it applies before the controllers that depend on the CRDs.  The
// version pin and shared ambient values live in ../istio.cue.
userDefinedBuildPlan: {
	metadata: name: "istio-base"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [
				{
					kind:   "Helm"
					output: "helm-output.yaml"
					helm: {
						namespace: IstioNamespace
						chart: {
							name:       "base"
							version:    IstioVersion
							release:    "istio-base"
							repository: IstioRepository
						}
						values: IstioValues
					}
				},
			]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						// Forces istio-system onto every namespaced resource.  The
						// charts emit nothing destined for another namespace today;
						// re-verify that assumption when bumping IstioVersion.
						namespace: IstioNamespace
						resources: inputs
					}
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

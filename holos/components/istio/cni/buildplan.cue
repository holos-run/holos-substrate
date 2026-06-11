package holos

// istio-cni renders the Istio CNI node agent chart in ambient mode.  The
// version pin and shared ambient values live in ../istio.cue.
userDefinedBuildPlan: {
	metadata: name: "istio-cni"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Helm"
				output: "helm-output.yaml"
				helm: {
					namespace: IstioNamespace
					chart: {
						name:       "cni"
						version:    IstioVersion
						release:    "istio-cni"
						repository: IstioRepository
					}
					values: IstioValues & {
						// k3d runs k3s, which uses nonstandard locations for CNI
						// configuration and binaries.  The single registered
						// cluster is k3d, so set the paths unconditionally; gate
						// them behind a cluster tag when a non-k3s cluster is
						// registered.  See
						// https://istio.io/latest/docs/ambient/install/platform-prerequisites/#k3d
						cni: {
							cniConfDir: "/var/lib/rancher/k3s/agent/etc/cni/net.d"
							cniBinDir:  "/bin"
						}
					}
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
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

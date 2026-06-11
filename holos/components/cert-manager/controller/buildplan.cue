package holos

// cert-manager renders the controller, webhook, and cainjector from the
// upstream Helm chart.  The CRDs are deliberately NOT rendered here — they
// are isolated in the sibling crds component per the component guidelines.
// The version pin and shared names live in ../cert-manager.cue.
userDefinedBuildPlan: {
	metadata: name: "cert-manager"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Helm"
				output: "helm-output.yaml"
				helm: {
					namespace: CertManagerNamespace
					chart: {
						name: "cert-manager"
						// Upstream chart versions carry a leading v
						// (e.g. v1.19.5).
						version:    "v\(CertManagerVersion)"
						release:    "cert-manager"
						repository: CertManagerRepository
					}
					values: {
						// CRDs are isolated in the cert-manager-crds component.
						crds: enabled: false
						// startupapicheck is a Helm post-install hook Job that
						// polls the webhook once after install.  Disabled: this
						// platform applies rendered manifests declaratively, and
						// a hook Job re-applies poorly (Job specs are immutable
						// and the Job is deleted after its TTL).  Readiness is
						// the apply tooling's concern.
						startupapicheck: enabled: false
						// The chart defaults leader election to kube-system.
						// Keep it in the release namespace instead so the chart
						// emits nothing destined for another namespace — the
						// Kustomize transformer below forces the release
						// namespace onto every namespaced resource.
						global: leaderElection: namespace: CertManagerNamespace
					}
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						// Forces cert-manager onto every namespaced resource.  The
						// chart emits nothing destined for another namespace today
						// (leader election is pinned to the release namespace in
						// values above); re-verify that assumption when bumping
						// CertManagerVersion.
						namespace: CertManagerNamespace
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

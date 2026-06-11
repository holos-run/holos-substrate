package holos

// namespaces renders the central namespaces registry (holos/namespaces.cue)
// as one manifest artifact per Namespace resource.  The one-file-per-resource
// guardrail (holos/docs/component-guidelines.md) is satisfied CUE-natively:
// each artifact is produced by a single Resources generator holding exactly
// one Namespace, so no Kustomize bundle or kubectl-slice Command transformer
// is needed — there is never a multi-resource bundle to slice.
userDefinedBuildPlan: {
	metadata: name: "namespaces"
	spec: artifacts: manifests: {
		for NAME, NS in namespaces {
			// namespace-<name>.yaml matches the kubectl-slice naming
			// convention used everywhere else in the deploy tree.
			"clusters/\(clusterName)/components/\(metadata.name)/namespace-\(NAME).yaml": {
				artifact: _
				generators: [{
					kind:   "Resources"
					output: artifact
					// Unify with #Resources (holos/resources.cue) so the
					// registry entries validate against the vendored
					// Kubernetes schemas at render time.
					resources: #Resources & {
						Namespace: (NAME): NS
					}
				}]
			}
		}
	}
}

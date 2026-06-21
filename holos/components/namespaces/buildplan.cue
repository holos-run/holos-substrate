package holos

// namespaces renders the central namespaces registry (holos/namespaces.cue)
// as one manifest artifact per Namespace resource.  The one-file-per-resource
// guardrail (holos/docs/component-guidelines.md) is satisfied CUE-natively:
// each artifact is produced by a single Resources generator holding exactly
// one Namespace, so no Kustomize bundle or kubectl-slice Command transformer
// is needed — there is never a multi-resource bundle to slice.
userDefinedBuildPlan: {
	metadata: name: "namespaces"

	// _ validates the project/app collections on the RENDER path.  holos render
	// platform evaluates this BuildPlan but not arbitrary hidden ancestor fields,
	// so the collection contract (ownerless projects, dangling app→project
	// references, malformed app names/images) is tied here to an always-rendered
	// component by referencing #CollectionsValidated (holos/collections.cue).
	// The reference is what forces the validation; the field carries no manifest
	// data (it unifies the empty struct), so it does not affect the output.  This
	// component is the natural anchor: it already derives the project namespaces
	// from the same `projects` collection.
	_collectionsValidated: #CollectionsValidated

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

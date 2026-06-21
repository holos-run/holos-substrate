package holos

// namespaces renders the central namespaces registry (holos/namespaces.cue)
// as one manifest artifact per Namespace resource.  The one-file-per-resource
// guardrail (holos/docs/component-guidelines.md) is satisfied CUE-natively:
// each artifact is produced by a single Resources generator holding exactly
// one Namespace, so no Kustomize bundle or kubectl-slice Command transformer
// is needed — there is never a multi-resource bundle to slice.
userDefinedBuildPlan: {
	metadata: name: "namespaces"

	// _collectionsValidated puts the project/app collection contract on the
	// holos render path.  holos render platform evaluates this BuildPlan, so
	// referencing #CollectionsValidated (holos/collections.cue) here forces every
	// collection constraint that produces a _|_ — a dangling app→project
	// reference (conflict), a malformed app/project name or empty image/bad port
	// (out of bound), and an ownerless project — to fail the render.  The field is
	// hidden and unifies the empty struct, so it adds NOTHING to the rendered
	// Namespace manifests (and emits no namespaced resource into this
	// bootstrap-ordering component — see the note in collections.cue on why the
	// missing-required-field case is enforced at consumption, not here).  This
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

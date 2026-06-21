package holos

// namespaces renders the central namespaces registry (holos/namespaces.cue)
// as one manifest artifact per Namespace resource.  The one-file-per-resource
// guardrail (holos/docs/component-guidelines.md) is satisfied CUE-natively:
// each artifact is produced by a single Resources generator holding exactly
// one Namespace, so no Kustomize bundle or kubectl-slice Command transformer
// is needed — there is never a multi-resource bundle to slice.
userDefinedBuildPlan: {
	metadata: name: "namespaces"

	// _collectionsValidated puts the _|_-producing half of the project/app
	// collection contract on the holos render path.  holos render platform
	// evaluates this BuildPlan, so referencing #CollectionsValidated
	// (holos/collections.cue) from this hidden field forces every constraint that
	// produces a _|_ — an ownerless project, a dangling app→project reference
	// (conflict), and a malformed app/project name / empty image / out-of-range
	// port (out of bound) — to fail the render.  The field is hidden, so it adds
	// NOTHING to the rendered Namespace manifests and emits no namespaced resource
	// into this bootstrap-ordering component.
	//
	// The remaining half — a MISSING required app field (incomplete, not _|_,
	// which a hidden reference tolerates) — is forced by EXPORT, not this
	// reference: holos/namespaces.cue folds each app's #CollectionsValidated.tokens
	// interpolation into its project's prod-<name> control-namespace annotation,
	// and exporting an interpolation of an incomplete value is a render error.
	// Both halves live in this always-rendered component because it already
	// derives the project namespaces from the same `projects`/`apps` collections.
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

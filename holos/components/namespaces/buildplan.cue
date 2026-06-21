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

		// Collection-validation manifest.  This component is the anchor that puts
		// the project/app collection contract on the holos render path: holos
		// EXPORTS this ConfigMap's data, and the data is built from
		// #CollectionsValidated (holos/collections.cue) — the per-project
		// owners-ok bools and the per-app interpolated tokens.  Exporting forces
		// every project/app entry to be valid and CONCRETE: an ownerless project
		// (false), a missing required app field (interpolation of an incomplete
		// value), a dangling app→project reference (conflict), or a malformed
		// name/image/port (out of bound) each fails the render here.  The
		// ConfigMap lands in the holos-controller namespace (an always-registered
		// platform namespace) and is deterministic, so the deploy tree stays
		// diff-clean; it documents, in-cluster, the validated collection state.
		"clusters/\(clusterName)/components/\(metadata.name)/collections-validated.yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: #Resources & {
					ConfigMap: "holos-collections-validated": {
						metadata: {
							name:      "holos-collections-validated"
							namespace: "holos-controller"
							labels: "app.holos.run/collection": "validated"
						}
						// project.<name> = "owners-ok" only when the project has
						// >0 owners (#CollectionsValidated.ownersOk is true);
						// app.<name> = the interpolated required-field token.  Both
						// reference #CollectionsValidated, so exporting this data
						// concretizes and validates every collection entry.
						data: {
							for PNAME, OK in #CollectionsValidated.ownersOk {
								// OK is `true` for a valid project and _|_ (render
								// error) for an ownerless one, so exporting it here
								// fails the render on an ownerless project.
								"project.\(PNAME)": "\(OK)"
							}
							for ANAME, TOK in #CollectionsValidated.tokens {
								"app.\(ANAME)": TOK
							}
						}
					}
				}
			}]
		}
	}
}

package holos

import "path"

// cnpg-crds renders the CloudNativePG CRDs from the upstream release
// manifest.  CRDs are isolated from the operator component per the component
// guidelines and labeled crds: "true" at the registration site so they apply
// before the controllers that depend on them.  The version pin lives in
// ../cnpg.cue, shared with the operator component.
//
// Upstream publishes a single release manifest carrying both the CRDs and
// the operator — there is no separate CRDs-only asset like cert-manager's —
// so the shared ../read-thru-cache caches the whole bundle once and each
// leaf component takes its half by kind with ../filter-kinds.
userDefinedBuildPlan: {
	metadata: name: "cnpg-crds"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			// The shared fetch and filter scripts live in the parent cnpg
			// directory, a CUE ancestor of both leaf components, so the
			// cached bundle is committed exactly once.  The path derives
			// from BuildContext so it tracks the component directory
			// regardless of the command working directory or a
			// metadata.name override.
			let SHARED_DIR = path.Dir(BuildContext.leafDir)
			generators: [{
				kind: "Command"
				command: {
					// read-thru-cache fetches the release manifest once and
					// caches it in ../manifests/bundle.<VERSION>.yaml for
					// offline reproducible rendering.
					args: ["\(BuildContext.rootDir)/\(SHARED_DIR)/read-thru-cache", CnpgVersion]
					isStdoutOutput: true
				}
				output: "bundle.yaml"
			}]
			transformers: [
				{
					// Select only the CustomResourceDefinition documents;
					// the operator component takes the complement.
					kind: "Command"
					inputs: [for G in generators {G.output}]
					output: "crds-bundle.yaml"
					command: {
						args: ["\(BuildContext.rootDir)/\(SHARED_DIR)/filter-kinds", "include", "\(BuildContext.tempDir)/\(inputs[0])", "CustomResourceDefinition"]
						isStdoutOutput: true
					}
				},
				{
					kind: "Kustomize"
					inputs: [transformers[0].output]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: resources: inputs
				},
				{
					kind: "Command"
					inputs: [transformers[1].output]
					// this output is the artifact holos writes to the deploy
					// directory, one file per resource.
					output: artifact
					command: args: ["holos", "kubectl-slice", "-f", "\(BuildContext.tempDir)/\(inputs[0])", "-o", "\(BuildContext.tempDir)/\(artifact)"]
				},
			]
		}
	}
}

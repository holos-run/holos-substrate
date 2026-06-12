package holos

import "path"

// cnpg renders the CloudNativePG operator — the controller-manager
// Deployment, its RBAC, webhook Service, ConfigMap, and the admission
// webhook configurations — from the upstream release manifest.  The CRDs in
// the same manifest are isolated in the sibling crds component per the
// component guidelines; the Namespace resource is stripped because
// namespaces are registered centrally (the cnpg-system entry in
// holos/namespaces.cue), never emitted by components.  The version pin
// lives in ../cnpg.cue, shared with the crds component.
//
// The operator's validating and mutating webhooks for postgresql.cnpg.io
// resources fail closed, so scripts/apply gates on the controller-manager
// rollout before later components apply Cluster resources.  The webhook
// clientConfig carries no caBundle in the rendered manifests — the operator
// injects it at runtime — so a force re-apply never claims or strips it (the
// same shape as cert-manager's cainjector, see holos/README.md).
userDefinedBuildPlan: {
	metadata: name: "cnpg"
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
					// Drop the CustomResourceDefinition documents (the
					// sibling crds component ships them) and the Namespace
					// document (namespaces are registered centrally).
					kind: "Command"
					inputs: [for G in generators {G.output}]
					output: "operator-bundle.yaml"
					command: {
						args: ["\(BuildContext.rootDir)/\(SHARED_DIR)/filter-kinds", "exclude", "\(BuildContext.tempDir)/\(inputs[0])", "CustomResourceDefinition", "Namespace"]
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

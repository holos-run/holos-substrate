package holos

// keycloak-operator-crds renders the Keycloak operator CRDs
// (keycloaks.k8s.keycloak.org, keycloakrealmimports.k8s.keycloak.org) from
// the upstream keycloak-k8s-resources manifests.  CRDs are isolated from
// the operator component per the component guidelines and labeled
// crds: "true" at the registration site so they apply before the
// controllers that depend on them.  The version pin lives in
// ../keycloak.cue, shared with the operator component.
//
// Upstream publishes each CRD as its own single-CRD file with no combined
// asset, so this component's read-thru-cache fetches both files and caches
// them concatenated as one committed bundle — no kind filtering is needed
// (unlike cnpg, whose single upstream manifest mixes CRDs with the
// operator).
userDefinedBuildPlan: {
	metadata: name: "keycloak-operator-crds"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind: "Command"
				command: {
					// read-thru-cache fetches the two CRD manifests once and
					// caches them in manifests/bundle.<VERSION>.yaml for
					// offline reproducible rendering.  The path derives from
					// BuildContext so it tracks the component directory
					// regardless of the command working directory or a
					// metadata.name override.
					args: ["\(BuildContext.rootDir)/\(BuildContext.leafDir)/read-thru-cache", KeycloakVersion]
					isStdoutOutput: true
				}
				output: "crds-bundle.yaml"
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: resources: inputs
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

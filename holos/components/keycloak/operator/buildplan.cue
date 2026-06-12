package holos

// keycloak-operator renders the Keycloak operator — the operator Deployment,
// its ServiceAccount, RBAC, and metrics Service — from the upstream
// keycloak-k8s-resources kubernetes.yml manifest.  The CRDs are separate
// upstream files shipped by the sibling operator-crds component per the
// component guidelines.  The version pin and the deployment-method decision
// (operator over a plain StatefulSet/Deployment) live in ../keycloak.cue,
// shared with the operator-crds component.
//
// The upstream resources carry no namespace (upstream docs apply them with
// -n keycloak), so the Kustomize transformer sets
// kustomization.namespace: keycloak — placing every namespaced resource in
// the centrally registered keycloak namespace (holos/namespaces.cue) and
// aligning the (Cluster)RoleBinding ServiceAccount subjects with it.  The
// upstream manifest carries no Namespace resource; namespaces are never
// emitted by components, so if upstream ever adds one to kubernetes.yml the
// committed bundle diff will surface it in review — strip it with a filter
// step (see components/cnpg/filter-kinds for the precedent) rather than
// letting it through.
userDefinedBuildPlan: {
	metadata: name: "keycloak-operator"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind: "Command"
				command: {
					// read-thru-cache fetches kubernetes.yml once and caches
					// it in manifests/bundle.<VERSION>.yaml for offline
					// reproducible rendering.  The path derives from
					// BuildContext so it tracks the component directory
					// regardless of the command working directory or a
					// metadata.name override.
					args: ["\(BuildContext.rootDir)/\(BuildContext.leafDir)/read-thru-cache", KeycloakVersion]
					isStdoutOutput: true
				}
				output: "bundle.yaml"
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						resources: inputs
						// Place the namespace-less upstream resources in the
						// centrally registered keycloak namespace.
						namespace: KeycloakNamespace
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

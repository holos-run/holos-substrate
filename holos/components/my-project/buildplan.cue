package holos

// my-project is the Layer 3 sample application's delivery scaffold (HOL-1268).
// This first phase (HOL-1269) lays the foundation: it emits an Argo CD
// AppProject and an Argo CD Application that reconciles the rendered
// my-project-config manifests from an OCI artifact in the in-cluster Quay
// registry.  The Kargo Project, ProjectConfig (the quay webhook receiver),
// Warehouse, and the project-config promotion Stage that drives this
// Application's targetRevision land in the next phase (HOL-1270).
//
// The delivery loop, once HOL-1270 wires Kargo, mirrors the echo pipeline
// (components/kargo-echo) but with the Project namespace doubling as the
// workload namespace (see the my-project entry in holos/namespaces.cue):
//
//   scripts/publish → new my-project-config OCI artifact in Quay
//     → Warehouse discovers it → creates Freight
//       → project-config Stage's argocd-update step patches this Application's
//         spec.source.targetRevision = <new digest>
//           → Argo CD syncs the new rendered manifests into the my-project
//             namespace.
//
// The OCI artifact does not exist yet; the Application reports Unknown/Missing
// until scripts/publish pushes the first my-project-config artifact.  That is
// expected scaffolding for this phase.

// NAMESPACE is the my-project Kargo Project + workload namespace.  Unifying
// with #RegisteredNamespace (holos/namespaces.cue) turns silent drift between
// this literal and the registry entry into a render failure: if "my-project"
// is ever removed or renamed in the registry, rendering fails here instead of
// at apply time with a NotFound namespace error.
let NAMESPACE = "my-project" & #RegisteredNamespace

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).
// Both the AppProject and the Application are namespaced into it — Argo CD's
// project and application objects live alongside the controller.  Unifying with
// #RegisteredNamespace ties the literal to the registry.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// NAME is the shared name of the AppProject and the Application, and the anchor
// of the Kargo Project added in HOL-1270.
let NAME = "my-project"

// STAGE is the Kargo Stage (added in HOL-1270) authorized to patch this
// Application's targetRevision.  The kargo.akuity.io/authorized-stage
// annotation below references it as "<project>:<stage>"; without that
// annotation Kargo's argocd-update step refuses to touch the Application, so
// the value must match the Stage name the next phase creates.
let STAGE = "project-config"

// CONFIG_REPO is the my-project-config rendered-manifests OCI repository the
// client-side publish workflow pushes to.  The Argo CD Application OCI source
// takes the oci:// form below; the Warehouse subscription added in HOL-1270
// takes the bare registry/repo form, and Kargo's argocd-update matches the
// Application source by EXACT repoURL string, so the two forms must stay
// consistent.  Scoped under the my-project/ Quay org path so the AppProject can
// constrain sourceRepos to oci://quay.holos.localhost/my-project/*.
let CONFIG_REPO_OCI = "oci://quay.holos.localhost/my-project/my-project-config"

// APPPROJECT_RESOURCE is the Argo CD AppProject that scopes what the
// my-project Application may deploy.  It is intentionally minimal but
// functional for the local single-tenant cluster:
//   - sourceRepos restricts sources to the my-project Quay org path, so the
//     Application can only sync artifacts published under my-project/.
//   - destinations restricts deploys to the in-cluster API server, my-project
//     namespace — matching the Application's destination below.
//   - the permissive cluster/namespace resource whitelists (group "*", kind
//     "*") let the rendered my-project-config manifests carry any kind; the
//     destination namespace constraint is the real guard rail on a
//     single-tenant cluster.
let APPPROJECT_RESOURCE = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "AppProject"
	metadata: {
		name:      NAME
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		sourceRepos: ["oci://quay.holos.localhost/my-project/*"]
		destinations: [{
			server:    "https://kubernetes.default.svc"
			namespace: NAMESPACE
		}]
		clusterResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
		namespaceResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
	}
}

// APPLICATION_RESOURCE is the Argo CD Application Kargo patches (HOL-1270).  It
// is authored standalone here (an OCI-source Application) rather than through
// the userDefinedBuildPlan gitops projection, the same deliberate choice the
// echo pipeline makes (see components/kargo-echo/buildplan.cue): that
// projection emits a GIT-source Application, which is the wrong shape for
// Kargo's OCI argocd-update step to patch.
//
// The kargo.akuity.io/authorized-stage annotation authorizes the my-project
// Project's project-config Stage to modify this Application; without it Kargo's
// argocd-update step refuses to touch it.  The value format is <project>:<stage>.
//
// targetRevision carries a placeholder ("HEAD") in this committed manifest:
// Kargo's argocd-update step OWNS spec.source.targetRevision and patches it to
// each promoted Freight's digest from HOL-1270 onward.  The placeholder is a
// documented Kargo-managed field — Argo CD reports the Application Unknown until
// the first promotion overwrites it with a real artifact digest.  (The echo
// pipeline omits the field entirely to avoid a server-side-apply ownership war;
// this phase keeps a placeholder per the acceptance criteria, and HOL-1270
// revisits ownership when it wires the Stage.)
let APPLICATION_RESOURCE = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "Application"
	metadata: {
		name:      NAME
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": NAME
		annotations: "kargo.akuity.io/authorized-stage": "\(NAMESPACE):\(STAGE)"
	}
	spec: {
		project: NAME
		source: {
			repoURL: CONFIG_REPO_OCI
			// Kargo-managed placeholder revision; see the comment above.
			targetRevision: "HEAD"
			// The manifests sit at the tarball root (scripts/publish packages
			// the kustomize output flat), so the source path is ".".
			path: "."
		}
		destination: {
			server:    "https://kubernetes.default.svc"
			namespace: NAMESPACE
		}
		syncPolicy: {
			automated: {
				prune:    true
				selfHeal: true
			}
			// The my-project namespace is registered centrally and applied by
			// the namespaces component, so Argo CD must not try to create it.
			syncOptions: ["CreateNamespace=false"]
		}
	}
}

userDefinedBuildPlan: {
	metadata: name: "my-project"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored AppProject and Application validate against the
				// vendored Argo CD schemas at render time.
				resources: #Resources & {
					AppProject: (NAME):  APPPROJECT_RESOURCE
					Application: (NAME): APPLICATION_RESOURCE
				}
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

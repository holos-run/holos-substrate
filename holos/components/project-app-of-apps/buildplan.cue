package holos

// project-app-of-apps is the PER-PROJECT, control-plane/workload-split App-of-Apps
// (HOL-1382).  It SUPERSEDES the single global tenant App-of-Apps
// (components/projects, HOL-1377), splitting the one `projects-bootstrap` root
// that fanned out EVERY project's manifests into ONE pair of root Applications
// PER registered project, each pulling that project's OWN OCI config bundle
// (oci://quay.holos.internal/holos/<project>-config:dev) rather than the single
// shared holos-paas-config bundle.  This is the "clean cut line" HOL-1382 asks
// for: the PLATFORM App-of-Apps (components/app-of-apps) bootstraps the system,
// and each project is bootstrapped separately by its own per-project roots —
// built, pushed, and applied by scripts/apply-project-app-of-apps (control plane)
// and scripts/apply-project-workload-app-of-apps (workload).
//
// TWO roots per project — the control-plane / workload split (HOL-1382).  The
// application component (HOL-1356) already renders each app into two subtrees,
// control-plane/ and workload/; the project component (HOL-1355, split in
// HOL-1382) renders its own all-control-plane resources into control-plane/.  The
// per-project OCI bundle therefore carries, for project <name>:
//   clusters/<cluster>/components/project/<name>/control-plane/        (the project CRs)
//   clusters/<cluster>/components/application/<app>/control-plane/      (each app's CRs)
//   clusters/<cluster>/components/application/<app>/workload/           (each app's workload)
// and this component emits two roots that select disjoint halves of it:
//   - the CONTROL-PLANE root (<name>-control-plane) reconciles everything EXCEPT
//     the app workload (directory.exclude: **/workload/**) — the platform team's
//     half (AppProject, Argo CD Applications, Kargo control plane, Quay
//     Organization/Repository, Keycloak CRs, RBAC).  Applied by the platform via
//     scripts/apply-project-app-of-apps.
//   - the WORKLOAD root (<name>-workload) reconciles ONLY the app workload
//     (directory.include: **/workload/**) — the service owner's half (each app's
//     Deployment/Service/HTTPRoute/ConfigMap/SA/RoleBinding).  Applied by the
//     service owner via scripts/apply-project-workload-app-of-apps, ONLY after the
//     platform team has applied the control plane (the workload's namespace,
//     ServiceAccount references, and AppProject come from the control plane).
//
// Relationship to the per-app Kargo delivery.  The app's control plane includes a
// per-app Argo CD Application (<project>-<app>) whose source is the app's OWN
// <app>-config repo, the Kargo-driven digest-pinned delivery path
// (application/buildplan.cue).  That path stays DORMANT until a service owner
// publishes an <app>-config artifact; the WORKLOAD root here is the
// foundational-phase, App-of-Apps delivery of the same workload straight from the
// per-project bundle (the "scaffold envs, wire one delivery path" posture, ADR-21).
// The two are alternative delivery modes for the same objects, so a deployment
// enables ONE at a time — the workload root for the simple App-of-Apps path, or
// the per-app Kargo Application once <app>-config publishing is wired.  This
// component wires the workload root.
//
// Layout (the self-management-safe App-of-Apps shape).  The root Applications
// render into THIS component's per-project directory
// (clusters/<cluster>/components/project-app-of-apps/<name>/application-<name>-<role>.yaml),
// which is DELIBERATELY NOT part of any per-project OCI bundle (the bundle tars
// only the project/ and application/ subtrees — see scripts/apply-project-app-of-apps),
// so a root never reconciles itself.  The scripts apply the rendered root file
// directly with kubectl; Argo CD then owns continuous reconciliation of the
// project's half it points at.

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).  The
// canonical value lives in holos/components/argocd/argocd.cue, an ancestor only of
// the argocd leaf components, so this sibling unifies the literal with
// #RegisteredNamespace to tie it to the central namespaces registry (the same
// constraint the projects/app-of-apps components work around).
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// CONFIG_REPO_BASE is the OCI repository PREFIX the per-project bundles live under:
// the platform-owned `holos` Quay org.  Each project's bundle is
// <CONFIG_REPO_BASE>/<name>-config (distinct from the per-APP delivery repos at
// oci://quay.holos.internal/<project>/<app>-config, which are under each project's
// OWN org).  The projects AppProject's sourceRepos authorize EXACTLY these
// per-project bundle URLs — one entry per registered project, not a
// oci://quay.holos.internal/holos/* wildcard (argocd-projects, the exact-match
// discipline the platform AppProject uses) — and argocd-projects commits one
// credential-less repository registration Secret per bundle so each PUBLIC
// <name>-config bundle is pullable anonymously (insecure: "true" for the mkcert
// cert).  Keep it consistent with scripts/apply-project-app-of-apps's
// CONFIG_REPO_BASE.
let CONFIG_REPO_BASE = "oci://quay.holos.internal/holos"

// CONFIG_TAG is the mutable bootstrap tag the per-project roots pin themselves.
// Unlike the per-project/app Applications (components/project, components/application)
// that OMIT targetRevision so Kargo owns it, NO Kargo sits in THIS bootstrap path
// — the roots pin the tag directly (the same posture as the platform App-of-Apps).
let CONFIG_TAG = "dev"

// IN_CLUSTER is the in-cluster Kubernetes API server destination every Application
// targets.
let IN_CLUSTER = "https://kubernetes.default.svc"

// PROJECTS_PROJECT is the Argo CD AppProject the argocd-projects component emits
// for tenant delivery.  Both per-project roots are assigned to it: it permits the
// argocd destination (the per-project AppProject + per-project/app Applications
// land there), the cluster-scoped Kargo Project the project control plane emits,
// and — widened in HOL-1382 — the oci://quay.holos.internal/holos/* per-project
// bundle sourceRepos.  Only these platform-trusted roots and the manifests they
// deliver are assigned project: projects; tenant artifacts use their own
// per-project AppProjects (project: <project>).
let PROJECTS_PROJECT = "projects"

// COMPONENTS_BASE is the deploy-tree subpath the rendered manifests live under,
// matching the OCI bundle layout (oci-publish-workflow.md): the per-project bundle
// tar is rooted at holos/deploy/, so member paths begin at clusters/.  Each root's
// source.path is COMPONENTS_BASE and Argo CD recurses it.
let COMPONENTS_BASE = "clusters/\(clusterName)/components"

// WORKLOAD_GLOB is the directory glob (doublestar semantics, matched against paths
// under source.path) selecting the per-app WORKLOAD subtree
// (application/<app>/workload/<file>).  The control-plane root EXCLUDES it (so it
// reconciles only the control plane); the workload root INCLUDES it (so it
// reconciles only the workload).  The project component emits no workload subtree,
// so excluding the glob leaves the project's control plane intact and including it
// selects nothing from the project subtree — exactly the split intended.
let WORKLOAD_GLOB = "**/workload/**"

// SYNC_OPTIONS are applied to every root so Argo CD's reconciliation matches how
// scripts/apply-projects applies the same manifests (server-side apply, and no
// tenant Application creates a namespace — the namespaces component owns them).
let SYNC_OPTIONS = [
	"ServerSideApply=true",
	"CreateNamespace=false",
]

// #ProjectRoot builds ONE per-project root Argo CD Application for one half
// (role) of the project's resources.  inputs are builder parameters; out is the
// resulting Application kept in a separate field so the builder's parameters never
// leak into the rendered resource (the #Resources.Application binding is closed).
#ProjectRoot: {
	inputs: {
		// project is the registered project name; the bundle repo and root names
		// derive from it.
		project: string
		// role is "control-plane" or "workload" — the half of the bundle this root
		// reconciles, and the suffix on the root Application's name.
		role: "control-plane" | "workload"
		// directory is the source.directory selector: {recurse, exclude?|include?}.
		// The control-plane root excludes the workload glob; the workload root
		// includes it.
		directory: {...}
	}
	out: {
		apiVersion: "argoproj.io/v1alpha1"
		kind:       "Application"
		metadata: {
			// <project>-<role> — globally unique in the shared argocd namespace:
			// project names are unique, "control-plane"/"workload" are reserved app
			// names (application/buildplan.cue), so neither collides with the
			// project-level Application (<project>) nor a per-app Application
			// (<project>-<app>).
			name:      "\(inputs.project)-\(inputs.role)"
			namespace: ArgoCDNamespace
			labels: {
				"app.kubernetes.io/name":      "\(inputs.project)-\(inputs.role)"
				"app.kubernetes.io/component": inputs.role
				"app.kubernetes.io/part-of":   inputs.project
			}
		}
		spec: {
			project: PROJECTS_PROJECT
			source: {
				// The project's OWN config bundle in the holos org.
				repoURL:        "\(CONFIG_REPO_BASE)/\(inputs.project)-config"
				targetRevision: CONFIG_TAG
				path:           COMPONENTS_BASE
				directory:      inputs.directory
			}
			destination: {
				server: IN_CLUSTER
				// The child Applications/AppProject are argoproj.io resources
				// reconciled in the argocd namespace; namespaced CRs carry their own
				// metadata.namespace, which Argo CD honors per-resource.
				namespace: ArgoCDNamespace
			}
			syncPolicy: {
				automated: {
					prune:    true
					selfHeal: true
				}
				syncOptions: SYNC_OPTIONS
			}
		}
	}
}

// CONTROL_PLANE_ROOT / WORKLOAD_ROOT build the two roots for one project.  The
// control-plane root EXCLUDES the workload glob (reconciles project + app control
// plane); the workload root INCLUDES it (reconciles only the app workload).
#ControlPlaneRoot: {
	project: string
	out:     (#ProjectRoot & {inputs: {
		"project": project
		role:      "control-plane"
		directory: {
			recurse: true
			exclude: WORKLOAD_GLOB
		}
	}}).out
}
#WorkloadRoot: {
	project: string
	out:     (#ProjectRoot & {inputs: {
		"project": project
		role:      "workload"
		directory: {
			recurse: true
			include: WORKLOAD_GLOB
		}
	}}).out
}

userDefinedBuildPlan: {
	metadata: name: "project-app-of-apps"
	// One pair of root Applications per registered project, each rendered into the
	// project's own subdirectory of this component (NOT into any per-project OCI
	// bundle, so a root never reconciles itself).  scripts/apply-project-app-of-apps
	// applies the control-plane root; scripts/apply-project-workload-app-of-apps
	// applies the workload root.  A project with no apps still gets both roots: the
	// control-plane root reconciles the project's control plane and the workload
	// root simply selects nothing (no application/<app>/workload subtree in the
	// bundle) — a harmless no-op until the project gains an app.
	spec: artifacts: manifests: {
		for PROJECT, P in projects {
			let CP_ROOT = (#ControlPlaneRoot & {project: PROJECT}).out
			let WL_ROOT = (#WorkloadRoot & {project: PROJECT}).out

			"\(COMPONENTS_BASE)/project-app-of-apps/\(PROJECT)/application-\(PROJECT)-control-plane.yaml": {
				artifact: _
				generators: [{
					kind:   "Resources"
					output: artifact
					resources: #Resources & {
						Application: (CP_ROOT.metadata.name): CP_ROOT
					}
				}]
			}

			"\(COMPONENTS_BASE)/project-app-of-apps/\(PROJECT)/application-\(PROJECT)-workload.yaml": {
				artifact: _
				generators: [{
					kind:   "Resources"
					output: artifact
					resources: #Resources & {
						Application: (WL_ROOT.metadata.name): WL_ROOT
					}
				}]
			}
		}
	}
}

package holos

// projects is the TENANT-side App-of-Apps (HOL-1377, parent HOL-1373).  It is
// the counterpart to the platform App-of-Apps (components/app-of-apps,
// HOL-1376): where that root reconciles the SYSTEM components under the
// platform AppProject, this root reconciles the TENANT project/application
// resources under the projects AppProject (both AppProjects from HOL-1375,
// components/argocd-projects).  Separating the two roots keeps the system and
// tenant delivery boundaries distinct — the system root lives under platform,
// this top-level Application lives under projects and fans out to the
// per-project control-plane resources and per-app Applications that the
// project/application collection components already emit.
//
// What it bootstraps.  The project component (components/project) renders, for
// every registered project, the per-project AppProject (named <project>), the
// project-level Argo CD Application, the Kargo Project/ProjectConfig/Warehouse/
// Stage, the Quay Organization, and the Keycloak CRs into
// clusters/<cluster>/components/project/<name>/.  The application component
// (components/application) renders, for every registered app, the app's
// control-plane bundle (the app Argo CD Application <project>-<app>, the
// Repository, the KeycloakClient, the Kargo Warehouse/Stage) under
// clusters/<cluster>/components/application/<app>/control-plane/ plus a workload
// bundle under .../workload/.  This App-of-Apps points Argo CD at those two
// component subpaths (directory.recurse: true) so the whole set is bootstrapped
// declaratively rather than only via scripts/apply-projects.
//
// Layout (the self-management-safe App-of-Apps shape, mirroring app-of-apps):
//   - The child Applications render into the children/ SUBDIRECTORY of this
//     component's deploy directory:
//       clusters/<cluster>/components/projects/children/application-<c>.yaml
//   - The root Application renders into this component's directory ABOVE that
//     subdirectory:
//       clusters/<cluster>/components/projects/application-projects-bootstrap.yaml
//     Its source.path points at the children/ subdirectory with
//     directory.recurse: true, so the root reconciles ONLY the child
//     Applications — never itself (it lives one level up, outside the path it
//     scans).
//
// Two children, NOT one-per-instance.  The child set is the two COLLECTION
// components (project, application), each child Application recursing the whole
// component subpath (every registered project's / app's manifests at once),
// rather than one child per project or app.  This keeps the App-of-Apps stable
// as projects/apps are registered: adding projects.<name> or apps.<name> grows
// the rendered manifests under the existing project/application subpaths that
// these two children already recurse — no new child Application is needed and
// this component does not change.
//
// Kargo's targetRevision posture is PRESERVED.  The per-project/app Argo CD
// Applications (project/buildplan.cue, application/buildplan.cue) deliberately
// OMIT spec.source.targetRevision so Kargo's argocd-update owns it; this
// App-of-Apps delivers those Application manifests verbatim from the OCI bundle
// and never sets targetRevision on them, so Kargo remains the sole owner.  This
// component's OWN root + child Applications DO pin targetRevision: dev (the
// CONFIG_TAG below) — there is no Kargo in this bootstrap path, exactly as the
// platform App-of-Apps pins it for the system children.
//
// caBundle stays uncommitted and apply-injected.  The project Organization and
// the app Repository carry spec.caBundle ONLY when the per-cluster local-ca PEM
// is injected at apply time (scripts/apply-projects, --inject ca_bundle_pem);
// the committed render (scripts/render, no injection) omits it (tags.cue
// _CABundlePEM defaults to ""), so the holos-paas-config bundle this root pulls
// carries NO caBundle material — the no-committed-material guarantee holds.  At
// reconcile time the Holos Controller falls back to its pod system trust store
// for those caBundle-less CRs (the documented default when caBundle is empty).
// If a deployment needs the controller to trust the in-cluster Quay's
// mkcert-signed cert, scripts/apply-projects still injects the PEM on a manual
// apply; the two paths coexist — the OCI-sourced bootstrap delivers the
// caBundle-less CRs, and an operator re-running scripts/apply-projects layers
// the injected caBundle on top via server-side apply.  See HOL-1378 for the
// apply-flow wiring that reconciles the two.
//
// SCOPE — this phase RENDERS the projects bootstrap component; WIRING it into
// scripts/apply's apply sequence is the explicit deliverable of HOL-1378 ("wire
// the App-of-Apps bootstrap into apply"), so scripts/apply / scripts/apply-projects
// are DELIBERATELY not modified here.  The component is correct and complete on
// its own; only the apply hookup is deferred to that sibling issue.

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).  The
// canonical value lives in holos/components/argocd/argocd.cue, but that file is
// a CUE ancestor only of the argocd leaf components, so this sibling component
// cannot import it (the same constraint the app-of-apps/argocd-projects/project
// components work around).  Unifying the literal with #RegisteredNamespace ties
// it to the central namespaces registry, turning drift into a render failure.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// CONFIG_REPO_OCI is the platform config OCI repository the App-of-Apps pulls
// from (HOL-1374 publishes holos-paas-config:dev to it).  Keep it consistent
// with argocd-projects' / app-of-apps' CONFIG_REPO_OCI (the projects AppProject's
// sourceRepos pin EXACTLY this URL — HOL-1377 narrowed them from the prior
// oci://quay.holos.internal/*/* wildcard) and scripts/publish-config's
// CONFIG_REPO default.  The project/application component manifests (AppProject +
// Applications + Kargo/Quay/Keycloak CRs) live in THIS bundle, distinct from the
// per-project/app config repos (oci://quay.holos.internal/<project>/<app>-config)
// the per-project/app Applications themselves source — those are Kargo-driven
// workload deliveries.
let CONFIG_REPO_OCI = "oci://quay.holos.internal/holos/holos-paas-config"

// CONFIG_TAG is the mutable bootstrap tag the projects Applications pin
// themselves.  Unlike the per-project/app Applications (components/project,
// components/application) that OMIT targetRevision so Kargo owns it, NO Kargo
// sits in THIS bootstrap path — the root + children pin the tag directly, so
// the field IS committed (the same posture as the platform App-of-Apps).
let CONFIG_TAG = "dev"

// IN_CLUSTER is the in-cluster Kubernetes API server destination every
// Application targets.
let IN_CLUSTER = "https://kubernetes.default.svc"

// PROJECTS_PROJECT is the Argo CD AppProject the argocd-projects component
// (HOL-1375) emits for tenant delivery.  HOL-1377 widens it so this App-of-Apps
// (assigned to it) can deliver the project/application manifests: the per-project
// AppProject and per-project/app Applications land in the argocd namespace, and
// the project component emits a CLUSTER-SCOPED Kargo Project — both of which the
// Phase-2 projects AppProject denied.  See argocd-projects/buildplan.cue for the
// widened destination (argocd re-permitted) and clusterResourceWhitelist (the
// Kargo Project kind) and the security rationale (only this platform-trusted
// App-of-Apps and its children are assigned project: projects; tenant artifacts
// use their own per-project AppProjects).
let PROJECTS_PROJECT = "projects"

// COMPONENTS_BASE is the deploy-tree subpath the rendered manifests live under,
// matching the OCI bundle layout (oci-publish-workflow.md): the tar is rooted at
// holos/deploy/, so member paths begin at clusters/.  Each child Application's
// source.path is "\(COMPONENTS_BASE)/<component>".
let COMPONENTS_BASE = "clusters/\(clusterName)/components"

// APPLICATION_WORKLOAD_EXCLUDE is the directory.exclude glob the application
// child Application carries so the App-of-Apps reconciles ONLY the per-app
// CONTROL-PLANE manifests, never the per-app WORKLOAD manifests.  The application
// component (components/application) renders each app into TWO subtrees:
//   clusters/<cluster>/components/application/<app>/control-plane/  (the Repository,
//     KeycloakClient, Kargo Warehouse/Stage, and the app's OWN Argo CD Application)
//   clusters/<cluster>/components/application/<app>/workload/       (the Deployment/
//     Service/HTTPRoute/ConfigMap/SA/RoleBinding)
// The workload subtree is the <app>-config artifact the app's OWN Argo CD
// Application syncs from oci://quay.holos.internal/<project>/<app>-config (the
// Kargo-driven delivery path).  If this App-of-Apps' application child recursed
// the whole application/ tree it would ALSO reconcile those workload manifests
// straight from holos-paas-config — a SECOND Argo CD manager for the same
// Deployment/Service/HTTPRoute, fighting the per-app Application and bypassing
// Kargo's targetRevision ownership (the exact second-manager hazard the
// application component splits the bundles to avoid).  This glob — matched by
// Argo CD's directory.exclude against paths under the source.path
// (clusters/<cluster>/components/application) with doublestar semantics — excludes
// every <app>/workload/<file>, so the application child delivers only the
// control-plane manifests while the workload stays exclusively Kargo/app-Application
// owned.
let APPLICATION_WORKLOAD_EXCLUDE = "**/workload/**"

// TENANT_COMPONENTS is the set of COLLECTION components this projects App-of-Apps
// reconciles: the project and application components (holos/platform/platform.cue
// registers them AFTER app-of-apps, capping the system set).  Each entry becomes
// one child Application that recurses the whole component subpath in the OCI
// bundle — every registered project's / app's manifests at once — so registering
// a new project/app grows the manifests under these existing subpaths without
// adding a child Application.  The application child carries an exclude glob
// (APPLICATION_WORKLOAD_EXCLUDE) so it reconciles only the per-app control-plane
// manifests, never the Kargo-delivered workload subtree; the project component
// emits no workload split, so the project child needs no exclude.
//
// Both children share sync-wave 0 — DELIBERATELY NOT serialized project-then-
// application — because the argocd controller (components/argocd/controller,
// HOL-1376) installs an Application HEALTH customization that makes a child
// Application report Healthy only once its OWN status is Healthy, and Argo CD
// gates sync-wave N+1 on every wave-N resource becoming Healthy.  The project
// child delivers the per-project Argo CD Application, whose targetRevision is
// DELIBERATELY OMITTED so Kargo owns it (project/buildplan.cue) — that
// Application stays Missing/Unknown (never Healthy) until Kargo's first
// promotion patches the revision.  Putting the project child in an EARLIER wave
// than the application child would therefore HEALTH-GATE the application child on
// the project child becoming Healthy, which it cannot until Kargo runs — a
// bootstrap DEADLOCK where the app control-plane resources (Repository,
// KeycloakClient, Warehouse/Stage) never reconcile.  A shared wave applies both
// children at once, so neither gates the other.
//
// This is SAFE because the cross-component dependency is UNIDIRECTIONAL and
// resolved REACTIVELY, not by apply order: the application component's CRs
// reference the project component's (an app KeycloakClient is referenced by the
// project role groups' clientRef, a Repository's organizationRef names the
// project Organization), but the Holos Controller reconciles each CR with retry,
// and each child's syncPolicy.automated selfHeal converges regardless of arrival
// order — exactly the "selfHeal converges regardless of arrival order" property
// the platform App-of-Apps documents for its own steady state.  scripts/apply-
// projects likewise applies project-before-apps only as a Ready-gating
// convenience, not a hard ordering the controllers require.
let TENANT_COMPONENTS = [
	{component: "project"},
	{component: "application", exclude: APPLICATION_WORKLOAD_EXCLUDE},
]

// SYNC_OPTIONS are applied consistently to the root and every child Application
// so Argo CD's reconciliation matches how scripts/apply-projects applies the same
// manifests (kubectl apply --server-side --force-conflicts):
//   - ServerSideApply=true mirrors --server-side, so Argo CD and a manual
//     scripts/apply-projects share server-side-apply field ownership rather than
//     fighting over client-side last-applied annotations — this is also what lets
//     an operator's apply-time caBundle injection coexist with the caBundle-less
//     CRs this root delivers (each owns disjoint fields).
//   - CreateNamespace=false: every project control namespace is owned by the
//     namespaces component (delivered by the platform App-of-Apps), so no tenant
//     Application may create one — the convention the per-project/app Applications
//     already follow.
let SYNC_OPTIONS = [
	"ServerSideApply=true",
	"CreateNamespace=false",
]

// CHILD_DIR is the deploy-tree subdirectory the child Applications render into
// and the path (inside the OCI bundle) the root Application scans.  It is a
// SUBDIRECTORY of this component's directory so the root Application (which
// renders one level up) is never in the path it reconciles.
let CHILD_DIR = "\(COMPONENTS_BASE)/projects/children"

// #ChildApplication builds one child Argo CD Application for a tenant collection
// component.  inputs.component / inputs.wave are builder parameters; out is the
// resulting Application — kept in a separate field so the builder's parameters
// never leak into the rendered resource (the #Resources.Application binding is
// closed and rejects unknown fields).  Each Application pins the :dev tag itself
// (no Kargo in this bootstrap path), recurses the component's subpath in the
// bundle, lands in the projects AppProject, and targets the argocd namespace
// (the AppProject + per-project/app Applications it delivers are argoproj.io
// resources reconciled there; the namespaced CRs it also delivers carry their own
// metadata.namespace, which Argo CD honors per-resource regardless of the
// Application's spec.destination.namespace default).
#ChildApplication: {
	inputs: {
		component: string
		wave:      int
		// exclude is an optional directory.exclude glob (empty = none); the
		// application child sets it to skip the Kargo-delivered workload subtree.
		exclude: string | *""
	}
	out: {
		apiVersion: "argoproj.io/v1alpha1"
		kind:       "Application"
		metadata: {
			// projects- prefix marks these as projects-bootstrap-owned and avoids
			// colliding with same-named Applications in the argocd namespace (e.g.
			// the per-project Application named <project>, the per-app Application
			// named <project>-<app>).
			name:      "projects-\(inputs.component)"
			namespace: ArgoCDNamespace
			labels: {
				"app.kubernetes.io/name":      "projects-\(inputs.component)"
				"app.kubernetes.io/component": inputs.component
				"app.kubernetes.io/part-of":   "projects"
			}
			annotations: "argocd.argoproj.io/sync-wave": "\(inputs.wave)"
		}
		spec: {
			project: PROJECTS_PROJECT
			source: {
				repoURL:        CONFIG_REPO_OCI
				targetRevision: CONFIG_TAG
				// The component subpath holds the per-project / per-app manifests
				// (one nested directory per registered instance); recurse scans the
				// whole tree so every instance is reconciled by this one child.
				path: "\(COMPONENTS_BASE)/\(inputs.component)"
				directory: {
					recurse: true
					// exclude the Kargo-delivered workload subtree when set (the
					// application child), so this root never becomes a second manager
					// for the per-app Deployment/Service/HTTPRoute.
					if inputs.exclude != "" {
						exclude: inputs.exclude
					}
				}
			}
			destination: {
				server:    IN_CLUSTER
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

// CHILD_WAVE is the SHARED sync-wave every child carries (0).  All children sit
// in one wave on purpose — see the TENANT_COMPONENTS note: serializing the
// project child before the application child would health-gate the application
// child on the project child becoming Healthy, which it cannot until Kargo's
// first promotion (the per-project Application's omitted targetRevision), so a
// shared wave avoids that bootstrap deadlock.  The root (-1) still settles before
// the children fan out.
let CHILD_WAVE = 0

// CHILD_APPLICATIONS is one child Application per tenant collection component,
// every child carrying the shared CHILD_WAVE so neither health-gates the other.
let CHILD_APPLICATIONS = [
	for c in TENANT_COMPONENTS {
		(#ChildApplication & {inputs: {component: c.component, wave: CHILD_WAVE, if c.exclude != _|_ {exclude: c.exclude}}}).out
	},
]

// ROOT_APPLICATION is the tenant App-of-Apps: it reconciles the children/
// subdirectory of the bundle (directory.recurse: true), so it manages exactly the
// child Applications above and nothing else.  It is assigned to the projects
// AppProject and pins the :dev tag like its children.  Its sync-wave is one less
// than the first child's (-1) so, on the bootstrap apply, the root settles before
// Argo CD begins fanning the children out.
let ROOT_APPLICATION = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "Application"
	metadata: {
		name:      "projects-bootstrap"
		namespace: ArgoCDNamespace
		labels: {
			"app.kubernetes.io/name":    "projects-bootstrap"
			"app.kubernetes.io/part-of": "projects"
		}
		annotations: "argocd.argoproj.io/sync-wave": "-1"
	}
	spec: {
		project: PROJECTS_PROJECT
		source: {
			repoURL:        CONFIG_REPO_OCI
			targetRevision: CONFIG_TAG
			// The children/ subdirectory holds one child Application manifest per
			// tenant collection component; recurse scans it for all of them.
			path: CHILD_DIR
			directory: recurse: true
		}
		destination: {
			server: IN_CLUSTER
			// The child Applications are argoproj.io resources reconciled in the
			// argocd namespace.
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

// Assert the child set is non-empty and every child carries the shared
// CHILD_WAVE, so a future edit that reintroduces per-child wave serialization
// (re-opening the health-gate deadlock the TENANT_COMPONENTS note describes)
// fails render rather than shipping silently.  Each comprehension element unifies
// the rendered wave string with the expected shared-wave string; a mismatch fails
// render.
_assertNonEmpty: true & (len(CHILD_APPLICATIONS) > 0)
_assertWaves: [
	for app in CHILD_APPLICATIONS {
		"\(CHILD_WAVE)" & app.metadata.annotations."argocd.argoproj.io/sync-wave"
	},
]

userDefinedBuildPlan: {
	metadata: name: "projects"
	spec: artifacts: manifests: {
		// The root Application renders to a single file in this component's
		// directory — applied once by scripts/apply during bootstrap (HOL-1378),
		// then it owns the children/ subdirectory below.  It sits OUTSIDE children/
		// so it never reconciles itself.
		"\(COMPONENTS_BASE)/\(metadata.name)/application-projects-bootstrap.yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				// Unify with #Resources so the Application validates against the
				// vendored argoproj.io/application binding at render time.
				resources: #Resources & {
					Application: (ROOT_APPLICATION.metadata.name): ROOT_APPLICATION
				}
			}]
		}

		// The child Applications render into the children/ subdirectory the root
		// scans, one file per Application (kubectl-slice) so they diff cleanly and
		// Argo CD's recurse picks each up.
		(CHILD_DIR): {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				resources: #Resources & {
					Application: {
						for app in CHILD_APPLICATIONS {
							(app.metadata.name): app
						}
					}
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

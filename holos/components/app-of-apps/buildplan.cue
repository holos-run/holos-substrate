package holos

// app-of-apps bootstraps the whole platform through Argo CD (HOL-1376, parent
// HOL-1373).  It is the change that makes Argo CD actually reconcile the
// platform: a single root Application (the App-of-Apps) plus one child
// Application per SYSTEM component, every child reading the holos-paas-config
// OCI bundle at the mutable :dev tag the producer phase publishes (HOL-1374)
// and assigned to the platform AppProject the prior phase stood up (HOL-1375).
//
// Layout (the self-management-safe App-of-Apps shape):
//   - The child Applications render into the children/ SUBDIRECTORY of this
//     component's deploy directory:
//       clusters/<cluster>/components/app-of-apps/children/application-<c>.yaml
//   - The root Application renders into this component's directory ABOVE that
//     subdirectory:
//       clusters/<cluster>/components/app-of-apps/application-bootstrap.yaml
//     Its source.path points at the children/ subdirectory with
//     directory.recurse: true, so the root reconciles ONLY the child
//     Applications — never itself (it lives one level up, outside the path it
//     scans).  This avoids the self-reference loop a root Application that
//     scanned its own directory would create.
//
// Bootstrap vs. steady state.  scripts/apply applies the whole app-of-apps/
// directory once (root + children) during cluster bring-up, so Argo CD has the
// root and the children to start from.  Thereafter the root Application owns the
// children/ subdirectory: it re-pulls the :dev bundle, regenerates the child set
// from children/, and Argo CD's automated sync reconciles each child against the
// cluster.  The children in turn each pull their own component's manifests from
// the same bundle at their per-component source.path.
//
// SCOPE — this phase (HOL-1376) RENDERS the bootstrap component; WIRING it into
// scripts/apply's apply sequence (the bring-up step that applies the root +
// children once so Argo CD takes over) is the explicit deliverable of HOL-1378
// ("wire the App-of-Apps bootstrap into apply").  scripts/apply is DELIBERATELY
// not modified here — HOL-1376's testing is manifest inspection + render
// diff-cleanliness, no live-cluster apply — so until HOL-1378 lands, a fresh
// scripts/apply does not yet create platform-bootstrap.  The component is correct
// and complete on its own; only the apply hookup is deferred to that sibling
// issue to keep the two changes reviewable in isolation.
//
// "Always" re-pull of the mutable :dev tag.  Argo CD caches the resolved
// manifest of an OCI tag in the repo-server's repo cache; a moved :dev tag is
// not re-pulled until that cache entry expires.  Argo CD 3.4.2 exposes NO
// OCI-tag-specific expiration knob (verified against the vendored chart 9.5.15:
// the only OCI cmd-params keys are reposerver.oci.manifest.max.extracted.size
// and reposerver.oci.layer.media.types — size/format limits, not a TTL).  The
// applicable mechanism is the repo cache expiration —
// reposerver.repo.cache.expiration (ARGOCD_REPO_CACHE_EXPIRATION, default 24h),
// which governs the manifest/revision-metadata cache OCI tag resolution uses.
// The argocd controller component (components/argocd/controller) shortens it so
// a re-pushed :dev is re-resolved within the interval; combined with each
// child's syncPolicy.automated (prune + selfHeal) that re-resolution is then
// reconciled to the cluster — the "Always" image-tag update policy this phase
// requires.  See that component for the chosen value and the documented
// tradeoff.
//
// Scope of the sync-wave ordering guarantee (deliberate, AC-bounded).  The
// ascending child sync-waves — backed by the Application health customization in
// the argocd controller (so the root waits for an earlier wave to become Healthy
// before applying the next) — serialize the BOOTSTRAP rollout: the order in which
// the root App-of-Apps first CREATES the child Applications and their resources,
// which is what AC #4 requires ("ArgoCD will try to apply controllers before
// their CRDs" without them).  They do NOT serialize STEADY-STATE :dev updates:
// each child independently tracks targetRevision: dev with its own automated
// sync (the "Always" policy AC #1/#3 MANDATE), so when the tag moves the children
// re-resolve and sync their own component paths in parallel rather than in wave
// order.  This is an intrinsic, accepted tradeoff of the per-child :dev-tracking
// design the ACs chose: a globally wave-serialized UPDATE rollout would require
// the root to be the only object that changes per release (e.g. digest-pinned
// child revisions regenerated into children/ each publish), which directly
// contradicts AC #1/#3's committed targetRevision: dev on the children.  In
// practice steady-state CRD/schema changes are rare and additive (a moved :dev
// is almost always a workload/config change, not a CRD-vs-controller reordering),
// and each child's selfHeal converges regardless of arrival order; if a future
// release needs serialized cross-component UPDATE ordering, that is a separate
// design (Kargo-style staged promotion or digest-pinned children), tracked
// outside this phase.

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).  The
// canonical value lives in holos/components/argocd/argocd.cue, but that file is
// a CUE ancestor only of the argocd leaf components, so this sibling component
// cannot import it (the same constraint the argocd-projects/project/kargo-echo
// components work around).  Unifying the literal with #RegisteredNamespace ties
// it to the central namespaces registry, turning drift between the two literals
// into a render failure.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// CONFIG_REPO_OCI is the platform config OCI repository the App-of-Apps pulls
// from (HOL-1374 publishes holos-paas-config:dev to it).  Keep it consistent
// with argocd-projects' CONFIG_REPO_OCI (the platform AppProject's sourceRepos
// authorize exactly this URL) and scripts/publish-config's CONFIG_REPO default.
let CONFIG_REPO_OCI = "oci://quay.holos.internal/holos/holos-paas-config"

// CONFIG_TAG is the mutable bootstrap tag the platform Applications pin
// themselves.  Unlike the Kargo-driven Applications (components/project,
// components/kargo-echo) that OMIT targetRevision so Kargo owns it, NO Kargo
// sits in this path — the platform Applications pin the tag directly, so the
// field IS committed.
let CONFIG_TAG = "dev"

// IN_CLUSTER is the in-cluster Kubernetes API server destination every platform
// Application targets.
let IN_CLUSTER = "https://kubernetes.default.svc"

// PLATFORM_PROJECT is the Argo CD AppProject the argocd-projects component
// (HOL-1375) emits; it authorizes CONFIG_REPO_OCI as a source, all namespaces as
// destinations, and BOTH namespace- and cluster-scoped resources (the system
// owns CRDs/Namespaces/ClusterRoles).  Every Application here is assigned to it.
let PLATFORM_PROJECT = "platform"

// COMPONENTS_BASE is the deploy-tree subpath the rendered manifests live under,
// matching the OCI bundle layout Phase 1 recorded (oci-publish-workflow.md): the
// tar is rooted at holos/deploy/, so member paths begin at clusters/.  Each
// child Application's source.path is "\(COMPONENTS_BASE)/<component>".
let COMPONENTS_BASE = "clusters/\(clusterName)/components"

// SYSTEM_COMPONENTS is the ordered set of "system" components the platform
// App-of-Apps reconciles: every component registered in
// holos/platform/platform.cue BEFORE the project/application collection
// components, which is EXACTLY the ordered COMPONENTS=(...) array in
// scripts/apply (the single authoritative apply order).  The order is
// load-bearing: the list index becomes each child's argocd.argoproj.io/sync-wave
// (ascending), so Argo CD applies CRDs/namespaces before their controllers,
// Istio base before istiod/cni/ztunnel, cert-manager before local-ca, and
// operators before their instances — mirroring scripts/apply's dependency order.
//
// Why enumerate instead of derive: each component is rendered in ISOLATION
// (holos render platform evaluates each components/<name> directory on its own
// with only its injected _Tags in scope), so the platform.components registry is
// NOT reachable from inside this component buildplan.  The issue sanctions an
// explicit list sourced from platform.cue when derivation is impractical; this
// is that list.  KEEP IT IN LOCK-STEP with scripts/apply's COMPONENTS array and
// the platform.cue registration order when adding, removing, or renaming a
// system component.
//
// DELIBERATELY EXCLUDED (NOT system bootstrap, per platform.cue + scripts/apply):
//   - keycloak-instance, project, application — each carries a per-cluster
//     local-ca caBundle injected at APPLY time (scripts/apply-projects), never
//     committed; Argo CD reconciling their committed, caBundle-less manifests
//     would strip that material.  They are out of the master scripts/apply array
//     for the same reason and so are out of this system App-of-Apps.
//
// The echo/kargo-echo overlap is DELIBERATE, not a bug.  scripts/apply applies
// BOTH echo (the smoke-test Deployment/Service/HTTPRoute) and kargo-echo (the
// Kargo Warehouse/Stage plus a Kargo-driven echo Argo CD Application), so this
// App-of-Apps mirrors that — both are in the system set (AC #2/#4 require the set
// to BE the scripts/apply order; dropping echo would violate them).  The two
// Applications source from DIFFERENT repos and do not fight over the workload
// image: platform-echo (here) reconciles the STATIC echo scaffold committed in
// the holos-paas-config bundle, while the kargo-echo component's own echo
// Application sources the holos-paas-manifests repo whose targetRevision Kargo
// owns and re-points on promotion.  The committed echo Deployment pins the
// default agnhost smoke-test image (holos/tags.cue _AppImage), so a selfHeal
// from platform-echo only ever re-asserts that same static scaffold — it does
// not undo a Kargo promotion, which lives on the separate manifests-repo
// Application.  If a future cluster wants Kargo to be the sole owner of the echo
// workload, drop "echo" from this list and let only kargo-echo's Application
// reconcile it (record the exclusion here).
let SYSTEM_COMPONENTS = [
	"namespaces",
	"coredns",
	"gateway-api",
	"cert-manager-crds",
	"istio-base",
	"istiod",
	"istio-cni",
	"istio-ztunnel",
	"cert-manager",
	"local-ca",
	"istio-gateway",
	"echo",
	"holos-authenticator",
	"cnpg-crds",
	"cnpg",
	"cnpg-clusters",
	"keycloak-operator-crds",
	"keycloak-operator",
	"keycloak",
	"keycloak-esso-config",
	"keycloak-config",
	"quay",
	"argocd-crds",
	"argocd",
	"argocd-projects",
	"kargo-crds",
	"kargo",
	"kargo-project-echo",
	"kargo-echo",
]

// SYNC_OPTIONS are applied consistently to the root and every child Application
// so Argo CD's reconciliation matches how scripts/apply applies the same
// manifests (kubectl apply --server-side --force-conflicts):
//   - ServerSideApply=true mirrors --server-side, so Argo CD and a manual
//     scripts/apply share the same server-side-apply semantics and field
//     ownership rather than fighting over client-side last-applied annotations.
//   - CreateNamespace=false: every platform namespace is owned by the
//     namespaces component (the namespaces child Application, sync-wave 0), so
//     no other Application may create one — the convention the project/kargo-echo
//     Applications already follow.
// Argo CD's automated sync resolves field-manager conflicts via server-side
// apply; the scripts/apply --force-conflicts posture is the manual counterpart
// for the one-shot bootstrap apply.
let SYNC_OPTIONS = [
	"ServerSideApply=true",
	"CreateNamespace=false",
]

// CHILD_DIR is the deploy-tree subdirectory the child Applications render into
// and the path (inside the OCI bundle) the root Application scans.  It is a
// SUBDIRECTORY of this component's directory so the root Application (which
// renders one level up) is never in the path it reconciles.
let CHILD_DIR = "\(COMPONENTS_BASE)/app-of-apps/children"

// #ChildApplication builds one child Argo CD Application for a system component.
// inputs.component / inputs.wave are builder parameters; out is the resulting
// Application — kept in a separate field so the builder's parameters never leak
// into the rendered resource (the #Resources.Application binding is closed and
// rejects unknown fields).  Each Application pins the :dev tag itself (no Kargo
// in this path), targets the component's subpath in the bundle, lands in the
// platform AppProject, and carries an ascending sync-wave so Argo CD applies
// dependencies first.
#ChildApplication: {
	inputs: {
		component: string
		wave:      int
	}
	out: {
		apiVersion: "argoproj.io/v1alpha1"
		kind:       "Application"
		metadata: {
			// platform- prefix marks these as platform-bootstrap-owned and avoids
			// colliding with same-named Applications already in the argocd
			// namespace (e.g. the echo Application kargo-echo emits, the
			// my-project Application the project component emits).
			name:      "platform-\(inputs.component)"
			namespace: ArgoCDNamespace
			labels: {
				"app.kubernetes.io/name":      "platform-\(inputs.component)"
				"app.kubernetes.io/component": inputs.component
				"app.kubernetes.io/part-of":   "platform"
			}
			annotations: "argocd.argoproj.io/sync-wave": "\(inputs.wave)"
		}
		spec: {
			project: PLATFORM_PROJECT
			source: {
				repoURL:        CONFIG_REPO_OCI
				targetRevision: CONFIG_TAG
				path:           "\(COMPONENTS_BASE)/\(inputs.component)"
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

// CHILD_APPLICATIONS is one child Application per system component, the list
// index supplying the ascending sync-wave.
let CHILD_APPLICATIONS = [
	for i, c in SYSTEM_COMPONENTS {
		(#ChildApplication & {inputs: {component: c, wave: i}}).out
	},
]

// ROOT_APPLICATION is the App-of-Apps: it reconciles the children/ subdirectory
// of the bundle (directory.recurse: true), so it manages exactly the child
// Applications above and nothing else.  It is assigned to the platform
// AppProject and pins the :dev tag like its children.  Its sync-wave is one less
// than the first child's (-1) so, on the bootstrap apply, the root settles
// before Argo CD begins fanning the children out.
let ROOT_APPLICATION = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "Application"
	metadata: {
		name:      "platform-bootstrap"
		namespace: ArgoCDNamespace
		labels: {
			"app.kubernetes.io/name":    "platform-bootstrap"
			"app.kubernetes.io/part-of": "platform"
		}
		annotations: "argocd.argoproj.io/sync-wave": "-1"
	}
	spec: {
		project: PLATFORM_PROJECT
		source: {
			repoURL:        CONFIG_REPO_OCI
			targetRevision: CONFIG_TAG
			// The children/ subdirectory holds one child Application manifest
			// per system component; recurse scans it for all of them.
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

// Assert the child set is non-empty and each child's sync-wave is exactly its
// list index, so a future edit that reorders or drops an entry can't silently
// produce a gap or a duplicate wave.  Each comprehension element unifies the
// rendered wave string with the expected index string; a mismatch fails render.
_assertNonEmpty: true & (len(CHILD_APPLICATIONS) > 0)
_assertWaves: [
	for i, app in CHILD_APPLICATIONS {
		"\(i)" & app.metadata.annotations."argocd.argoproj.io/sync-wave"
	},
]

userDefinedBuildPlan: {
	metadata: name: "app-of-apps"
	spec: artifacts: manifests: {
		// The root Application renders to a single file in this component's
		// directory — applied once by scripts/apply during bootstrap, then it
		// owns the children/ subdirectory below.  It sits OUTSIDE children/ so it
		// never reconciles itself.
		"\(COMPONENTS_BASE)/\(metadata.name)/application-bootstrap.yaml": {
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

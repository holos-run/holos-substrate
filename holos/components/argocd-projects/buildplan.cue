package holos

import "list"

// argocd-projects establishes the Argo CD project separation between the
// platform "system" and the tenant "projects", and registers the platform
// config OCI repository with Argo CD (HOL-1375, parent HOL-1373 — the
// App-of-Apps bootstrap).  It is the consumer-side counterpart to the
// holos-paas-config OCI bundle the producer phase publishes (HOL-1374): later
// phases (HOL-1376/HOL-1377) add the Applications that reference these two
// AppProjects and pull from this repository.  This phase introduces NO
// Applications — only the two AppProjects and the repository credential — so it
// is a safe, additive change.
//
// Two AppProjects:
//   - platform owns every system component.  Its sourceRepos authorize the
//     holos-paas-config bundle (and the per-app holos-paas-manifests repo the
//     echo/Kargo pipeline already uses); its destinations permit ALL namespaces
//     on the in-cluster API server; and — unlike a tenant project — it allows
//     BOTH namespace- and cluster-scoped resources (group:"*"/kind:"*"), because
//     the system components own CRDs, ClusterRoles, and Namespaces.
//   - projects owns the single top-level App-of-Apps that bootstraps tenant
//     projects (HOL-1377).  It is scoped to EXACTLY the holos-paas-config bundle
//     (sourceRepos), permits the argocd namespace plus all tenant namespaces
//     (every other platform namespace denied), and whitelists EXACTLY the one
//     cluster-scoped kind the project component emits (the Kargo Project) — NOT
//     the platform project's group:"*"/kind:"*" wildcard.  HOL-1377 widened it
//     from its Phase-2 minimal scoping (which denied argocd and omitted
//     clusterResourceWhitelist) so the App-of-Apps can deliver the per-project
//     AppProject/Applications into argocd and the cluster-scoped Kargo Project;
//     see the PROJECTS_PROJECT header below for the full security rationale.
//
// Registered after the argocd controller and before kargo in
// holos/platform/platform.cue: the AppProject kind is an argoproj.io custom
// resource, so the Argo CD CRDs (argocd-crds) and controller (argocd) must be
// established first.

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).  The
// canonical value lives in holos/components/argocd/argocd.cue, but that file is
// a CUE ancestor only of the argocd leaf components, so this sibling component
// cannot import it (the same constraint the project/kargo-echo components work
// around).  Unifying the literal with #RegisteredNamespace ties it to the
// central namespaces registry, turning drift between the two literals into a
// render failure.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// CONFIG_REPO is the platform config OCI repository the App-of-Apps bootstrap
// pulls from (HOL-1374 publishes holos-paas-config:dev to it).  The BARE form is
// the registry/repo path; the oci:// form is what an Argo CD Application source
// repoURL and the repository credential Secret's url take.  Keep both consistent
// with scripts/publish-config's CONFIG_REPO default and oci-publish-workflow.md.
let CONFIG_REPO = "quay.holos.internal/holos/holos-paas-config"
let CONFIG_REPO_OCI = "oci://\(CONFIG_REPO)"

// CONFIG_REPO_BASE is the OCI repository PREFIX the platform config bundles live
// under: the platform-owned `holos` Quay org.  The PLATFORM config bundle is
// <CONFIG_REPO_BASE>/holos-paas-config (CONFIG_REPO above); each PROJECT's
// per-project config bundle (HOL-1382) is <CONFIG_REPO_BASE>/<project>-config.
// Keep consistent with project-app-of-apps's CONFIG_REPO_BASE and
// scripts/apply-project-app-of-apps's CONFIG_REPO_BASE.
let CONFIG_REPO_BASE = "quay.holos.internal/holos"

// PROJECT_BUNDLE_OCI maps each registered project name to the oci:// URL of its
// per-project config bundle (HOL-1382).  The projects AppProject authorizes
// EXACTLY these repos (sourceRepos below) and a credential-less registration
// Secret is emitted per bundle (PROJECT_BUNDLE_REGISTRATIONS) so Argo CD pulls the
// public bundle with the in-cluster Quay's insecure: "true" setting.  An EXACT
// per-project list (not an oci://.../holos/* wildcard) keeps the projects
// AppProject from authorizing sibling holos-org repos, the same exact-match
// discipline the platform AppProject's sourceRepos use.
let PROJECT_BUNDLE_OCI = {for NAME, _ in projects {(NAME): "oci://\(CONFIG_REPO_BASE)/\(NAME)-config"}}

// MANIFESTS_REPO is the per-app rendered-manifests OCI repository the existing
// echo/Kargo delivery pipeline publishes to (components/kargo-echo).  The
// platform AppProject authorizes it too so a system-owned Application may also
// source from it; the oci:// form matches the Application source repoURL string.
let MANIFESTS_REPO_OCI = "oci://quay.holos.internal/holos/holos-paas-manifests"

// IN_CLUSTER is the in-cluster Kubernetes API server destination every Argo CD
// Application in these projects targets.
let IN_CLUSTER = "https://kubernetes.default.svc"

// DENIED_NAMESPACES are the namespaces the projects AppProject denies as
// destinations: every platform-infrastructure namespace (the central
// #ReservedNamespaceNames registry — argocd, keycloak, quay, kargo, istio-*,
// cert-manager, holos-controller, …) plus the Kubernetes system namespaces.
// Sourcing the platform set from the registry keeps the deny list in lock-step
// as platform namespaces are added (the registry is the single source of truth
// the project component already uses to reject colliding project names).
let DENIED_NAMESPACES = list.Concat([#ReservedNamespaceNames, [
	"kube-system",
	"kube-public",
	"kube-node-lease",
	"default",
]])

// --- AppProject: platform -------------------------------------------------
//
// Owns all system components.  sourceRepos authorize the config bundle and the
// per-app manifests repo; destinations permit every namespace on the in-cluster
// API server; both namespace- and cluster-scoped resources are whitelisted
// (the system owns CRDs/ClusterRoles/Namespaces).
let PLATFORM_PROJECT = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "AppProject"
	metadata: {
		name:      "platform"
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": "platform"
	}
	spec: {
		// Argo CD matches an Application's source by EXACT repoURL string and
		// selects the tag/digest via targetRevision — it does NOT suffix-match the
		// repoURL — so the config repo is listed by its exact oci:// URL (a '*'
		// suffix would also authorize sibling repos like holos-paas-config-backdoor
		// for a project that whitelists cluster-scoped resources).  The manifests
		// repo is listed in full for the system-owned Application that may source
		// from it.
		sourceRepos: [
			CONFIG_REPO_OCI,
			MANIFESTS_REPO_OCI,
		]
		destinations: [{
			server:    IN_CLUSTER
			namespace: "*"
		}]
		namespaceResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
		// Unlike a tenant project, the platform owns cluster-scoped resources
		// (CRDs, ClusterRoles, Namespaces), so cluster-scoped types are whitelisted.
		clusterResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
	}
}

// --- AppProject: projects -------------------------------------------------
//
// Owns the single top-level App-of-Apps that bootstraps tenant projects (the
// projects component, HOL-1377) and its two child Applications (projects-project,
// projects-application).  Scoped (sourceRepos) to EXACTLY the holos-paas-config
// bundle the projects App-of-Apps pulls the project/application manifests from —
// HOL-1377 narrowed it from the prior oci://quay.holos.internal/*/* wildcard (see
// the sourceRepos rationale below for why the wildcard was unsafe once the argocd
// destination is permitted).
//
// HOL-1377 widens this project so the App-of-Apps can DELIVER the project/
// application manifests, which Phase 2's minimal scoping rejected:
//   - argocd is RE-PERMITTED as a destination (the '!argocd' deny is removed):
//     the project component emits the per-project AppProject (named <project>)
//     and the per-project/app Argo CD Applications into the argocd namespace, so
//     the App-of-Apps must be allowed to create them there.  This is safe because
//     ONLY this platform-trusted App-of-Apps and its children are assigned
//     project: projects — every TENANT artifact (the per-project/app Applications
//     this delivers) is assigned to its OWN per-project AppProject (project:
//     <project>, namespaced-only, no argocd destination), so re-permitting argocd
//     here does not let a tenant artifact mint an escalating Application: a tenant
//     would have to be assigned project: projects to use this destination, and
//     nothing tenant-authored is.  The remaining platform-infrastructure
//     namespaces stay DENIED (keycloak/quay/kargo/istio-*/… via DENIED_NAMESPACES
//     minus argocd), preserving the confused-deputy boundary for everything except
//     the Argo CD control namespace the App-of-Apps legitimately writes to.
//   - clusterResourceWhitelist is ADDED, scoped to EXACTLY the cluster-scoped
//     kinds the project component emits: the Kargo Project (kargo.akuity.io/Project
//     is cluster-scoped in Kargo 1.10 — it adopts the same-named project control
//     namespace).  It is NOT a group:"*"/kind:"*" wildcard (that is the PLATFORM
//     project's privilege); the tenant App-of-Apps may create only this one
//     cluster-scoped kind, keeping tenants confined to namespaced resources plus
//     the single Kargo Project their delivery pipeline requires.
let PROJECTS_PROJECT = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "AppProject"
	metadata: {
		name:      "projects"
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": "projects"
	}
	spec: {
		// EXACTLY the per-project config bundles (HOL-1382) — one oci:// URL per
		// registered project (oci://quay.holos.internal/holos/<project>-config), the
		// only repos any Application assigned to this project ever sources.  Each
		// per-project App-of-Apps root (components/project-app-of-apps,
		// <project>-control-plane / <project>-workload) pulls ONLY its own project's
		// bundle; the per-project/app Applications those bundles carry are assigned to
		// their OWN per-project AppProjects (project: <project>) and source their own
		// <project>/<app>-config repos, NOT these.  An EXACT per-project list (not an
		// oci://quay.holos.internal/holos/* wildcard, and never the old
		// oci://quay.holos.internal/*/* tenant wildcard) is the same exact-match
		// discipline the platform AppProject uses: a wildcard would let an Application
		// assigned to projects — which may write into the argocd namespace — source a
		// sibling holos-org (or tenant) repo and reconcile arbitrary namespaced
		// resources (Applications/AppProjects/Secrets) into argocd, a confused-deputy
		// escalation.  Pinning to the exact platform-owned per-project bundles closes
		// that — combined with the project-assignment boundary, only the
		// platform-trusted per-project roots use this project's argocd-write privilege.
		sourceRepos: [for NAME, _ in projects {PROJECT_BUNDLE_OCI[NAME]}]
		// Tenant namespaces on the in-cluster API server.  '*' admits the project
		// control and env-prefixed namespaces (ci-/qa-/prod-<name>, bare <name>)
		// the projects App-of-Apps fans out into, PLUS the argocd namespace the
		// per-project AppProject and per-project/app Applications land in (HOL-1377
		// re-permits it — see the project header note for why this does not widen
		// tenant access).  EVERY OTHER platform-infrastructure namespace is DENIED
		// ('!'-prefixed deny entries): without this a tenant artifact sourced from
		// oci://quay.holos.internal/<tenant>/* could create resources (e.g. a
		// Secret/RoleBinding in keycloak/quay/kargo) inside a platform namespace — a
		// confused-deputy privilege escalation across the tenant/platform boundary.
		// The deny set is the central #ReservedNamespaceNames registry (the static
		// platform namespaces a project may not name) plus the Kubernetes system
		// namespaces, MINUS argocd (now permitted), so it stays in lock-step with the
		// registry as platform namespaces are added.
		destinations: [
			{
				server:    IN_CLUSTER
				namespace: "*"
			},
			for ns in DENIED_NAMESPACES if ns != ArgoCDNamespace {
				server:    IN_CLUSTER
				namespace: "!\(ns)"
			},
		]
		namespaceResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
		// clusterResourceWhitelist scoped to EXACTLY the one cluster-scoped kind the
		// project component emits: the Kargo Project (cluster-scoped in Kargo 1.10;
		// it adopts the same-named project control namespace).  This is NOT the
		// platform project's group:"*"/kind:"*" wildcard — the tenant App-of-Apps may
		// create only this single cluster-scoped kind, keeping tenants otherwise
		// confined to namespaced resources.
		clusterResourceWhitelist: [{
			group: "kargo.akuity.io"
			kind:  "Project"
		}]
		// NOTE on the escalation boundary: an argoproj.io/Application *kind* blacklist
		// is deliberately NOT used: a kind blacklist cannot distinguish a tenant's
		// escalating Application from the legitimate per-project/app Applications the
		// projects App-of-Apps must deliver into argocd (HOL-1377), so it would break
		// the very purpose of this project.  The boundary that keeps a TENANT artifact
		// from minting an Application re-projected onto the cluster-privileged platform
		// project is project assignment, not the destination set: only this
		// platform-trusted App-of-Apps and its children are assigned project: projects
		// (and thus may use the argocd destination and the Kargo Project whitelist);
		// every tenant artifact is assigned its own per-project AppProject, which
		// permits neither the argocd destination nor any cluster-scoped resource.
	}
}

// --- Repository registration ----------------------------------------------
//
// Registers the holos-paas-config OCI repository with Argo CD via a repository
// Secret in the argocd namespace (labeled
// argocd.argoproj.io/secret-type: repository).
//
// The repository is PUBLIC (HOL-1381): holos-quay-organization sets the
// holos-paas-config Repository's visibility to `public`, so Argo CD's repo-server
// pulls the App-of-Apps bundle ANONYMOUSLY.  This Secret therefore carries NO
// username/password — no robot pull credential, no secret MATERIAL — so unlike
// the prior create-if-absent robot bootstrap it is rendered and COMMITTED
// directly to the deploy tree (the Runtime Secret Handling guardrail forbids
// committing a Secret's MATERIAL; this Secret has none, only the non-sensitive
// registration constants).  Making the repo public removes the dependency on the
// holos-paas-config-robot SOURCE Secret and the bootstrap Job + RBAC entirely.
//
// The Secret still exists because Argo CD needs the per-repository
// `insecure: "true"` setting even for an anonymous pull: the in-cluster Quay
// serves *.holos.internal with a machine-local mkcert CA certificate that is not
// in the repo-server image's trust store, so without it the pull fails
// `x509: certificate signed by unknown authority` (see
// holos/docs/argocd-application-source.md).  `type: oci` selects the native OCI
// source.  Distributing the local CA into in-cluster trust stores (which would
// let `insecure` drop) is the deferred node-level registry trust placeholder.

// REPO_SECRET is the argocd-format repository registration Secret.
let REPO_SECRET = "holos-paas-config"

// The credential-less repository registration.  stringData carries only the
// non-sensitive constants (name/url/type/insecure) — there is no
// username/password because the repository is public and the pull is anonymous,
// so nothing here is secret MATERIAL and the Secret is safe to commit.
let REPO_REGISTRATION = {
	apiVersion: "v1"
	kind:       "Secret"
	metadata: {
		name:      REPO_SECRET
		namespace: ArgoCDNamespace
		labels: "argocd.argoproj.io/secret-type": "repository"
	}
	type: "Opaque"
	stringData: {
		name:     REPO_SECRET
		url:      CONFIG_REPO_OCI
		type:     "oci"
		insecure: "true"
	}
}

// PROJECT_BUNDLE_REGISTRATIONS is one credential-less repository registration
// Secret per registered project (HOL-1382), registering that project's per-project
// config bundle (oci://quay.holos.internal/holos/<project>-config) with Argo CD.
// Each carries ONLY the non-sensitive registration constants (name/url/type/
// insecure), exactly like REPO_REGISTRATION above: the per-project bundle repos are
// PUBLIC (holos-quay-organization emits a public Repository CR per project), so the
// pull is anonymous and nothing here is secret MATERIAL — the Secrets are safe to
// commit.  The insecure: "true" is still required because the in-cluster Quay serves
// *.holos.internal with the machine-local mkcert CA the repo-server does not trust.
// metadata.name is holos-<project>-config (the holos-org bundle for <project>),
// unambiguous in the argocd namespace.  Applied with the platform floor
// (argocd-projects is in scripts/apply and the platform App-of-Apps), so the
// registrations exist before scripts/apply-project-app-of-apps applies a per-project
// root that pulls the bundle.
let PROJECT_BUNDLE_REGISTRATIONS = {
	for NAME, _ in projects {
		"holos-\(NAME)-config": {
			apiVersion: "v1"
			kind:       "Secret"
			metadata: {
				name:      "holos-\(NAME)-config"
				namespace: ArgoCDNamespace
				labels: "argocd.argoproj.io/secret-type": "repository"
			}
			type: "Opaque"
			stringData: {
				name:     "holos-\(NAME)-config"
				url:      PROJECT_BUNDLE_OCI[NAME]
				type:     "oci"
				insecure: "true"
			}
		}
	}
}

userDefinedBuildPlan: {
	metadata: name: "argocd-projects"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per resource
		// so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the AppProjects
				// (typed argoproj.io/appproject binding) and the credential-less
				// repository registration Secret validate against the vendored
				// schemas at render time.
				resources: #Resources & {
					AppProject: {
						(PLATFORM_PROJECT.metadata.name): PLATFORM_PROJECT
						(PROJECTS_PROJECT.metadata.name): PROJECTS_PROJECT
					}
					Secret: {
						(REPO_REGISTRATION.metadata.name): REPO_REGISTRATION
						// One credential-less registration per per-project bundle (HOL-1382).
						for SECRET_NAME, S in PROJECT_BUNDLE_REGISTRATIONS {
							(SECRET_NAME): S
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

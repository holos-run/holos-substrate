package holos

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// namespaces is the central registry of every platform namespace, rendered by
// the namespaces component (components/namespaces/) as one manifest file per
// Namespace resource.  This file lives at the holos root so it is a CUE
// ancestor of every component instance: register a namespace here, not inline
// in a component.
//
// Mesh enrollment is the registry's main policy surface: platform namespaces
// carrying workloads MUST carry the istio.io/dataplane-mode=ambient label per
// holos/docs/mesh-enrollment.md.  Every entry declares enrollment
// deliberately through the required _ambient field — rendering fails until it
// is set — so an exemption is a reviewable `_ambient: false` with a rationale
// comment, never a silent omission.
//
// The kubernetes.io/metadata.name label is NOT declared here: the repo's
// corev1.#Namespace overlay (cue.mod/usr/k8s.io/api/core/v1/namespace.cue)
// forces it onto every Namespace, matching the value the API server sets
// automatically, so the rendered manifests carry it without any entry
// declaring it.
namespaces: [NAME=string]: corev1.#Namespace & {
	apiVersion: "v1"
	kind:       "Namespace"
	// Namespace names must be RFC 1123 DNS labels — the rule the API server
	// enforces — and NAME flows into the rendered artifact's file path
	// (components/namespaces/buildplan.cue), so reject anything else at
	// render time before it can produce an invalid manifest or escape the
	// deploy tree.
	metadata: name: NAME & =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"

	// _ambient declares whether the namespace's workloads enroll in the
	// Istio ambient mesh; true derives the enrollment label below.  No
	// default: every entry must take a position.
	_ambient: bool
	if _ambient {
		// Enroll every workload in this namespace in the Istio ambient
		// mesh; ztunnel captures their traffic over HBONE.  See
		// holos/docs/mesh-enrollment.md.
		metadata: labels: "istio.io/dataplane-mode": "ambient"
	}
}

// #RegisteredNamespace is the disjunction of every registered namespace
// name.  Components unify their namespace literal with it so silent drift
// between the literal and the registry entry becomes a render failure
// instead of an apply-time NotFound error.  This file is a CUE ancestor of
// every component instance, so components reference this definition rather
// than cloning the comprehension.  It enumerates EVERY entry — the static
// ones below and the env-prefixed ones derived from the projects collection.
#RegisteredNamespace: or([for NAME, _ in namespaces {NAME}])

// --- Project namespace topology (ADR-21 multi-environment) -------------------
//
// A Project is realized not as a single Namespace but as one Namespace PER
// ENVIRONMENT (ADR-21's revised topology): ci-<name>, qa-<name>, prod-<name>.
// The set of environments and the <env>-<name> naming convention are declared
// ONCE here as reusable definitions the Project and Application components
// (HOL-1355/HOL-1356) consume, so the prefix convention lives in a single place.

// #Environments is the canonical, ordered list of per-project delivery
// environments.  Declared once; every derivation of an env-prefixed namespace
// (here, and in the components) iterates this list rather than re-spelling the
// "ci"/"qa"/"prod" strings.  Each value is a DNS-label-safe prefix.
#Environments: ["ci", "qa", "prod"]

// #ProjectControlEnvironment is the environment whose namespace doubles as the
// Project's single CONTROL namespace — where the project-scoped, env-independent
// control-plane CRs live (the Quay Organization, the keycloak.holos.run CRs, and
// the cluster-scoped Kargo Project's adopted namespace).  ADR-21 records the
// rationale for choosing prod-<name>: it is a real, always-present delivery
// environment (no extra un-prefixed namespace to register), and the production
// environment is the natural long-lived home for a project's identity/registry
// control objects.  The components reference THIS definition rather than
// hard-coding "prod".
#ProjectControlEnvironment: "prod"

// #ProjectNamespace maps a (project name, environment) pair to its derived
// namespace name, <env>-<name>.  It is the single source of truth for the
// prefix convention: the comprehension below and the Project/Application
// components all build env namespace names through this definition so the naming
// never drifts.  The result is validated as a DNS label (the same constraint the
// namespaces map key enforces), so an over-long project name that would overflow
// the 63-char label limit once prefixed fails at render.
#ProjectNamespace: {
	project: string
	env:     string
	name:    "\(env)-\(project)" & =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"
}

// #DNSLabel is the RFC 1123 DNS-label pattern (the same regex the namespaces map
// key and the projects/apps collections enforce), named once for reuse by the
// derivations below.
#DNSLabel: =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"

// #ProjectNameNoEnvPrefix REJECTS a project name that begins with a reserved
// "<env>-" prefix (ci-/qa-/prod-).  Such a name's derived BARE control namespace
// (<name>) would collide with another project's derived ENV namespace — e.g. a
// project "prod-foo" derives control namespace "prod-foo", which is also the prod
// env namespace of project "foo" — silently unifying two projects' namespaces and
// (via the Project component's owner-admin RoleBinding) leaking admin across them.
// The pattern is the single env list (#Environments) joined into one alternation,
// so the reserved set stays in lock-step with the environments.  Applied to a
// project's bare control-namespace name so a violating registration fails at
// render.
#ProjectNameNoEnvPrefix: !~"^(\(strings.Join(#Environments, "|")))-"

// #ReservedNamespaceNames is the set of platform-infrastructure namespace names a
// project may NOT take as its name.  A project's derived BARE control namespace is
// the bare project name <name>; without this guard a project named e.g. "argocd",
// "keycloak", or "quay" would derive a control namespace that UNIFIES with the
// existing static platform namespace of that name (rather than failing), and the
// Project component's owner-admin RoleBinding would then grant that project's
// owners admin over the platform namespace — a privilege escalation.  Enumerated
// explicitly (not computed from `namespaces`, which would be circular: the project
// derivations contribute to that same map) and asserted in
// holos/collections.cue's #CollectionsValidated.  Keep this in lock-step with the
// static `namespaces` entries below: adding a platform namespace that a tenant
// could plausibly name-collide with adds an entry here.  (The env-prefixed
// derived names ci-/qa-/prod-<name> are covered by #ProjectNameNoEnvPrefix; the
// reserved set below is the static, non-project platform namespaces.  my-project
// is itself a registered project as of HOL-1357, so it is not reserved.)
#ReservedNamespaceNames: [
	"argocd",
	"cert-manager",
	"cnpg-system",
	"echo",
	"holos-controller",
	"istio-gateways",
	"istio-system",
	"kargo",
	"kargo-cluster-secrets",
	"kargo-echo",
	"kargo-shared-resources",
	"kargo-system-resources",
	"keycloak",
	"quay",
]

// #ProjectNameNotReserved REJECTS a project name equal to any reserved
// platform-namespace name (#ReservedNamespaceNames).  Built as a pattern that is
// the project name unified with "not equal to" each reserved name; expressed as a
// regexp anchored full-string NON-match of the alternation so a single constraint
// covers the whole set and stays single-sourced.
#ProjectNameNotReserved: !~"^(\(strings.Join(#ReservedNamespaceNames, "|")))$"

namespaces: {
	// istio-system hosts the mesh dataplane and control plane themselves:
	// istiod, istio-cni, and ztunnel.  It is deliberately NOT enrolled in
	// ambient: ztunnel is the node proxy that implements enrollment;
	// redirecting its own traffic (or the control plane it synchronizes
	// with) through itself is circular and unsupported.  The mesh
	// infrastructure secures its own control-plane connections natively.
	// See holos/docs/mesh-enrollment.md.
	//
	// Keep this name in sync with IstioNamespace in
	// components/istio/istio.cue: that file is an ancestor only of the istio
	// leaf components, so it cannot be referenced from here.  istio.cue
	// asserts at render time that its value is registered here.
	"istio-system": _ambient: false

	// istio-gateways hosts the auto-provisioned shared Gateway pods.  It is
	// deliberately NOT enrolled in ambient: the gateway pods are Envoy
	// proxies themselves and terminate mesh traffic natively, so redirecting
	// them through ztunnel adds nothing.  See holos/docs/mesh-enrollment.md.
	"istio-gateways": _ambient: false

	// cert-manager hosts the cert-manager controller, webhook, and
	// cainjector; its workloads enroll in the ambient mesh per the platform
	// convention.  scripts/local-ca pre-creates this namespace at cluster
	// bootstrap (the local-ca Secret must exist before cert-manager
	// installs).  The script and the namespaces component both server-side
	// apply this Namespace with kubectl's default field manager, so the
	// script's manifest must carry the same labels as this entry — an apply
	// that omitted the enrollment label would silently strip it.
	//
	// Keep this name and the labels in sync with CertManagerNamespace in
	// components/cert-manager/cert-manager.cue and with the namespace
	// manifest scripts/local-ca creates: cert-manager.cue asserts at render
	// time that its value is registered here.
	"cert-manager": _ambient: true

	// cnpg-system hosts the CloudNativePG operator (the controller-manager
	// Deployment and its webhook Service); its workloads enroll in the
	// ambient mesh per the platform convention for controller namespaces,
	// like cert-manager.
	//
	// Keep this name in sync with CnpgNamespace in
	// components/cnpg/cnpg.cue: that file is an ancestor only of the cnpg
	// leaf components, so it cannot be referenced from here.  cnpg.cue
	// asserts at render time that its value is registered here.
	"cnpg-system": _ambient: true

	// echo is the permanent Layer 0 smoke-test namespace; its workloads
	// enroll in the ambient mesh per the platform convention.
	echo: _ambient: true

	// keycloak hosts the Keycloak server and its CNPG Postgres cluster
	// (keycloak-db, components/cnpg-clusters).  Deliberately NOT enrolled in
	// ambient: Keycloak terminates its own TLS with a cert-manager
	// certificate end-to-end, and the reference platform's root-cause
	// analysis found ztunnel ambient interception breaks Keycloak — the
	// reference sets istio.io/dataplane-mode: none at both the namespace and
	// pod level.  The CNPG Postgres pods in this namespace are consequently
	// unenrolled too; Keycloak↔Postgres traffic stays in-namespace and
	// CNPG/Keycloak handle their own transport security.  See the exceptions
	// section of holos/docs/mesh-enrollment.md.
	keycloak: _ambient: false

	// quay hosts the Quay registry and its CNPG Postgres cluster (quay-db,
	// components/cnpg-clusters); its workloads enroll in the ambient mesh
	// per the platform convention.  This position is final, verified live
	// (HOL-1178): repo_push webhook delivery to cluster-internal plain-HTTP
	// URLs and registry restart resilience both work through ambient
	// interception, so Quay needs no Keycloak-style exception.
	quay: _ambient: true

	// argocd hosts the Argo CD core install (application controller, repo
	// server, server, redis); its workloads enroll in the ambient mesh per
	// the platform convention, following the quay and cert-manager
	// precedent.  The reference platform runs Argo CD in ambient with the
	// server behind the shared Gateway (server.insecure: "true" — the
	// Gateway terminates TLS), so no Keycloak-style exception is needed.
	//
	// Keep this name in sync with ArgoCDNamespace in
	// components/argocd/argocd.cue: that file is an ancestor only of the
	// argocd leaf components, so it cannot be referenced from here.
	// argocd.cue asserts at render time that its value is registered here.
	argocd: _ambient: true

	// The nats, webhook-receiver, and webhook-subscriber namespaces (the NATS
	// event-driven deployment pipeline) were retired in HOL-1241: Kargo plus
	// the client-side ORAS publish workflow (ADR-16) now own deployment,
	// superseding the deprecated receiver/subscriber/deployer path
	// (ADR-9/10/11/14).

	// kargo hosts the Kargo control plane (controller, API/UI, management
	// controller, garbage collector, and the internal and external webhooks
	// servers — components/kargo); its workloads enroll in the ambient mesh
	// per the platform convention, following the argocd and quay
	// precedent for in-cluster services behind the shared Gateway.  The Kargo
	// API runs without authentication on this local single-user cluster (MVP
	// posture — see holos/docs/placeholders.md), so the Gateway→api hop relies
	// on the mesh: the namespace is ambient-enrolled, ztunnel captures traffic
	// over HBONE with mTLS, and the API is reachable from the host only through
	// the shared Gateway at kargo.holos.localhost.  The Kargo CRDs are
	// cluster-scoped and carry no namespace; the kargo-crds component installs
	// them independently of this namespace.
	kargo: _ambient: true

	// kargo-system-resources and kargo-shared-resources are Kargo-internal
	// namespaces the chart references from its RoleBindings: the system one
	// holds namespaced resources that back cluster-scoped Kargo resources (e.g.
	// Secrets a cluster-scoped ClusterConfig references), and the shared one
	// holds credentials shared across Kargo Projects.  The chart would emit
	// these as Namespace resources, but components must not (the component
	// guidelines), so the kargo component disables the chart's namespace
	// creation (global.{system,shared}Resources.createNamespace: false) and the
	// names are registered here instead — they MUST match the
	// global.{system,shared}Resources.namespace chart defaults the chart's
	// RoleBindings target (kargo-system-resources, kargo-shared-resources).
	// Deliberately NOT ambient-enrolled: they carry no workloads, only
	// configuration/credential objects referenced by the Kargo control plane in
	// the (enrolled) kargo namespace, so there is no pod traffic for ztunnel to
	// capture.
	"kargo-system-resources": _ambient: false
	"kargo-shared-resources": _ambient: false

	// kargo-cluster-secrets is the (deprecated) chart concern
	// global.clusterSecretsNamespace, defaulting to this name: it holds Secrets
	// referenced by cluster-scoped Kargo resources (e.g. a ClusterConfig).  The
	// kargo component sets global.createClusterSecretsNamespace: false so the
	// chart does not emit it as a Namespace (components must not), but the chart
	// unconditionally renders Role/RoleBinding objects IN this namespace
	// (role-kargo-cluster-secrets-*, rolebinding-kargo-cluster-secrets-*), so it
	// MUST exist before the kargo component applies or those RBAC objects fail
	// with a NotFound namespace error (codex round 1).  Registering it here is
	// the same pattern as kargo-system-resources / kargo-shared-resources.  The
	// name MUST match global.clusterSecretsNamespace's chart default.
	// Deliberately NOT ambient-enrolled: it carries only Secrets, no workloads.
	"kargo-cluster-secrets": _ambient: false

	// kargo-echo is the Kargo Project namespace for the echo sample app's
	// delivery pipeline (HOL-1240): it holds the Warehouse that watches the
	// rendered-manifests OCI artifact and the Stage whose promotion patches the
	// echo Argo CD Application's OCI targetRevision (components/kargo-project-echo
	// and components/kargo-echo).  It is deliberately a DEDICATED Kargo Project
	// namespace rather than the echo workload namespace: a Kargo Project adopts a
	// same-named namespace and adds its own kargo.akuity.io/finalizer to it, so
	// pointing the Project at the echo namespace (server-side-applied by the
	// namespaces component) would risk finalizer/label contention between the two
	// reconcilers.  Keeping the Project's namespace separate isolates Kargo's
	// ownership; the Stage targets the echo workload through the Argo CD
	// Application, not by sharing a namespace.
	//
	// The kargo.akuity.io/project label lets the Kargo Project controller ADOPT
	// this pre-created namespace instead of refusing it (the controller requires
	// the exact label value "true" to adopt an existing namespace), and the
	// kargo.akuity.io/keep-namespace annotation tells Kargo not to delete this
	// namespace if the Project is ever removed, since the namespaces component
	// owns the base Namespace object.  Deliberately NOT ambient-enrolled: it
	// carries only Kargo control resources (Warehouse, Stage, Freight,
	// Promotion), not workloads, so there is no pod traffic for ztunnel to
	// capture.
	"kargo-echo": {
		_ambient: false
		metadata: {
			labels: "kargo.akuity.io/project":             "true"
			annotations: "kargo.akuity.io/keep-namespace": "true"
		}
	}

	// my-project's bare control namespace is no longer a STATIC entry: as of
	// HOL-1357 the bespoke holos/components/my-project component was deleted and
	// my-project is a registered project (holos/projects/my-project.cue), so its
	// bare control namespace is DERIVED by the per-project comprehension below
	// (alongside ci-/qa-/prod-my-project) — the same ambient-enrolled,
	// Kargo-adoption-labelled shape this static entry carried.  The derivation no
	// longer skips my-project (it formerly did, only so this static entry would not
	// duplicate it while the bespoke component still referenced it).

	// holos-controller hosts the Holos Controller (ADR-18): the
	// controller-runtime manager that reconciles the quay.holos.run API group's
	// Organization and Repository custom resources (ADR-19) against the
	// in-cluster Quay registry.  Its workloads enroll in the ambient mesh per the
	// platform convention for controller namespaces, like cert-manager and
	// cnpg-system.
	//
	// The Namespace object is owned here centrally (the component guidelines
	// forbid a component creating its own namespace); the conventional kubebuilder
	// config/ kustomize tree (config/default) merely TARGETS this namespace and
	// does not duplicate Namespace creation.  The controller resolves its
	// credential Secret (holos-controller-quay-creds) from this namespace via the
	// downward-API POD_NAMESPACE the manager Deployment sets (HOL-1313).
	"holos-controller": _ambient: true

	// --- Derived: per-project, per-environment namespaces (ADR-21) -----------
	//
	// For every projects.<name> entry (the projects collection, bound into the
	// root `holos` scope by holos/collections.cue) derive one namespace per
	// environment in #Environments: ci-<name>, qa-<name>, prod-<name>.  This is
	// the collection-driven generalization that formerly mirrored the hand-written
	// my-project entry — ADR-21 calls it "the design's hardest constraint": a
	// one-line project registration must produce correctly-labelled,
	// ambient-enrolled registry entries WITHOUT a component emitting a Namespace
	// inline (the no-inline-Namespace guardrail, holos/docs/component-guidelines.md).
	//
	// Each derived entry carries:
	//   - _ambient: true (the project namespaces carry workloads and enroll in
	//     the mesh).
	//   - the kargo.akuity.io/project: "true" ADOPTION label, so the Kargo Project
	//     controller adopts the pre-created namespace instead of refusing it.
	//   - the kargo.akuity.io/keep-namespace: "true" annotation, so Kargo does not
	//     delete the namespace if the Project is removed (the namespaces component
	//     owns the base Namespace object).
	//
	// The name is built through #ProjectNamespace so the <env>-<name> convention
	// is single-sourced and the derived name is DNS-label validated.  As of
	// HOL-1357 the static my-project entry was removed and my-project is a
	// registered project, so its bare control namespace and ci-/qa-/prod-my-project
	// env namespaces are all DERIVED here, exactly like any other project.
	// Derived: per-project CONTROL namespace — the bare project name <name>.
	//
	// The Project component (HOL-1355) places a project's control-plane CRs (the
	// keycloak.holos.run role/custodian groups + owner user + client, the Quay
	// Organization, and the cluster-scoped Kargo Project's adopted namespace) in a
	// namespace named EXACTLY the bare project name.  This is forced by the
	// as-built controller guard validateDirectClientRole (HOL-1350): a role group
	// may confer <name>-<role> on the platform Quay client directly only when its
	// CR namespace equals the bare project name <name> — the Quay claim-population
	// the Project's syncedTeams depend on.  This DEVIATES from ADR-21 Revision 3's
	// prod-<name> control-namespace pick (recorded as a Deferred AC on HOL-1355,
	// to be ratified in ADR-21 by HOL-1358); the bare-<name> control namespace is
	// also exactly what the bespoke my-project component uses today.
	//
	// As of HOL-1357 this comprehension derives a bare control namespace for EVERY
	// registered project, including my-project: the bespoke component and the
	// hand-written static my-project entry were both removed, so the derivation is
	// now the sole producer of the bare my-project namespace.  The derived bare
	// entry reproduces the env entries' shape (ambient + the Kargo adoption
	// label/keep annotation), since the control namespace also hosts the
	// cluster-scoped Kargo Project that adopts it.
	//
	// COLLISION GUARD: a project literally named "<env>-<other>" (e.g. "prod-foo")
	// would derive a BARE control namespace "prod-foo" that collides with the prod
	// env namespace of a project "foo" — a cross-project namespace takeover (the
	// owner-admin RoleBinding the Project component emits there would leak admin
	// across the two projects).  That env-prefixed name is rejected at RENDER by
	// #CollectionsValidated.projectNamesNoEnvPrefix (holos/collections.cue), which
	// fails HARD via the namespaces component's _collectionsValidated reference —
	// a bottom map KEY here is silently dropped by CUE and would NOT raise an
	// error, so the rejection cannot live on this comprehension's key.  Here NS is
	// only DNS-label validated; the env-prefix rejection is the collections-level
	// assertion.
	for PROJECT, _ in projects {
		let NS = PROJECT & #DNSLabel
		(NS): {
			_ambient: true
			metadata: {
				labels: "kargo.akuity.io/project":             "true"
				annotations: "kargo.akuity.io/keep-namespace": "true"
			}
		}
	}

	for PROJECT, _ in projects {
		for ENV in #Environments {
			let NS = (#ProjectNamespace & {project: PROJECT, env: ENV}).name
			(NS): {
				_ambient: true
				metadata: {
					labels: "kargo.akuity.io/project":             "true"
					annotations: "kargo.akuity.io/keep-namespace": "true"

					// On the CONTROL namespace (prod-<name>,
					// #ProjectControlEnvironment), record the project's validated
					// apps as an annotation built from #CollectionsValidated.tokens
					// (the per-app name|project|image|port INTERPOLATION).  This is
					// what puts the apps contract on the render path: the namespaces
					// component EXPORTS this annotation, and exporting an
					// interpolation of an INCOMPLETE value (a required app field
					// omitted entirely) is a render error — so a malformed OR
					// MISSING-required-field app fails here, alongside the
					// _|_-producing cases the collections.cue hidden reference
					// already catches.  Only this project's apps are folded in (the
					// app's `project` selects its control namespace), so an app
					// annotates exactly one namespace and the value is deterministic.
					// When a project has no apps the annotation is absent, so the
					// committed tree stays diff-clean until apps are registered.
					if ENV == #ProjectControlEnvironment {
						for ANAME, A in apps if A.project == PROJECT {
							// The annotation KEY is app.holos.run/<app-name>.  The
							// name segment after the "/" must be ≤63 chars (the
							// Kubernetes annotation-key segment limit); ANAME is
							// already #DNSLabel-bounded to ≤63, so the bare app name
							// is a valid segment.  Do NOT prefix it (e.g. "app.")
							// here — that would push a 60-63-char app name past 63
							// and render an invalid annotation key.
							annotations: "app.holos.run/\(ANAME)": #CollectionsValidated.tokens[ANAME]
						}
					}
				}
			}
		}
	}
}

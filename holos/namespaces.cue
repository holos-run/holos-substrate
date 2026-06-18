package holos

import (
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
// than cloning the comprehension.
#RegisteredNamespace: or([for NAME, _ in namespaces {NAME}])

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

	// my-project is the Kargo Project namespace for the my-project sample
	// application's project-config delivery pipeline (HOL-1268): it holds the
	// Kargo Project, ProjectConfig (quay webhook receiver), Warehouse, and the
	// project-config promotion Stage added in the next phase (HOL-1270), whose
	// promotion patches the my-project Argo CD Application's OCI targetRevision.
	// Unlike the dedicated kargo-echo Project namespace, my-project IS the
	// workload namespace as well: the Argo CD Application's destination targets
	// it directly, so the rendered my-project-config artifact deploys here.  It
	// is therefore ambient-enrolled (its workloads enroll in the mesh per the
	// platform convention), in contrast to kargo-echo, which carries only Kargo
	// control resources and stays unenrolled.
	//
	// The kargo.akuity.io/project label lets the Kargo Project controller ADOPT
	// this pre-created namespace instead of refusing it (the controller requires
	// the exact label value "true" to adopt an existing namespace), and the
	// kargo.akuity.io/keep-namespace annotation tells Kargo not to delete this
	// namespace if the Project is ever removed, since the namespaces component
	// owns the base Namespace object.  This mirrors the kargo-echo adopt pattern
	// above.
	"my-project": {
		_ambient: true
		metadata: {
			labels: "kargo.akuity.io/project":             "true"
			annotations: "kargo.akuity.io/keep-namespace": "true"
		}
	}

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
}

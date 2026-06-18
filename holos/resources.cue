package holos

import (
	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	batchv1 "k8s.io/api/batch/v1"

	ci "cert-manager.io/clusterissuer/v1"
	rgv1 "gateway.networking.k8s.io/referencegrant/v1beta1"
	certv1 "cert-manager.io/certificate/v1"
	hrv1 "gateway.networking.k8s.io/httproute/v1"
	gwv1 "gateway.networking.k8s.io/gateway/v1"
	ap "argoproj.io/appproject/v1alpha1"
	app "argoproj.io/application/v1alpha1"
	kwarehouse "kargo.akuity.io/warehouse/v1alpha1"
	kstage "kargo.akuity.io/stage/v1alpha1"
	es "external-secrets.io/externalsecret/v1beta1"
	ss "external-secrets.io/secretstore/v1beta1"
	cnpg "postgresql.cnpg.io/cluster/v1"
	kc "k8s.keycloak.org/keycloak/v2beta1"
	kcri "k8s.keycloak.org/keycloakrealmimport/v2beta1"
	dr "networking.istio.io/destinationrule/v1"
	se "networking.istio.io/serviceentry/v1"
	azp "security.istio.io/authorizationpolicy/v1"
)

#Resources: {
	[Kind=string]: [InternalLabel=string]: {
		kind: Kind
		// metadata is OPEN (the trailing `...`) so every resource may carry the
		// standard object-meta fields (namespace, labels, annotations, …) that
		// the typed bindings below already permit and that the open Project /
		// ProjectConfig entries need.  The ENTRY ITSELF stays closed: a Kind
		// without a typed binding (e.g. a misspelled Warehose) still cannot
		// carry a spec, so render-time validation keeps catching typos.
		metadata: {
			name: string | *InternalLabel
			...
		}
	}

	AppProject?: [_]:          ap.#AppProject
	Application?: [_]:         app.#Application
	AuthorizationPolicy?: [_]: azp.#AuthorizationPolicy
	Certificate?: [_]:         certv1.#Certificate
	Cluster?: [_]:             cnpg.#Cluster
	ClusterIssuer?: [_]:       ci.#ClusterIssuer
	ClusterRole?: [_]:         rbacv1.#ClusterRole
	ClusterRoleBinding?: [_]:  rbacv1.#ClusterRoleBinding
	ConfigMap?: [_]:           corev1.#ConfigMap
	CronJob?: [_]:             batchv1.#CronJob
	Deployment?: [_]:          appsv1.#Deployment
	DestinationRule?: [_]:     dr.#DestinationRule
	ExternalSecret?: [_]:      es.#ExternalSecret
	HTTPRoute?: [_]:           hrv1.#HTTPRoute
	Job?: [_]:                 batchv1.#Job
	// Keycloak CRs use v2beta1, the storage version of the pinned Keycloak
	// 26.6.3 CRDs (v2alpha1 is served for compatibility but deprecated; both
	// are vendored under cue.mod/gen/k8s.keycloak.org/).
	Keycloak?: [_]:              kc.#Keycloak
	KeycloakRealmImport?: [_]:   kcri.#KeycloakRealmImport
	Namespace?: [_]:             corev1.#Namespace
	// The quay.holos.run Organization kind (ADR-19, the shipped Holos
	// Controller) gets an explicit but DELIBERATELY OPEN entry (the trailing
	// `...`) rather than a vendored binding: the controller's CRDs have no
	// generated CUE type under cue.mod/gen/ (they live in api/quay/v1alpha1/,
	// not a vendored chart).  The openness is SCOPED to this kind, like the
	// Kargo Project / ProjectConfig entries below, so the generic catch-all
	// above stays CLOSED and a misspelled Kind still fails render-time
	// validation.  my-project (HOL-1322) emits an Organization through
	// #Resources, so the entry must exist and be open.
	Organization?: [_]: {
		kind: "Organization"
		metadata: name: string
		...
	}
	PersistentVolumeClaim?: [_]: corev1.#PersistentVolumeClaim
	// The Kargo Project and ProjectConfig kinds (kargo.akuity.io) get explicit
	// but DELIBERATELY OPEN entries (the trailing `...`) rather than a vendored
	// binding:
	//   - Project: the vendored #Project binding
	//     (cue.mod/gen/kargo.akuity.io/project/v1alpha1) is STALE for the Kargo
	//     1.10.3 CRD this platform installs (components/kargo-crds).  The binding
	//     carries a required spec! (#ProjectSpec with promotionPolicies) from an
	//     older Kargo, but the 1.10.3 Project CRD is cluster-scoped with NO spec
	//     at all (the promotion policy moved onto the namespaced ProjectConfig
	//     CRD).  A binding-typed Project? entry would force every Project author
	//     into the wrong schema — a spec the server prunes or rejects.
	//     components/kargo-project-echo authors its Project as a plain struct
	//     outside #Resources for exactly this reason; my-project (HOL-1270)
	//     unifies through #Resources, so the entry must exist and be open.
	//   - ProjectConfig: has no generated CUE type under cue.mod/gen/ at all.
	// They are OPEN (apiVersion, spec, metadata.namespace, labels, …) precisely
	// because no schema constrains them; this openness is SCOPED to these two
	// kinds only, so the generic catch-all above stays CLOSED and a misspelled
	// Kind (e.g. Warehose) still fails render-time validation.  Warehouse and
	// Stage below are fully typed: their 1.10.3 CRDs are namespaced with a
	// required spec matching the vendored bindings (kargo-echo validates against
	// them today).
	Project?: [_]: {
		kind: "Project"
		metadata: name: string
		...
	}
	ProjectConfig?: [_]: {
		kind: "ProjectConfig"
		metadata: name: string
		...
	}
	ReferenceGrant?: [_]:        rgv1.#ReferenceGrant
	Role?: [_]:                  rbacv1.#Role
	RoleBinding?: [_]:           rbacv1.#RoleBinding
	Secret?: [_]:                corev1.#Secret
	SecretStore?: [_]:           ss.#SecretStore
	Service?: [_]:               corev1.#Service
	ServiceAccount?: [_]:        corev1.#ServiceAccount
	ServiceEntry?: [_]:          se.#ServiceEntry
	Stage?: [_]:                 kstage.#Stage
	StatefulSet?: [_]:           appsv1.#StatefulSet
	Warehouse?: [_]:             kwarehouse.#Warehouse

	Gateway?: [_]: gwv1.#Gateway & {
		spec: gatewayClassName: string | *"istio"
	}
}

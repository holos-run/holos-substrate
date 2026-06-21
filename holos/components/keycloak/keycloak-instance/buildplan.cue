// keycloak-instance emits the central keycloak.holos.run KeycloakInstance the
// shipped Holos Controller (ADR-18/ADR-20) reconciles the rest of the
// keycloak.holos.run Kinds against, plus the security.holos.run ReferenceGrant(s)
// that authorize project namespaces (my-project) to reference that instance
// cross-namespace (HOL-1348).
//
// It is a sibling leaf of the operator, operator-crds, instance, and
// realm-config components and inherits KeycloakNamespace from the shared
// ancestor ../keycloak.cue.  The KeycloakInstance and the ReferenceGrant both
// live in the keycloak namespace: the ReferenceGrant convention places the grant
// in the REFERENT (target) namespace — the KeycloakInstance's namespace — naming
// the referrer namespaces it trusts (internal/referencegrant/authorize.go,
// ADR-22).
//
// The controller authenticates to this instance with the credential the
// realm-config component's CONTROLLER_CREDS_BOOTSTRAP Job provisions into the
// holos-controller-keycloak-creds Secret (credentialsSecretRef's default,
// resolved from the controller's own namespace; api/keycloak/v1alpha1
// DefaultCredentialsSecretName).  spec.caBundle carries the per-cluster local-ca
// PEM so the controller trusts the in-cluster Keycloak's mkcert-signed serving
// certificate (the same trust-anchor pattern as the my-project Organization);
// the PEM is injected at apply time via the _CABundlePEM tag and never committed.
package holos

import (
	"encoding/base64"
)

let NAMESPACE = KeycloakNamespace

// NAME is the KeycloakInstance the project Keycloak CRs reference by
// instanceRef.name; it matches the ADR-20 worked examples (holos-keycloak in the
// keycloak namespace).  The my-project KeycloakGroup/KeycloakUser/KeycloakClient
// CRs set instanceRef: {name: "holos-keycloak", namespace: "keycloak"}.
let NAME = "holos-keycloak"

// REALM is the realm this instance binds (the platform realm the realm-config
// component reconciles).  A KeycloakInstance binds exactly one realm.
let REALM = "holos"

// The in-cluster Keycloak admin API URL.  The operator names the Keycloak
// Service "<cr-name>-service" = "keycloak-service", serving HTTPS on 8443 (the
// SAN the keycloak-tls cert covers — ../instance/buildplan.cue).  The controller
// reaches it by the in-namespace short name when it shares the keycloak
// namespace and by the fully-qualified name otherwise; the controller runs in
// holos-controller, so use the cluster-FQDN form that resolves from any
// namespace.  An absolute https URL is required (the KeycloakInstance CRD rejects
// http at admission).
let KEYCLOAK_URL = "https://keycloak-service.\(NAMESPACE).svc:8443"

// CONTROLLER_CREDS_SECRET is the credential Secret the controller reads (the
// realm-config CONTROLLER_CREDS_BOOTSTRAP Job writes it; named here explicitly to
// match the default and document the contract).
let CONTROLLER_CREDS_SECRET = "holos-controller-keycloak-creds"

// PROJECT_NAMESPACES are the project namespaces the ReferenceGrant authorizes to
// reference this instance.  Derived from the `projects` collection (HOL-1355):
// each project's bare <name> CONTROL namespace — where the Project component
// places its keycloak.holos.run CRs (the bare-<name> control-namespace resolution
// forced by the controller's validateDirectClientRole guard; see
// holos/components/project/buildplan.cue).  As of HOL-1357 the derivation covers
// EVERY project including my-project (the bespoke component and its static literal
// were removed; my-project is now a registered project), so the grant authorizes
// the bare my-project namespace exactly as the static literal did before.  Each
// entry is unified with #RegisteredNamespace so a typo or a removed registry entry
// is a render failure rather than a silent unauthorized reference at reconcile
// time.
let PROJECT_NAMESPACES = [
	for PROJECT, _ in projects {
		PROJECT & #RegisteredNamespace
	},
]

// KEYCLOAK_GROUP is the API group every keycloak.holos.run referrer and the
// KeycloakInstance target share.
let KEYCLOAK_GROUP = "keycloak.holos.run"

// The referrer Kinds the project namespaces hold — each carries an instanceRef
// the controller gates through this grant (the FromRef Kinds in
// internal/controller/keycloak/{group,user,client}_controller.go).
let REFERRER_KINDS = ["KeycloakGroup", "KeycloakUser", "KeycloakClient"]

// KEYCLOAK_INSTANCE_RESOURCE is the central KeycloakInstance.  spec.caBundle is
// GATED on a non-empty _CABundlePEM tag (holos/tags.cue): empty (the default,
// e.g. during `holos render platform` and scripts/render's clean-tree gate)
// omits the field entirely, so the committed deploy tree carries no per-cluster
// CA material — the runtime-secret posture, injected at apply time by the
// platform apply.  base64.Encode(null, _CABundlePEM) renders the PEM as the
// single base64 string the caBundle []byte field serializes to (api/keycloak/v1alpha1
// CABundle convention).
let KEYCLOAK_INSTANCE_RESOURCE = {
	apiVersion: "keycloak.holos.run/v1alpha1"
	kind:       "KeycloakInstance"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": "keycloak"
	}
	spec: {
		url:   KEYCLOAK_URL
		realm: REALM
		credentialsSecretRef: name: CONTROLLER_CREDS_SECRET
		if _CABundlePEM != "" {
			caBundle: base64.Encode(null, _CABundlePEM)
		}
	}
}

// REFERENCE_GRANT_RESOURCE is the security.holos.run ReferenceGrant in the
// KeycloakInstance's namespace.  It authorizes the project namespaces'
// keycloak.holos.run referrers (KeycloakGroup/User/Client) to reference this
// KeycloakInstance by name.  from lists every (referrer namespace × referrer
// Kind) pair; to constrains the grant to this one KeycloakInstance by name (the
// least-privilege ToRef.Name match the authorizer evaluates).
let REFERENCE_GRANT_RESOURCE = {
	apiVersion: "security.holos.run/v1alpha1"
	kind:       "ReferenceGrant"
	metadata: {
		name:      "\(NAME)-projects"
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": "keycloak"
	}
	spec: {
		from: [
			for ns in PROJECT_NAMESPACES
			for k in REFERRER_KINDS {
				group:     KEYCLOAK_GROUP
				kind:      k
				namespace: ns
			},
		]
		to: [{
			group: KEYCLOAK_GROUP
			kind:  "KeycloakInstance"
			name:  NAME
		}]
	}
}

userDefinedBuildPlan: {
	metadata: name: "keycloak-instance"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per resource
		// so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue): KeycloakInstance and
				// the security.holos.run ReferenceGrant ride the open, Kind-scoped
				// entries there (their CRDs have no vendored CUE type).
				resources: #Resources & {
					KeycloakInstance: (NAME):                   KEYCLOAK_INSTANCE_RESOURCE
					ReferenceGrant: (REFERENCE_GRANT_RESOURCE.metadata.name): REFERENCE_GRANT_RESOURCE
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

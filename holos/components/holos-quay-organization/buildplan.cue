// holos-quay-organization emits the platform's own quay.holos.run Organization —
// the `holos` Quay organization — plus the public `holos-controller` Repository
// the shipped Holos Controller (ADR-18/ADR-19) reconciles into the in-cluster
// Quay registry (HOL-1380).  This is the bootstrap home of the two OCI artifacts
// the platform delivers from a fresh cluster: the holos-controller container
// image and (in the holos-paas-config repo the App-of-Apps bundle is pushed to)
// the platform config bundle.
//
// It is a SIBLING of the collection-driven `project` / `application` components,
// but it is NOT a tenant project: it carries no Kargo/Argo CD/Keycloak control
// plane, only the org + one repo.  Like those components (and keycloak-instance)
// its caBundle is injected at apply time via the _CABundlePEM tag and never
// committed, so it is render-here / apply-separately: EXCLUDED from the master
// scripts/apply COMPONENTS and applied by scripts/apply-holos-quay-organization,
// which injects the per-cluster local-ca PEM the controller trusts the in-cluster
// Quay's mkcert-signed serving certificate with.
//
// "All authenticated users may push and pull": the holos-controller Repository is
// PUBLIC (world-pullable, including unauthenticated pulls during bootstrap before
// any robot exists), and the org binds an OIDC-synced Quay team to the realm's
// `authenticated` default group (every Keycloak realm user is a member, see
// holos/components/keycloak/realm-config/buildplan.cue) with org role `creator`
// and an org default `write` repository permission — so every authenticated user
// can create repos in the org (e.g. the holos-paas-config bundle repo) and
// push/pull existing ones.  Automated pushes (the controller image, the
// App-of-Apps bundle) still use a Quay ROBOT credential (scripts/publish-config's
// ORAS_USERNAME/ORAS_PASSWORD); the runbook the apply script prints explains how
// to obtain one.
package holos

import (
	"encoding/base64"
)

// NAMESPACE hosts the Organization and Repository CRs.  holos-controller is the
// controller's own namespace (holos/namespaces.cue): the credential Secret is
// always resolved there regardless of the CR namespace
// (internal/controller/quay/credentials.go), and this is platform infrastructure
// owned by the controller, not a tenant project namespace.
let NAMESPACE = "holos-controller"

// ORG_NAME is the Quay organization name (and the Organization CR name).
let ORG_NAME = "holos"

// CONTROLLER_REPO is the public repository the holos-controller image is pushed
// to and pulled from.
let CONTROLLER_REPO = "holos-controller"

// CREDS_SECRET is the controller's Quay superuser OAuth-Application credential
// (named explicitly to document the contract; it is also the field default).
// scripts/apply-svc-quay-resource-controller-creds provisions it.
let CREDS_SECRET = "holos-controller-quay-creds"

// AUTHENTICATED_GROUP is the realm default group every authenticated Keycloak
// user belongs to; its bare name is what the OIDC groups claim carries
// (holos/components/keycloak/realm-config/buildplan.cue).
let AUTHENTICATED_GROUP = "authenticated"

// ORGANIZATION_RESOURCE is the quay.holos.run Organization the Holos Controller
// creates (spec.adopt: false).  Its single synced team grants every
// authenticated user creator + org-default write, the "all authenticated users
// push and pull" intent.  spec.caBundle carries the per-cluster local-ca PEM only
// when _CABundlePEM is injected at apply time (scripts/apply-holos-quay-organization);
// the committed tree omits it (the runtime-secret posture).
let ORGANIZATION_RESOURCE = {
	apiVersion: "quay.holos.run/v1alpha1"
	kind:       "Organization"
	metadata: {
		name:      ORG_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": ORG_NAME
	}
	spec: {
		name:  ORG_NAME
		email: "\(ORG_NAME)@holos.internal"
		credentialsSecretRef: name: CREDS_SECRET
		adopt: false
		syncedTeams: [
			{
				name:                 "\(ORG_NAME)-authenticated"
				oidcGroup:            AUTHENTICATED_GROUP
				role:                 "creator"
				repositoryPermission: "write"
			},
		]
		if _CABundlePEM != "" {
			caBundle: base64.Encode(null, _CABundlePEM)
		}
	}
}

// REPOSITORY_RESOURCE is the public holos-controller Repository within the org.
// organizationRef names the Organization CR in this same namespace (never a Quay
// org by string); spec.name is the repo name within the org.  Public visibility
// makes it world-pullable.  caBundle follows the same gated-injection posture as
// the Organization.
let REPOSITORY_RESOURCE = {
	apiVersion: "quay.holos.run/v1alpha1"
	kind:       "Repository"
	metadata: {
		name:      CONTROLLER_REPO
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": ORG_NAME
	}
	spec: {
		organizationRef: ORG_NAME
		name:            CONTROLLER_REPO
		visibility:      "public"
		description:     "The holos-controller container image (HOL-1380)."
		credentialsSecretRef: name: CREDS_SECRET
		if _CABundlePEM != "" {
			caBundle: base64.Encode(null, _CABundlePEM)
		}
	}
}

userDefinedBuildPlan: {
	metadata: name: "holos-quay-organization"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per resource
		// so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue): Organization and
				// Repository ride the open, Kind-scoped entries there (their CRDs
				// have no vendored CUE type).
				resources: #Resources & {
					Organization: (ORG_NAME):     ORGANIZATION_RESOURCE
					Repository: (CONTROLLER_REPO): REPOSITORY_RESOURCE
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

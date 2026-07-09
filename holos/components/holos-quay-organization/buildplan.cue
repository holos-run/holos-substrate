// holos-quay-organization emits the platform's own quay.holos.run Organization —
// the `holos` Quay organization — plus the public `holos-controller` Repository
// the shipped Holos Controller (ADR-18/ADR-19) reconciles into the in-cluster
// Quay registry (HOL-1380).  This is the bootstrap home of the two OCI artifacts
// the platform delivers from a fresh cluster: the holos-controller container
// image and (in the holos-substrate-config repo the App-of-Apps bundle is pushed to)
// the platform config bundle.
//
// It is a SIBLING of the collection-driven `project` / `application` components,
// but it is NOT a tenant project: it carries no Kargo/Argo CD/Keycloak control
// plane, only the org + its two repos (the public holos-controller image repo and
// the public holos-substrate-config bundle repo).  Like those components (and
// keycloak-instance)
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
// can create repos in the org (e.g. the holos-substrate-config bundle repo) and
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

// AUTHENTICATOR_REPO is the public repository the holos-authenticator image
// (HOL-1389) is pushed to and pulled from: quay.holos.internal/holos/
// holos-authenticator, the image the holos-authenticator component's manager
// Deployment references.  Public (world-pullable) like CONTROLLER_REPO so the
// manager pod pulls it with no imagePullSecret; managed as a CR so the repo
// exists before the image is pushed (the push robot then needs only WRITE,
// not org creator/repo-create — the same posture as the other repos here).
let AUTHENTICATOR_REPO = "holos-authenticator"

// CONFIG_REPO is the repository the App-of-Apps platform config bundle
// (holos-substrate-config:dev) is pushed to and Argo CD pulls from.  It is managed as
// a CR — rather than relying on a first-push to create it — so the push robot
// only needs WRITE (push) access, not org `creator`/repo-create rights, and the
// scripts/apply-platform-app-of-apps push targets an already-existing repo (round-1
// review finding).  It is PUBLIC (HOL-1381): Argo CD pulls the bundle
// ANONYMOUSLY, so the platform no longer depends on a holos-substrate-config-robot
// pull credential — see CONFIG_REPOSITORY_RESOURCE below and the credential-less
// repository registration in components/argocd-projects.
let CONFIG_REPO = "holos-substrate-config"

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

// AUTHENTICATOR_REPOSITORY_RESOURCE is the public holos-authenticator image
// Repository within the org (HOL-1389) — the mirror of REPOSITORY_RESOURCE for
// the authenticator's manager image.  Public visibility makes it world-pullable
// so the manager pod pulls quay.holos.internal/holos/holos-authenticator:dev
// without an imagePullSecret; managed as a CR so it exists before the image is
// pushed.  Same gated-injection posture for caBundle.
let AUTHENTICATOR_REPOSITORY_RESOURCE = {
	apiVersion: "quay.holos.run/v1alpha1"
	kind:       "Repository"
	metadata: {
		name:      AUTHENTICATOR_REPO
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": ORG_NAME
	}
	spec: {
		organizationRef: ORG_NAME
		name:            AUTHENTICATOR_REPO
		visibility:      "public"
		description:     "The holos-authenticator container image (HOL-1389)."
		credentialsSecretRef: name: CREDS_SECRET
		if _CABundlePEM != "" {
			caBundle: base64.Encode(null, _CABundlePEM)
		}
	}
}

// CONFIG_REPOSITORY_RESOURCE is the holos-substrate-config Repository — the
// App-of-Apps platform config bundle's home — managed declaratively so it exists
// before scripts/apply-app-of-apps pushes to it (the push robot then needs only
// write, not repo-create).  PUBLIC (HOL-1381, world-pullable including
// unauthenticated pulls): Argo CD pulls the bundle anonymously, so the platform
// no longer needs a holos-substrate-config-robot pull credential — the
// components/argocd-projects registration carries only the non-sensitive
// insecure/type settings, with no robot username/password.  Same gated-injection
// posture for caBundle.
let CONFIG_REPOSITORY_RESOURCE = {
	apiVersion: "quay.holos.run/v1alpha1"
	kind:       "Repository"
	metadata: {
		name:      CONFIG_REPO
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": ORG_NAME
	}
	spec: {
		organizationRef: ORG_NAME
		name:            CONFIG_REPO
		visibility:      "public"
		description:     "The App-of-Apps platform config bundle (HOL-1380)."
		credentialsSecretRef: name: CREDS_SECRET
		if _CABundlePEM != "" {
			caBundle: base64.Encode(null, _CABundlePEM)
		}
	}
}

// PROJECT_CONFIG_REPOSITORIES is one PUBLIC Repository per registered project
// (HOL-1382): the per-project App-of-Apps config bundle's home,
// holos/<project>-config, pushed to by scripts/apply-project-app-of-apps and
// pulled ANONYMOUSLY by the project-app-of-apps roots (so the
// components/argocd-projects registration for each carries no credential, exactly
// like holos-substrate-config).  Managed as a CR — rather than relying on a first-push
// to create it — so the push robot needs only WRITE (push) access, not org
// `creator`/repo-create rights, and the per-project bundle repo exists before the
// per-project root reconciles.  Same gated-injection posture for caBundle.  These
// live in the holos org alongside holos-controller / holos-substrate-config; the
// per-APP delivery repos (oci://quay.holos.internal/<project>/<app>-config) are a
// separate concern under each project's OWN Quay org (the project Organization).
let PROJECT_CONFIG_REPOSITORIES = {
	for NAME, _ in projects {
		"\(NAME)-config": {
			apiVersion: "quay.holos.run/v1alpha1"
			kind:       "Repository"
			metadata: {
				name:      "\(NAME)-config"
				namespace: NAMESPACE
				labels: "app.kubernetes.io/name": ORG_NAME
			}
			spec: {
				organizationRef: ORG_NAME
				name:            "\(NAME)-config"
				visibility:      "public"
				description:     "The per-project App-of-Apps config bundle for project \(NAME) (HOL-1382)."
				credentialsSecretRef: name: CREDS_SECRET
				if _CABundlePEM != "" {
					caBundle: base64.Encode(null, _CABundlePEM)
				}
			}
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
					Organization: (ORG_NAME): ORGANIZATION_RESOURCE
					Repository: {
						(CONTROLLER_REPO): REPOSITORY_RESOURCE
						(AUTHENTICATOR_REPO): AUTHENTICATOR_REPOSITORY_RESOURCE
						(CONFIG_REPO):     CONFIG_REPOSITORY_RESOURCE
						// One public bundle repo per registered project (HOL-1382).
						for REPO_NAME, R in PROJECT_CONFIG_REPOSITORIES {
							(REPO_NAME): R
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

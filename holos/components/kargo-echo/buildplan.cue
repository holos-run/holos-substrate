package holos

import (
	kargowarehouse "kargo.akuity.io/warehouse/v1alpha1"
	kargostage "kargo.akuity.io/stage/v1alpha1"
	application "argoproj.io/application/v1alpha1"
)

// kargo-echo wires the Kargo delivery pipeline for the echo sample app
// (HOL-1240): a Warehouse that watches the rendered-manifests OCI artifact the
// client-side publish workflow pushes, a Stage whose promotion patches the echo
// Argo CD Application's OCI targetRevision to the new artifact digest, and the
// target Argo CD Application itself.  Together they close the loop the NATS
// deployer subscriber used to close — now driven by Kargo (ADR-16):
//
//   scripts/publish → new OCI artifact in Quay
//     → Warehouse discovers it → creates Freight
//       → auto-promotion runs the Stage's argocd-update step
//         → Argo CD Application.spec.source.targetRevision = <new digest>
//           → Argo CD syncs the new rendered manifests.
//
// The Kargo Project for this pipeline is the sibling kargo-project-echo
// component; these resources live in that Project's namespace (PROJECT below).
//
// Promotion uses argocd-update only — NOT helm-template (the explicit
// Kustomize-not-Helm decision from the parent issue, HOL-1236).  No in-promotion
// manifest assembly is needed: the publish workflow already ran `kustomize
// build` client-side and pushed the finished manifests, and Argo CD's OCI source
// consumes them directly, so the Stage just repoints targetRevision.  The
// kustomize-build promotion step would only be needed if the Stage assembled
// manifests in-cluster, which this pipeline deliberately does not.

// PROJECT is the Kargo Project namespace (kargo-project-echo).  The Warehouse
// and Stage are namespaced into it.  Unifying with #RegisteredNamespace makes a
// rename or removal of the namespace a render failure here.
let PROJECT = "kargo-echo" & #RegisteredNamespace

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).  The
// target Argo CD Application lives here, and Kargo's controller is configured
// (components/kargo: controller.argocd.namespace) to find Applications in it for
// the argocd-update promotion step.  Unifying with #RegisteredNamespace ties the
// literal to the registry.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// APP is the Argo CD Application name and the Kargo Stage name's anchor.  echo
// is the sample workload (components/echo).
let APP = "echo"

// STAGE is the single Stage in this spike pipeline.  It MUST match the
// promotionPolicy stage in kargo-project-echo so auto-promotion is enabled.
let STAGE = "test"

// WAREHOUSE is the Warehouse name; the Stage requests Freight originating from
// it.
let WAREHOUSE = "manifests"

// MANIFESTS_REPO is the rendered-manifests OCI repository scripts/publish pushes
// to (its default PUBLISH_REPO — holos/docs/oci-publish-workflow.md).  The
// Warehouse image subscription takes the BARE registry/repo form (no oci://, no
// tag); the Argo CD Application OCI source takes the oci:// form.  Kargo's
// argocd-update matches the Application source by EXACT repoURL string, so the
// two forms below must stay consistent with each other and with the publish
// workflow's default.
let MANIFESTS_REPO = "quay.holos.localhost/holos/holos-paas-manifests"
let MANIFESTS_REPO_OCI = "oci://\(MANIFESTS_REPO)"

// MANIFESTS_TAG_REGEX matches the input-addressed tags scripts/publish mints:
// render-<config-digest-12>-<appimage-digest-12> (holos/docs/oci-publish-workflow.md).
// It scopes the Warehouse to only the rendered-manifests artifacts, ignoring any
// other tag that might land in the repo.
let MANIFESTS_TAG_REGEX = "^render-[0-9a-f]{12}-[0-9a-f]{12}$"

// WAREHOUSE_RESOURCE subscribes to the rendered-manifests OCI artifact.
//
// imageSelectionStrategy: Lexical — NOT SemVer (the render-* tags are not
// semver) and NOT NewestBuild.  NewestBuild orders by an image's build
// timestamp, which it reads from the OCI config blob / manifest annotations;
// ORAS-pushed rendered-manifests artifacts carry no org.opencontainers.image.created
// annotation and an arbitrary config blob, so NewestBuild has nothing reliable
// to order by.  Lexical instead sorts the matching tag strings descending and
// only needs each tag's digest, which every artifact has — it is the robust
// strategy for these ORAS artifacts (Kargo 1.10 pkg/image research).
//
// Spike caveat: the render-<config12>-<appimage12> tag is input-addressed, not
// monotonic, so "lexically greatest tag" is NOT necessarily "most recently
// published".  For the single-app spike — where each publish carries a distinct
// input and Freight is created for any newly discovered tag — this is
// acceptable: freightCreationPolicy: Automatic creates Freight for newly
// discovered artifacts regardless of ordering, and the verification publishes
// one new artifact at a time.  A production pipeline that needs strict
// most-recent-wins ordering should switch to a monotonic tag (e.g. a
// zero-padded counter or timestamp prefix) or a Digest strategy against a
// mutable tag; tracked as future work in the component docs.
//
// insecureSkipTLSVerify: true — the in-cluster Quay serves *.holos.localhost
// with a mkcert-signed certificate that is not in the Kargo controller's trust
// store, the same reason the Argo CD repository Secret sets insecure: "true"
// (holos/docs/argocd-application-source.md).  freightCreationPolicy: Automatic
// and a 1m interval make a newly published artifact produce Freight promptly
// for the spike's verification (the chart default interval is 5m).
let WAREHOUSE_RESOURCE = kargowarehouse.#Warehouse & {
	metadata: {
		name:      WAREHOUSE
		namespace: PROJECT
		labels: "app.kubernetes.io/name": APP
	}
	spec: {
		freightCreationPolicy: "Automatic"
		interval:              "1m"
		subscriptions: [{
			image: {
				repoURL:                MANIFESTS_REPO
				imageSelectionStrategy: "Lexical"
				allowTags:              MANIFESTS_TAG_REGEX
				insecureSkipTLSVerify:  true
				discoveryLimit:         20
			}
		}]
	}
}

// STAGE_RESOURCE requests Freight directly from the Warehouse and, on
// promotion, runs a single argocd-update step that repoints the echo Argo CD
// Application's OCI source at the Freight's artifact digest.
//
// desiredRevision uses the imageFrom(...).Digest expression: imageFrom takes the
// BARE subscription repoURL (matching the Warehouse subscription) and returns
// the discovered artifact's sha256 digest, which is the immutable revision Argo
// CD's OCI source should sync (ADR-8's digest-pinning preference).  .Digest, not
// .Tag — the tag (render-...) is the human/Warehouse-facing handle; the digest
// is the source of truth downstream (holos/docs/oci-publish-workflow.md).
//
// sources[].repoURL MUST be the oci:// form and match the Application source
// repoURL character-for-character — Kargo's argocd-update selects the source to
// update by exact repoURL string match.  updateTargetRevision: true writes
// desiredRevision into spec.source.targetRevision.
let STAGE_RESOURCE = kargostage.#Stage & {
	metadata: {
		name:      STAGE
		namespace: PROJECT
		labels: "app.kubernetes.io/name": APP
	}
	spec: {
		requestedFreight: [{
			origin: {
				kind: "Warehouse"
				name: WAREHOUSE
			}
			sources: direct: true
		}]
		promotionTemplate: spec: steps: [{
			uses: "argocd-update"
			config: {
				apps: [{
					name:      APP
					namespace: ArgoCDNamespace
					sources: [{
						repoURL:              MANIFESTS_REPO_OCI
						desiredRevision:      "${{ imageFrom(\"\(MANIFESTS_REPO)\").Digest }}"
						updateTargetRevision: true
					}]
				}]
			}
		}]
	}
}

// APP_RESOURCE is the target Argo CD Application Kargo patches.  It is authored
// STANDALONE here rather than through the userDefinedBuildPlan gitops projection
// (the argoAppDisabled flip in components/user-defined-build-plan.cue) for a
// deliberate reason: that projection emits an Application with a GIT source
// (repoURL https://github.com/holos-run/holos-paas, targetRevision main) for the
// deferred whole-platform gitops delivery (holos/docs/placeholders.md).  Kargo's
// pipeline needs the opposite — an OCI source whose targetRevision is the
// rendered-manifests artifact digest (holos/docs/argocd-application-source.md) —
// so the projection's git Application is the wrong shape to patch.  This
// component therefore authors the OCI-source Application directly and leaves the
// platform-wide argoAppDisabled default untouched.
//
// The kargo.akuity.io/authorized-stage annotation authorizes the kargo-echo
// Project's test Stage to modify this Application; without it Kargo's
// argocd-update step refuses to touch the Application.  The value format is
// <project>:<stage>.
//
// targetRevision starts at a placeholder tag (render-bootstrap): no artifact
// exists until the first scripts/publish run, so the Application is intentionally
// Unknown/Degraded until the first promotion patches targetRevision to a real
// digest.  This is the same "imperative artifact, declarative Application"
// posture argocd-application-source.md documents; for the spike the Application
// is committed so Kargo has a stable target to patch.  The repository credential
// Secret the repo-server uses to PULL the artifact is created imperatively
// (scripts/quay-init robot account; see argocd-application-source.md) and is NOT
// committed.
let APP_RESOURCE = application.#Application & {
	metadata: {
		name:      APP
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": APP
		annotations: "kargo.akuity.io/authorized-stage": "\(PROJECT):\(STAGE)"
	}
	spec: {
		project: "default"
		source: {
			repoURL:        MANIFESTS_REPO_OCI
			targetRevision: "render-bootstrap"
			// The manifests sit at the tarball root (scripts/publish packages
			// the kustomize output flat), so the source path is ".".
			path: "."
		}
		destination: {
			server:    "https://kubernetes.default.svc"
			namespace: APP
		}
		syncPolicy: {
			automated: {
				prune:    true
				selfHeal: true
			}
			// The echo namespace is registered centrally and applied by the
			// namespaces component, so Argo CD must not try to create it.
			syncOptions: ["CreateNamespace=false"]
		}
	}
}

userDefinedBuildPlan: {
	metadata: name: "kargo-echo"
	spec: artifacts: manifests: {
		// One resource per artifact, CUE-natively (component guidelines): each
		// Resources generator's output IS its artifact file, so each file holds
		// exactly one resource with no bundle to slice.  Three separate
		// artifacts keep the Warehouse, Stage, and Application in their own files
		// for clean diffs.
		"clusters/\(clusterName)/components/\(metadata.name)/warehouse-\(WAREHOUSE).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: Warehouse: (WAREHOUSE): WAREHOUSE_RESOURCE
			}]
		}
		"clusters/\(clusterName)/components/\(metadata.name)/stage-\(STAGE).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: Stage: (STAGE): STAGE_RESOURCE
			}]
		}
		"clusters/\(clusterName)/components/\(metadata.name)/application-\(APP).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: Application: (APP): APP_RESOURCE
			}]
		}
	}
}

package holos

import (
	"encoding/base64"

	kargowarehouse "kargo.akuity.io/warehouse/v1alpha1"
	kargostage "kargo.akuity.io/stage/v1alpha1"
)

// my-project is the Layer 3 sample application's delivery scaffold (HOL-1268).
// The foundation phase (HOL-1269) emitted an Argo CD AppProject and an Argo CD
// Application that reconciles the rendered my-project-config manifests from an
// OCI artifact in the in-cluster Quay registry.  This phase (HOL-1270) adds the
// Kargo control plane that drives that Application's targetRevision: a Kargo
// Project (the namespace boundary), a ProjectConfig (auto-promotion policy plus
// a native Quay webhook receiver and its receiver Secret), a Warehouse that
// watches the my-project-config OCI artifact, and the project-config promotion
// Stage whose argocd-update step patches the Application's source to each
// discovered Freight digest.
//
// Unlike the echo pipeline (components/kargo-echo + kargo-project-echo, which
// split the Project from its Warehouse/Stage across two components), my-project
// keeps the whole pipeline in ONE component because its Kargo Project namespace
// IS the workload namespace — there is no separate kargo-project-* sibling.  The
// my-project namespace carries the kargo.akuity.io/project adoption label and
// keep-namespace annotation (holos/namespaces.cue) so Kargo adopts it.
//
// The delivery loop mirrors the echo pipeline (components/kargo-echo) but with
// the Project namespace doubling as the workload namespace (see the my-project
// entry in holos/namespaces.cue):
//
//   scripts/publish → new my-project-config OCI artifact in Quay
//     → Warehouse discovers it (webhook from Quay, or the polling interval
//       fallback) → creates Freight
//       → auto-promotion runs the project-config Stage's argocd-update step
//         → patches this Application's spec.source.targetRevision = <new digest>
//           → Argo CD syncs the new rendered manifests into the my-project
//             namespace.
//
// The OCI artifact does not exist yet; the Application reports Unknown/Missing
// until scripts/publish pushes the first my-project-config artifact.  That is
// expected scaffolding for this phase.

// NAMESPACE is the my-project Kargo Project + workload namespace.  Unifying
// with #RegisteredNamespace (holos/namespaces.cue) turns silent drift between
// this literal and the registry entry into a render failure: if "my-project"
// is ever removed or renamed in the registry, rendering fails here instead of
// at apply time with a NotFound namespace error.
let NAMESPACE = "my-project" & #RegisteredNamespace

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd).
// Both the AppProject and the Application are namespaced into it — Argo CD's
// project and application objects live alongside the controller.  Unifying with
// #RegisteredNamespace ties the literal to the registry.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// NAME is the shared name of the AppProject and the Application, and the anchor
// of the Kargo Project added in HOL-1270.
let NAME = "my-project"

// STAGE is the Kargo Stage (added in HOL-1270) authorized to patch this
// Application's targetRevision.  The kargo.akuity.io/authorized-stage
// annotation below references it as "<project>:<stage>"; without that
// annotation Kargo's argocd-update step refuses to touch the Application, so
// the value must match the Stage name the next phase creates.
let STAGE = "project-config"

// CONFIG_REPO is the my-project-config rendered-manifests OCI repository the
// client-side publish workflow pushes to.  The Argo CD Application OCI source
// and the Stage's argocd-update sources[].repoURL take the oci:// form
// (CONFIG_REPO_OCI); the Warehouse image subscription takes the BARE
// registry/repo form (CONFIG_REPO, no oci://, no tag).  Kargo's argocd-update
// matches the Application source by EXACT repoURL string, so the oci:// form
// must stay byte-identical between APPLICATION_RESOURCE and STAGE_RESOURCE, and
// the imageFrom(...) expression in the Stage references the bare form to match
// the Warehouse subscription.  Scoped under the my-project/ Quay org path so the
// AppProject can constrain sourceRepos to oci://quay.holos.localhost/my-project/*.
let CONFIG_REPO = "quay.holos.localhost/my-project/my-project-config"
let CONFIG_REPO_OCI = "oci://\(CONFIG_REPO)"

// CONFIG_TAG_REGEX matches the input-addressed tags scripts/publish mints:
// render-<config-digest-12>-<appimage-digest-12> (holos/docs/oci-publish-workflow.md,
// scripts/publish).  It scopes the Warehouse image subscription to only the
// rendered-manifests artifacts, ignoring any other tag that might land in the
// repo.  Identical in shape to the kargo-echo Warehouse's MANIFESTS_TAG_REGEX.
let CONFIG_TAG_REGEX = "^render-[0-9a-f]{12}-[0-9a-f]{12}$"

// APPPROJECT_RESOURCE is the Argo CD AppProject that scopes what the
// my-project Application may deploy.  It is intentionally minimal but
// functional for the local single-tenant cluster:
//   - sourceRepos restricts sources to the my-project Quay org path, so the
//     Application can only sync artifacts published under my-project/.
//   - destinations restricts deploys to the in-cluster API server, my-project
//     namespace — matching the Application's destination below.
//   - namespaceResourceWhitelist is permissive (group "*", kind "*") so the
//     rendered my-project-config manifests can carry any NAMESPACED kind into
//     the my-project namespace.
//   - clusterResourceWhitelist is DELIBERATELY OMITTED (empty), which in Argo
//     CD forbids the Application from deploying any cluster-scoped resource
//     (CRDs, ClusterRoles, Namespaces, …).  my-project is a project-scoped
//     sample app whose config artifact deploys only namespaced workloads into
//     its own namespace; the platform owns cluster-scoped objects (CRDs,
//     namespaces) through dedicated components, so the Application has no need
//     to create them and the empty cluster whitelist keeps a stray
//     cluster-scoped manifest in the artifact from escaping the namespace
//     boundary.  Add specific {group, kind} entries here if a future
//     my-project artifact legitimately needs a cluster-scoped resource.
let APPPROJECT_RESOURCE = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "AppProject"
	metadata: {
		name:      NAME
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		sourceRepos: ["oci://quay.holos.localhost/my-project/*"]
		destinations: [{
			server:    "https://kubernetes.default.svc"
			namespace: NAMESPACE
		}]
		namespaceResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
	}
}

// APPLICATION_RESOURCE is the Argo CD Application Kargo patches (HOL-1270).  It
// is authored standalone here (an OCI-source Application) rather than through
// the userDefinedBuildPlan gitops projection, the same deliberate choice the
// echo pipeline makes (see components/kargo-echo/buildplan.cue): that
// projection emits a GIT-source Application, which is the wrong shape for
// Kargo's OCI argocd-update step to patch.
//
// The kargo.akuity.io/authorized-stage annotation authorizes the my-project
// Project's project-config Stage to modify this Application; without it Kargo's
// argocd-update step refuses to touch it.  The value format is <project>:<stage>.
//
// targetRevision is DELIBERATELY OMITTED from this committed manifest — the
// same posture the echo pipeline takes (components/kargo-echo/buildplan.cue) and
// the reason holos/docs/argocd-application-source.md gives.  Kargo's
// argocd-update step (added in HOL-1270) OWNS spec.source.targetRevision: it
// patches it to each promoted Freight's digest.  scripts/apply re-applies every
// component with `kubectl apply --server-side --force-conflicts`, which would
// seize a committed targetRevision back to its literal value on every run — a
// reconciliation war that would repeatedly revert the Application after Kargo
// promotes.  By leaving the field out of the desired state entirely, apply never
// asserts ownership of it: the Application is created with no targetRevision
// (Argo CD reports it Unknown until the first promotion), and from the first
// promotion onward Kargo is the sole owner of the field, untouched by later
// applies.  This is the "imperative revision, declarative Application" posture —
// the Application shell is committed so Kargo has a stable target to patch, while
// the revision itself is controller-owned.
let APPLICATION_RESOURCE = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "Application"
	metadata: {
		name:      NAME
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name":                NAME
		annotations: "kargo.akuity.io/authorized-stage": "\(NAMESPACE):\(STAGE)"
	}
	spec: {
		project: NAME
		source: {
			repoURL: CONFIG_REPO_OCI
			// targetRevision is omitted — Kargo owns it (see the comment above).
			// The manifests sit at the tarball root (scripts/publish packages
			// the kustomize output flat), so the source path is ".".
			path: "."
		}
		destination: {
			server:    "https://kubernetes.default.svc"
			namespace: NAMESPACE
		}
		syncPolicy: {
			automated: {
				prune:    true
				selfHeal: true
			}
			// The my-project namespace is registered centrally and applied by
			// the namespaces component, so Argo CD must not try to create it.
			syncOptions: ["CreateNamespace=false"]
		}
	}
}

// --- Quay data plane (HOL-1322) ---------------------------------------------

// ORGANIZATION_RESOURCE is the quay.holos.run/v1alpha1 Organization the shipped
// Holos Controller (ADR-18/ADR-19) reconciles into the in-cluster Quay registry:
// it creates (does not adopt — spec.adopt: false) the my-project org so the
// my-project-config repo the delivery loop publishes to has a home.  The
// controller authenticates with the superuser OAuth credential in the
// holos-controller-quay-creds Secret (spec.credentialsSecretRef.name, the
// resolver's default; named explicitly here to match the HOL-1319 worked
// example) resolved from the controller's own namespace.
//
// spec.caBundle carries the per-cluster local-ca certificate so the controller
// trusts Quay's mkcert-signed serving certificate (HOL-1319/HOL-1320): the
// in-cluster registry serves *.holos.localhost with a cert not in the
// controller pod's system trust store, so without this anchor the reconciler
// fails Quay TLS verification with x509: certificate signed by unknown
// authority.  The PEM arrives via the _CABundlePEM CUE tag (holos/tags.cue),
// injected at apply time by scripts/apply-my-project; the field is base64-encoded
// with encoding/base64 to satisfy the caBundle []byte/base64 serialization
// (api/quay/v1alpha1).
//
// The field is GATED on a non-empty tag: when _CABundlePEM is "" (the default,
// e.g. during `holos render platform` and scripts/render's clean-tree gate)
// spec.caBundle is omitted entirely, so the committed holos/deploy/ tree carries
// no per-cluster CA material (the runtime-secret posture — per-cluster trust is
// injected at apply time, never committed).
//
// spec.syncedTeams declares the OIDC-synced Quay teams the shipped controller
// reconciles for this org (HOL-1325, ADR-19 Revision 6; the API is
// api/quay/v1alpha1 SyncedTeam, the reconciler internal/controller/quay/teams.go).
// The set here is the worked GCP-style primitive-role example from ADR-19: a
// logical project's owner/editor/viewer OIDC groups map to three synced teams.
// OIDC groups are referenced BY NAME ONLY in this Quay CR (the oidcGroup string)
// — the Quay API group imports no IdP type.  The my-project-{owner,editor,viewer}
// values are now PRODUCED by the keycloak.holos.run CRs this component emits below
// (HOL-1348): the role/custodian KeycloakGroups, the owner KeycloakUser, and the
// project KeycloakClient, reconciled by the shipped Holos Controller (ADR-20,
// Partially Implemented).  As of HOL-1350 (ADR-20 Rev 4) the role groups confer
// the my-project-<role> client role on the platform Quay client directly (the
// "Keycloak data plane" note below), so those values surface in the platform Quay
// client's groups claim via the already-deployed quay-client-roles mapper and the
// synced teams' membership populates from the project role groups — the Quay-claim
// gap is closed.
//
// The two enums are distinct (ADR-19 "Two distinct Quay concepts"): `role` is the
// team's ORG role (admin | creator | member), `repositoryPermission` is an
// optional org DEFAULT REPOSITORY PERMISSION (a Quay prototype: read | write |
// admin) applied to repos created in the org. owner→admin needs no default
// permission (org admin reaches all repos); editor→member+write and
// viewer→member+read get their data-plane reach from the default permission.
// Authored as a plain CUE list of structs (the Organization is a structured
// Kubernetes object, so no marshalled YAML/JSON blob — the "marshal it" guardrail
// is naturally satisfied).
let ORGANIZATION_RESOURCE = {
	apiVersion: "quay.holos.run/v1alpha1"
	kind:       "Organization"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		name:  NAME
		email: "\(NAME)@holos.localhost"
		credentialsSecretRef: name: "holos-controller-quay-creds"
		adopt: false
		syncedTeams: [
			{
				name:      "my-project-owner"
				oidcGroup: "my-project-owner"
				role:      "admin"
			},
			{
				name:                 "my-project-editor"
				oidcGroup:            "my-project-editor"
				role:                 "creator"
				repositoryPermission: "write"
			},
			{
				name:                 "my-project-viewer"
				oidcGroup:            "my-project-viewer"
				role:                 "member"
				repositoryPermission: "read"
			},
		]
		// Only set caBundle when a PEM was injected (see the gate note above).
		// base64.Encode(null, _CABundlePEM) base64-encodes the PEM bytes with no
		// line breaks, the single-string form the caBundle []byte field expects.
		if _CABundlePEM != "" {
			caBundle: base64.Encode(null, _CABundlePEM)
		}
	}
}

// --- Kargo control plane (HOL-1270) -----------------------------------------

// WAREHOUSE is the Warehouse name; the Stage requests Freight originating from
// it.  Named after the project for a single-Warehouse pipeline.
let WAREHOUSE = NAME

// WEBHOOK_SECRET is the Secret the Quay webhook receiver references
// (ProjectConfig.spec.webhookReceivers[].quay.secretRef.name).  Its token is
// generated once by the bootstrap Job below and never committed.
let WEBHOOK_SECRET = "my-project-quay-webhook"

// WEBHOOK_BOOTSTRAP names the create-if-absent Job (and its ServiceAccount,
// Role, RoleBinding) that generates the webhook receiver Secret's token once.
let WEBHOOK_BOOTSTRAP = "\(WEBHOOK_SECRET)-bootstrap"

// KUBECTL_IMAGE pins the image the bootstrap Job runs kubectl from — the same
// alpine-based image and rationale as the quay secret-keys bootstrap
// (components/quay/buildplan.cue): a manifest list including linux/arm64,
// alpine-based so it provides the /bin/sh the script needs, and exercising only
// version-stable core/v1 Secret get/create.
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

// --- Quay-side provisioning is DEFERRED (HOL-1293) ---------------------------
//
// The Quay-side data plane this delivery scaffold needs — the my-project org,
// the my-project-config repo, an Argo CD pull robot, the repository pull-credential
// Secret, and the repo_push webhook registration — was previously provisioned by
// an in-component bootstrap Job (my-project-quay-bootstrap, HOL-1272) that
// authenticated with the quay-initial-admin superuser OAuth token.  That token no
// longer exists: HOL-1295 switched Quay to AUTHENTICATION_TYPE: OIDC, which
// disables the local admin user and the /api/v1/user/initialize bootstrap that
// minted it.  The Job, its RBAC, and the quay-init/quay-reset scripts that
// depended on the token are removed in this phase (HOL-1296).
//
// Quay org/repo/webhook/robot provisioning is intentionally deferred to a future
// Quay Resource Controller (HOL-1293), which will reconcile these objects against
// an OIDC-backed service account rather than the retired admin token.  Until then
// the my-project Application reports Unknown/Missing and the Warehouse cannot pull
// the (not-yet-provisioned) private repo — expected scaffolding for this phase.
//
// The Kargo-side webhook RECEIVER stays in place below (the ProjectConfig
// webhookReceivers block and its WEBHOOK_SECRET token Job): it does not depend on
// the Quay admin token.  Only the Quay-side webhook REGISTRATION moves to the
// future controller.

// PROJECT_RESOURCE is the Kargo Project.  In Kargo 1.10 the Project is
// CLUSTER-SCOPED with NO spec (only metadata + status): the controller maps the
// Project NAME to a same-named namespace and adopts it.  The promotion policy
// moved off Project onto the namespaced ProjectConfig below.  Authored as a
// plain CUE struct (not the vendored #Project binding) because that binding is
// stale for 1.10.3 — it carries a required spec! the cluster-scoped CRD rejects
// (see holos/resources.cue and components/kargo-project-echo for the same
// reasoning).  No metadata.namespace — the resource is cluster-scoped, and the
// my-project namespace it adopts carries the kargo.akuity.io/project adoption
// label + keep-namespace annotation in holos/namespaces.cue.
let PROJECT_RESOURCE = {
	apiVersion: "kargo.akuity.io/v1alpha1"
	kind:       "Project"
	metadata: {
		name: NAME
		labels: "app.kubernetes.io/name": NAME
	}
}

// PROJECT_CONFIG_RESOURCE is the namespaced ProjectConfig (Kargo 1.10).  It
// carries the auto-promotion policy and the native Quay webhook receiver.
// Authored as a plain CUE struct: ProjectConfig has no generated CUE type under
// cue.mod/gen at all (noted in holos/resources.cue), so it stays on the
// #Resources catch-all rather than a typed binding.  Field locations validated
// against the vendored CRD
// (components/kargo/vendor/1.10.3/kargo/resources/crds/kargo.akuity.io_projectconfigs.yaml):
// spec.promotionPolicies and spec.webhookReceivers[].quay.secretRef both live on
// ProjectConfig in 1.10.3, and quay is a supported receiver type.
//
//   - promotionPolicies: autoPromotionEnabled lets newly discovered Freight flow
//     into the project-config Stage without a manual promotion (the spike's
//     publish→Freight→promotion→sync loop must close on its own).  The stage name
//     MUST match STAGE_RESOURCE below.
//   - webhookReceivers: a single quay receiver whose secretRef points at the
//     WEBHOOK_SECRET Secret in this same namespace (the CRD requires
//     Project-scoped receiver Secrets to be co-namespaced with the ProjectConfig).
//     Once reconciled, Kargo populates status.webhookReceivers[].url — the
//     hard-to-guess URL the future Quay Resource Controller will register with
//     Quay (the Quay-side webhook registration is deferred — see the deferral
//     note below).
let PROJECT_CONFIG_RESOURCE = {
	apiVersion: "kargo.akuity.io/v1alpha1"
	kind:       "ProjectConfig"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		// The project-config Stage plus one auto-promotion policy per contained
		// app's <app>-config Stage (the Application component, HOL-1356): without a
		// matching promotionPolicies entry a Stage's discovered Freight never
		// auto-promotes, so app push-to-deploy would not close.  Empty app set →
		// just the project Stage.  HOL-1357 folds this into the generic project
		// component.
		promotionPolicies: [
			{
				stage:                STAGE
				autoPromotionEnabled: true
			},
			for APP, A in apps if A.project == NAME {
				{
					stage:                "\(APP)-config"
					autoPromotionEnabled: true
				}
			},
		]
		webhookReceivers: [{
			name: "quay"
			quay: secretRef: name: WEBHOOK_SECRET
		}]
	}
}

// The Quay webhook receiver Secret is generated at runtime, never committed (the
// repo's runtime-secret posture; see AGENTS.md "OIDC Client Secrets" and the
// quay secret-keys precedent).  A small create-if-absent Job generates the token
// once and leaves an existing Secret untouched, so the value stays stable across
// re-applies (Kargo derives the hard-to-guess receiver URL from it — a rotation
// would silently invalidate the URL Quay was registered with).
//
// Key naming: the Kargo 1.10.3 ProjectConfig CRD documents that a QUAY receiver
// Secret's data map is read from the `secret` key (verified against the vendored
// CRD; other receiver types read a different key).  The Job writes the generated
// token under that one functional `secret` key and nothing else — see
// holos/docs/secret-handling.md for why we never carry extra unread keys "for AC
// compliance".  The token is piped as a manifest on stdin so it never appears in
// the container's argv.
let WEBHOOK_BOOTSTRAP_SCRIPT = """
	set -eu
	if kubectl -n \(NAMESPACE) get secret \(WEBHOOK_SECRET) >/dev/null 2>&1; then
	  echo "Secret \(WEBHOOK_SECRET) already exists; leaving its generated token untouched."
	  # One-time migration (HOL-1274): older revisions of this Job also wrote a
	  # duplicate, unread `secret-token` key.  Prune it if present, without
	  # touching the functional `secret` value, so clusters that ran an older
	  # revision converge on the single key the Kargo quay receiver reads.
	  if kubectl -n \(NAMESPACE) get secret \(WEBHOOK_SECRET) -o 'jsonpath={.data.secret-token}' | grep -q .; then
	    kubectl -n \(NAMESPACE) patch secret \(WEBHOOK_SECRET) --type=json -p='[{"op": "remove", "path": "/data/secret-token"}]'
	    echo "Removed legacy secret-token key from \(WEBHOOK_SECRET)."
	  fi
	  exit 0
	fi
	random_key() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	TOKEN="$(random_key)"
	[ "${#TOKEN}" -eq 48 ]
	kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(WEBHOOK_SECRET)
	  namespace: \(NAMESPACE)
	stringData:
	  secret: "${TOKEN}"
	EOF
	echo "Secret \(WEBHOOK_SECRET) created."
	"""

let WEBHOOK_BOOTSTRAP_METADATA = {
	name:      WEBHOOK_BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": WEBHOOK_BOOTSTRAP
}

let WEBHOOK_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   WEBHOOK_BOOTSTRAP_METADATA
}

// Scoped to the one Secret the Job manages: get and patch are restricted to the
// WEBHOOK_SECRET resourceName (the API server evaluates resourceNames for both);
// patch is needed only for the one-time legacy-key migration in the script.
// create cannot be restricted by resourceName (the API server does not evaluate
// resourceNames for create), so the create grant is namespace-wide on secrets —
// acceptable in a namespace whose Secrets all belong to this project (the quay
// secret-keys bootstrap Role precedent).
let WEBHOOK_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   WEBHOOK_BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get", "patch"]
			resourceNames: [WEBHOOK_SECRET]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let WEBHOOK_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   WEBHOOK_BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     WEBHOOK_BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      WEBHOOK_BOOTSTRAP
		namespace: NAMESPACE
	}]
}

// CAVEAT (same as the quay secret-keys bootstrap): a completed Job's pod
// template is immutable, so scripts/apply cannot re-run it by re-applying the
// unchanged spec.  pre_my_project() in scripts/apply deletes any prior Job
// before each apply so a fresh idempotent Job runs every time; the Secret it
// created survives the deletion, so the generate-once guarantee holds.
// ttlSecondsAfterFinished garbage-collects the Job a day after completion.
let WEBHOOK_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   WEBHOOK_BOOTSTRAP_METADATA
	spec: {
		backoffLimit:            3
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: WEBHOOK_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: WEBHOOK_BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// The alpine/kubectl image declares no non-root USER; pick
					// the conventional "nobody" uid.
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "bootstrap"
					image: KUBECTL_IMAGE
					command: ["/bin/sh", "-c", WEBHOOK_BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME; point it
					// at the writable emptyDir since the root filesystem is
					// read-only.
					env: [{
						name:  "HOME"
						value: "/tmp"
					}]
					resources: {
						requests: {
							cpu:    "10m"
							memory: "32Mi"
						}
						limits: memory: "64Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					volumeMounts: [{
						name:      "tmp"
						mountPath: "/tmp"
					}]
				}]
				volumes: [{
					name: "tmp"
					emptyDir: {}
				}]
			}
		}
	}
}

// WAREHOUSE_RESOURCE subscribes to the my-project-config rendered-manifests OCI
// artifact via an IMAGE subscription against the bare registry/repo form (no
// oci://, no tag) — ADR-16's resolution for discovering a plain-manifest OCI
// artifact.
//
// imageSelectionStrategy: Lexical — matching the kargo-echo Warehouse and for the
// same reason.  scripts/publish tags the my-project-config artifact
// input-addressed as render-<config12>-<appimage12> (CONFIG_TAG_REGEX above), NOT
// a mutable tag like latest, so the Digest strategy (which tracks the digest of a
// single named/mutable tag) would discover nothing here.  The render-* tags are
// not semver, ruling out SemVer; and NewestBuild orders by an image's build
// timestamp read from the OCI config blob / created annotation, which ORAS-pushed
// artifacts do not carry — so Lexical, which sorts the matching tag strings
// descending and needs only each tag's digest, is the robust strategy for these
// artifacts.  allowTags scopes discovery to the render-* artifacts.
//
// Spike caveat (inherited from kargo-echo): the render-<config12>-<appimage12>
// tag is input-addressed, not monotonic, so "lexically greatest tag" is not
// strictly "most recently published".  For this single-app pipeline it is
// acceptable — freightCreationPolicy: Automatic creates Freight for any newly
// discovered tag regardless of ordering, and live validation publishes one
// artifact at a time.  A pipeline needing strict most-recent-wins ordering would
// switch to a monotonic tag (zero-padded counter/timestamp) or a Digest strategy
// against a mutable tag; tracked as future work.  The artifact does not exist yet
// (provisioning + first publish land in later phases — HOL-1272 onward), so this
// strategy is confirmed live then.
//
// insecureSkipTLSVerify: true — the in-cluster Quay serves *.holos.localhost with
// a mkcert-signed certificate not in the Kargo controller's trust store (same
// reason the kargo-echo Warehouse and the Argo CD repository Secret skip verify).
// freightCreationPolicy: Automatic + a 1m interval make a newly published
// artifact produce Freight promptly when the webhook is unavailable; the webhook
// receiver (ProjectConfig above) is the primary, low-latency trigger and this
// interval is the polling fallback.
let WAREHOUSE_RESOURCE = kargowarehouse.#Warehouse & {
	metadata: {
		name:      WAREHOUSE
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		freightCreationPolicy: "Automatic"
		interval:              "1m"
		subscriptions: [{
			image: {
				repoURL:                CONFIG_REPO
				imageSelectionStrategy: "Lexical"
				allowTags:              CONFIG_TAG_REGEX
				insecureSkipTLSVerify:  true
				discoveryLimit:         20
			}
		}]
	}
}

// STAGE_RESOURCE requests Freight directly from the Warehouse and, on promotion,
// runs a single argocd-update step that repoints the my-project Argo CD
// Application's OCI source at the Freight's artifact digest.
//
// desiredRevision uses imageFrom(<bare repoURL>).Digest — imageFrom takes the
// BARE subscription repoURL (matching the Warehouse subscription) and returns the
// discovered artifact's sha256 digest, the immutable revision Argo CD's OCI
// source syncs (ADR-8's digest-pinning preference).  sources[].repoURL MUST be
// the oci:// form and match APPLICATION_RESOURCE's source repoURL
// character-for-character — Kargo's argocd-update selects the source to update by
// exact repoURL string match.  updateTargetRevision: true writes desiredRevision
// into spec.source.targetRevision (which APPLICATION_RESOURCE deliberately omits
// so Kargo solely owns it — see the comment on APPLICATION_RESOURCE above).
let STAGE_RESOURCE = kargostage.#Stage & {
	metadata: {
		name:      STAGE
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
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
					name:      NAME
					namespace: ArgoCDNamespace
					sources: [{
						repoURL:              CONFIG_REPO_OCI
						desiredRevision:      "${{ imageFrom(\"\(CONFIG_REPO)\").Digest }}"
						updateTargetRevision: true
					}]
				}]
			}
		}]
	}
}

// --- Keycloak data plane (HOL-1348) -----------------------------------------
//
// The project's keycloak.holos.run CRs the shipped Holos Controller
// (ADR-18/ADR-20) reconciles into the holos realm, so a project registration
// produces the my-project-{owner,editor,viewer} values the Organization's
// spec.syncedTeams[].oidcGroup above binds to Quay team roles.  All reference the
// central KeycloakInstance (the keycloak-instance component) cross-namespace,
// authorized by its security.holos.run ReferenceGrant in the keycloak namespace.
//
// Mechanism (ADR-20 "Claim value via a client role"): Keycloak's group-membership
// mapper cannot synthesize the flat value my-project-owner from the path
// projects/my-project/roles/owner (it emits the leaf "owner" or the full path).
// The value is carried as a CLIENT ROLE: each roles/<role> KeycloakGroup confers
// the client role my-project-<role> on a KeycloakClient (KeycloakGroup.clientRoles
// → AssignClientRoleToGroup), and that client's auto-ensured
// oidc-usermodel-client-role-mapper folds the role name into the shared groups
// claim (the platform's quay-client-roles precedent).  A member of roles/owner
// thereby surfaces my-project-owner in the token.
//
// The role is conferred on TWO clients (ADR-20 Rev 4, HOL-1350):
//
//   - the platform Quay client (https://quay.holos.localhost) — the ADR-20 "Quay
//     use case".  A KeycloakGroup names this reserved clientId DIRECTLY via
//     clientRoles[].clientId (not clientRef — the KeycloakClient reconciler still
//     rejects a tenant CR that targets the reserved client, reservedClientIDs in
//     internal/controller/keycloak/client_controller.go, so no CR exists for it).
//     The group reconciler ensures the project-prefixed role on the reserved
//     client and assigns it WITHOUT seizing the client object (only project-
//     prefixed roles are controller-claimed; the platform's own platform-admin/
//     project-admin roles stay reserved).  Quay's already-deployed quay-client-
//     roles mapper then folds my-project-<role> into Quay's groups claim, so the
//     Organization's syncedTeams[].oidcGroup membership populates.
//   - the project's own URL-named client (https://my-project.holos.localhost) via
//     clientRef — the ADR-20 "project's own service" path, so the role also
//     reaches the project client's own token.  The KeycloakClient reconciler
//     creates that client, its my-project-<role> roles, and its own
//     client-role→groups mapper.

// KEYCLOAK_NAMESPACE is the central KeycloakInstance's namespace; the project CRs
// reference it cross-namespace (gated by the keycloak-instance component's
// ReferenceGrant).  Unified with #RegisteredNamespace to tie the literal to the
// registry.
let KEYCLOAK_NAMESPACE = "keycloak" & #RegisteredNamespace

// KEYCLOAK_INSTANCE is the central KeycloakInstance these CRs reconcile against
// (the keycloak-instance component's NAME).
let KEYCLOAK_INSTANCE = "holos-keycloak"

// INSTANCE_REF is the shared cross-namespace reference every project Keycloak CR
// carries (ReferenceGrant-gated because namespace differs from this component's).
let INSTANCE_REF = {
	name:      KEYCLOAK_INSTANCE
	namespace: KEYCLOAK_NAMESPACE
}

// PROJECT_CLIENT_NAME is the KeycloakClient CR's metadata.name — the object name
// the role groups' clientRoles[].clientRef resolves (NOT the URL clientId; the
// reconciler derives the clientId from the CR's spec.clientId, group_controller.go
// resolveClientID).
let PROJECT_CLIENT_NAME = "my-project"

// PROJECT_CLIENT_ID is the project client's URL-shaped clientId (the platform
// convention; ADR-20).  Distinct from the reserved https://quay.holos.localhost.
let PROJECT_CLIENT_ID = "https://my-project.holos.localhost"

// QUAY_CLIENT_ID is the platform Quay client's reserved clientId (realm-config's
// QUAY_CLIENT_ID).  The role groups confer the my-project-<role> client role on it
// directly via clientRoles[].clientId (NOT clientRef — no tenant KeycloakClient CR
// exists for the reserved client; the reserved-name guard forbids one), closing
// the ADR-20 Rev 4 "Quay use case" gap: the role surfaces in Quay's groups claim
// via the platform's already-deployed quay-client-roles mapper, so the
// Organization's syncedTeams[].oidcGroup membership populates (HOL-1350).  Only the
// project-prefixed role name is controller-claimed on this client; the client
// OBJECT stays keycloak-config-cli's.
let QUAY_CLIENT_ID = "https://quay.holos.localhost"

// The primitive role triad and the flat client-role / claim value each maps to
// (my-project-<role>), consistent with the Organization syncedTeams[].oidcGroup
// example above.
let ROLES = ["owner", "editor", "viewer"]
let CLIENT_ROLE = {for r in ROLES {(r): "\(NAME)-\(r)"}}

// PROJECT_CLIENT_SECRET is where the controller delivers the confidential
// client's generated secret — a generate-once Secret in THIS namespace, created
// at runtime by the controller and never committed (the Runtime Secret Handling
// guardrail; the KeycloakClient reconciler's secret delivery).  Only the one key
// the consumer reads is written.
let PROJECT_CLIENT_SECRET = "my-project-oidc"

// KEYCLOAK_CLIENT_RESOURCE is the project's confidential OIDC client.  Its
// clientRoles declare the my-project-{owner,editor,viewer} client roles; the
// reconciler ensures those roles and the client-role→groups mapper exist on the
// client, so a role group conferring one of them surfaces it in the groups claim.
// secretRef is REQUIRED for a confidential client (the CRD's CEL rule); the
// controller delivers the generated secret there.
let KEYCLOAK_CLIENT_RESOURCE = {
	apiVersion: "keycloak.holos.run/v1alpha1"
	kind:       "KeycloakClient"
	metadata: {
		name:      PROJECT_CLIENT_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		clientId:    PROJECT_CLIENT_ID
		type:        "confidential"
		instanceRef: INSTANCE_REF
		redirectUris: ["\(PROJECT_CLIENT_ID)/oauth2/callback"]
		webOrigins: [PROJECT_CLIENT_ID]
		// The flat claim values, declared as client roles on this client.  Each
		// ClientRef is THIS KeycloakClient's own metadata.name (the object name,
		// per ClientRoleReference).
		clientRoles: [
			for r in ROLES {
				clientRef: PROJECT_CLIENT_NAME
				role:      CLIENT_ROLE[r]
			},
		]
		secretRef: {
			name: PROJECT_CLIENT_SECRET
			key:  "client_secret"
		}
	}
}

// KEYCLOAK_ROLE_GROUP_RESOURCES are the nested role groups
// projects/my-project/roles/{owner,editor,viewer}.  Each confers the matching
// my-project-<role> client role on TWO clients and delegates membership management
// to the same-tier custodian group (custodians):
//
//   - the platform Quay client (clientId: https://quay.holos.localhost), so the
//     role surfaces in Quay's groups claim via the already-deployed
//     quay-client-roles mapper — the ADR-20 Rev 4 "Quay use case" that populates
//     the Organization's syncedTeams[].oidcGroup membership (HOL-1350).  Named by
//     clientId directly because no tenant KeycloakClient CR exists for the
//     reserved client.
//   - the project's own client (clientRef: my-project), the ADR-20 "project's own
//     service" path, so the role also reaches https://my-project.holos.localhost's
//     own token.
let KEYCLOAK_ROLE_GROUP_RESOURCES = {
	for r in ROLES {
		(r): {
			apiVersion: "keycloak.holos.run/v1alpha1"
			kind:       "KeycloakGroup"
			metadata: {
				name:      "my-project-roles-\(r)"
				namespace: NAMESPACE
				labels: "app.kubernetes.io/name": NAME
			}
			spec: {
				path:        "projects/\(NAME)/roles/\(r)"
				instanceRef: INSTANCE_REF
				clientRoles: [
					{
						clientId: QUAY_CLIENT_ID
						role:     CLIENT_ROLE[r]
					},
					{
						clientRef: PROJECT_CLIENT_NAME
						role:      CLIENT_ROLE[r]
					},
					// One entry per app contained by THIS project (apps where
					// app.project == my-project), mirroring the generic project
					// component (HOL-1356): the Application component emits each app's
					// KeycloakClient (CR named for the app) into this same bare
					// my-project namespace and defines its owner/editor/viewer client
					// roles, so the role group confers the matching role on the app
					// client via clientRef (same-namespace resolution).  Without this
					// a my-project app's KeycloakClient would exist but its roles would
					// never be conferred (the codex round-2 finding).  Empty when
					// my-project has no apps.  HOL-1357 folds this whole component into
					// the generic project component, at which point this duplication
					// disappears.
					for APP, A in apps if A.project == NAME {
						{
							clientRef: APP
							role:      r
						}
					},
				]
				custodians: [{
					path: "projects/\(NAME)/custodians/\(r)"
				}]
			}
		}
	}
}

// KEYCLOAK_CUSTODIAN_GROUP_RESOURCES are the nested custodian groups
// projects/my-project/custodians/{owner,editor,viewer} the role groups delegate
// membership management to (ADR-3's custodian model).  They confer no client role
// and have no further custodian.
let KEYCLOAK_CUSTODIAN_GROUP_RESOURCES = {
	for r in ROLES {
		(r): {
			apiVersion: "keycloak.holos.run/v1alpha1"
			kind:       "KeycloakGroup"
			metadata: {
				name:      "my-project-custodians-\(r)"
				namespace: NAMESPACE
				labels: "app.kubernetes.io/name": NAME
			}
			spec: {
				path:        "projects/\(NAME)/custodians/\(r)"
				instanceRef: INSTANCE_REF
			}
		}
	}
}

// KEYCLOAK_USER_RESOURCE pre-provisions the project owner bob@example.com into
// the owner role group, with the IdP federated-identity link so first federated
// login auto-links this record (paired with the realm's first-broker-login
// auto-link flow the realm-config component configures, HOL-1348).  The IdP alias
// "holos" is the realm's broker alias; userId is omitted so the link is keyed by
// the trusted email (the auto-link flow's email match).
let KEYCLOAK_USER_RESOURCE = {
	apiVersion: "keycloak.holos.run/v1alpha1"
	kind:       "KeycloakUser"
	metadata: {
		name:      "bob"
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		email:       "bob@example.com"
		instanceRef: INSTANCE_REF
		groups: ["projects/\(NAME)/roles/owner"]
		identityProviderLink: alias: "holos"
	}
}

userDefinedBuildPlan: {
	metadata: name: "my-project"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored AppProject, Application, Warehouse, and Stage
				// validate against the vendored schemas at render time.  Project
				// and ProjectConfig ride the #Resources catch-all (no typed
				// binding — see holos/resources.cue): the 1.10.3 Project CRD is
				// cluster-scoped with no spec and ProjectConfig has no generated
				// CUE type at all.
				resources: #Resources & {
					AppProject: (NAME):     APPPROJECT_RESOURCE
					Application: (NAME):    APPLICATION_RESOURCE
					Organization: (NAME):   ORGANIZATION_RESOURCE
					Project: (NAME):        PROJECT_RESOURCE
					ProjectConfig: (NAME):  PROJECT_CONFIG_RESOURCE
					Warehouse: (WAREHOUSE): WAREHOUSE_RESOURCE
					Stage: (STAGE):         STAGE_RESOURCE

					// HOL-1348: the project's keycloak.holos.run CRs (reconciled by
					// the shipped Holos Controller).  The project client, the nested
					// role + custodian KeycloakGroups, and the owner KeycloakUser.
					// They ride the open, Kind-scoped #Resources entries (no vendored
					// CUE binding — see holos/resources.cue).  The my-project-oidc
					// client Secret is NOT rendered here: the KeycloakClient
					// reconciler delivers it at runtime (generate-once, never
					// committed — the Runtime Secret Handling guardrail).
					KeycloakClient: (PROJECT_CLIENT_NAME): KEYCLOAK_CLIENT_RESOURCE
					KeycloakUser: (KEYCLOAK_USER_RESOURCE.metadata.name): KEYCLOAK_USER_RESOURCE
					KeycloakGroup: {
						for r in ROLES {
							(KEYCLOAK_ROLE_GROUP_RESOURCES[r].metadata.name):      KEYCLOAK_ROLE_GROUP_RESOURCES[r]
							(KEYCLOAK_CUSTODIAN_GROUP_RESOURCES[r].metadata.name): KEYCLOAK_CUSTODIAN_GROUP_RESOURCES[r]
						}
					}
					// The my-project-quay-webhook Secret is DELIBERATELY NOT
					// rendered here.  The bootstrap Job below is its sole creator
					// (the quay secret-keys precedent): committing an empty-data
					// Secret would let scripts/apply create it BEFORE the Job runs,
					// and the Job's create-if-absent guard would then see it
					// already exists and skip token generation — leaving the
					// receiver Secret permanently empty and the webhook unusable.
					// Keeping the Secret entirely Job-owned makes the
					// generate-once token the single source of truth.
					ServiceAccount: (WEBHOOK_BOOTSTRAP): WEBHOOK_BOOTSTRAP_SERVICE_ACCOUNT
					Role: (WEBHOOK_BOOTSTRAP):           WEBHOOK_BOOTSTRAP_ROLE
					RoleBinding: (WEBHOOK_BOOTSTRAP):    WEBHOOK_BOOTSTRAP_ROLE_BINDING
					Job: (WEBHOOK_BOOTSTRAP):            WEBHOOK_BOOTSTRAP_JOB

					// The Quay ORG is now reconciled by the shipped Holos
					// Controller (ADR-18/ADR-19) from the ORGANIZATION_RESOURCE
					// emitted above (HOL-1322).  The remaining Quay-side data
					// plane — the my-project-config repo, the Argo CD pull robot,
					// the repository pull-credential Secret, and the repo_push
					// webhook registration — is NOT modeled by the v1alpha1 CRDs
					// (ADR-19 out of scope) and stays manual for now.  The
					// my-project-quay-bootstrap Job and its RBAC that previously
					// emitted those resources were removed in HOL-1296 along with
					// the retired quay-initial-admin admin token they depended on.
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

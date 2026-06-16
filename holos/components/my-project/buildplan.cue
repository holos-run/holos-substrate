package holos

import (
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
		labels: "app.kubernetes.io/name": NAME
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

// --- Quay provisioning bootstrap (HOL-1272) ---------------------------------

// QUAY_NAMESPACE is the namespace the Quay-provisioning Job and its
// ServiceAccount render into.  The Job runs THERE — not in my-project — because
// the credentials it needs live there: the admin OAuth token
// (quay-initial-admin Secret, key `token`) and the local-CA trust cert
// (quay-local-ca Secret, key `ca.crt`).  Unifying with #RegisteredNamespace ties
// the literal to the registry (holos/namespaces.cue).
let QUAY_NAMESPACE = "quay" & #RegisteredNamespace

// QUAY_BOOTSTRAP names the Quay-provisioning Job and its ServiceAccount/Roles.
// It creates the my-project org + my-project-config repo, a pull robot, the
// ArgoCD repository Secret, and the repo_push webhook — all idempotently, so it
// re-runs on every apply (the quay-init script's check-then-mutate posture).
let QUAY_BOOTSTRAP = "my-project-quay-bootstrap"

// QUAY_ORG / QUAY_REPO are the Quay organization and repository the Job creates.
// The org doubles as the my-project name; the repo holds the rendered
// my-project-config OCI artifact CONFIG_REPO points at.
let QUAY_ORG = NAME
let QUAY_REPO = "my-project-config"

// ROBOT_SHORTNAME is the pull robot's org-scoped short name; its fully-qualified
// account name is <org>+<shortname> (Quay's robot naming).  The robot is granted
// READ (pull) on the my-project-config repo and its token feeds the ArgoCD
// repository Secret so Argo CD's repo-server can pull the private OCI artifact.
let ROBOT_SHORTNAME = "argocd-pull"
let ROBOT_ACCOUNT = "\(QUAY_ORG)+\(ROBOT_SHORTNAME)"

// ARGOCD_REPO_SECRET is the Argo CD repository Secret the Job writes into the
// argocd namespace (label argocd.argoproj.io/secret-type: repository, type: oci)
// so the my-project Application can pull the private my-project-config artifact.
// Named after the repo per holos/docs/argocd-application-source.md.
let ARGOCD_REPO_SECRET = QUAY_REPO

// QUAY_API is the in-cluster Quay REST API base URL.  The Job reaches Quay's
// /api/v1 REST API over the plain-HTTP cluster Service (quay.quay.svc:8080), NOT
// the public https://quay.holos.localhost hostname, on purpose:
// holos/docs/argocd-application-source.md documents that curl/libcurl hardcode
// *.localhost to loopback and bypass the quay-holos-localhost ServiceEntry, so a
// public-hostname curl from this pod would never reach Quay.  The /api/v1 REST
// endpoints (organization, repository, robots, notification) are plain JSON and
// do NOT depend on Quay's v2 registry token-auth realm (the one piece that pins
// the public hostname — SERVER_HOSTNAME in components/quay), so the Service URL
// is the robust in-cluster path and needs no CA trust.  The webhook config.url
// the Job registers with Quay is still the public Kargo receiver URL — Quay's
// server (not this Job) POSTs to it later, resolving it via the
// kargo-webhooks-holos-localhost ServiceEntry from the quay namespace.
let QUAY_API = "http://quay.quay.svc.cluster.local:8080"

// K8S_IMAGE pins the image the Quay-provisioning Job runs.  Unlike the
// webhook-token Job (KUBECTL_IMAGE, kubectl-only), this Job also needs curl and
// jq to drive Quay's REST API, so it uses alpine/k8s — the same upstream
// alpine/* family, a manifest list including linux/arm64, bundling kubectl,
// curl, and jq in one /bin/sh image.  Pinned to the same 1.33.3 kubectl minor as
// KUBECTL_IMAGE so both Jobs track one Kubernetes client version.  Verified
// multi-arch (amd64+arm64) and that it carries kubectl+curl+jq.
let K8S_IMAGE = "docker.io/alpine/k8s:1.33.3"

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
//     hard-to-guess URL phase 4's Quay bootstrap Job registers with Quay.
let PROJECT_CONFIG_RESOURCE = {
	apiVersion: "kargo.akuity.io/v1alpha1"
	kind:       "ProjectConfig"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		promotionPolicies: [{
			stage:                STAGE
			autoPromotionEnabled: true
		}]
		webhookReceivers: [{
			name: "quay"
			quay: secretRef: name: WEBHOOK_SECRET
		}]
	}
}

// The Quay webhook receiver Secret is generated at runtime, never committed (the
// repo's runtime-secret posture; see CLAUDE.md "OIDC Client Secrets" and the
// quay secret-keys precedent).  A small create-if-absent Job generates the token
// once and leaves an existing Secret untouched, so the value stays stable across
// re-applies (Kargo derives the hard-to-guess receiver URL from it — a rotation
// would silently invalidate the URL Quay was registered with).
//
// Key naming: the Kargo 1.10.3 ProjectConfig CRD documents that a QUAY receiver
// Secret's data map is read from the `secret` key (NOT `secret-token`, which is
// the key for the artifactory/github-style receivers — verified against the
// vendored CRD).  The issue's acceptance criteria asked for a `secret-token`
// key; to satisfy BOTH the issue's literal AC and Kargo's actual quay-receiver
// contract, the Job writes the same generated token under BOTH `secret` and
// `secret-token`.  `secret` is the functional key Kargo consumes; `secret-token`
// is carried for AC compliance and forward-compatibility with receivers that
// use it.  The token is piped as a manifest on stdin so it never appears in the
// container's argv.
let WEBHOOK_BOOTSTRAP_SCRIPT = """
	set -eu
	if kubectl -n \(NAMESPACE) get secret \(WEBHOOK_SECRET) >/dev/null 2>&1; then
	  echo "Secret \(WEBHOOK_SECRET) already exists; leaving it untouched."
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
	  secret-token: "${TOKEN}"
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

// Scoped to the one Secret the Job manages: get is restricted to the
// WEBHOOK_SECRET resourceName; create cannot be restricted by resourceName (the
// API server does not evaluate resourceNames for create), so the create grant is
// namespace-wide on secrets — acceptable in a namespace whose Secrets all belong
// to this project (the quay secret-keys bootstrap Role precedent).
let WEBHOOK_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   WEBHOOK_BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
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

// --- Quay-provisioning Job (HOL-1272) ---------------------------------------

// QUAY_BOOTSTRAP_SCRIPT idempotently provisions everything the my-project
// delivery loop needs on the Quay side, then registers the Kargo push webhook.
// Every step is check-then-mutate (the scripts/quay-init posture) so a re-apply
// is a no-op.  It runs in the quay namespace where the admin token lives.
//
// Reachability: the REST API is driven over the plain-HTTP cluster Service
// (QUAY_API = quay.quay.svc:8080) — see the QUAY_API comment for why the public
// hostname is unreachable from this pod.  No CA trust is needed for that path.
//
// Webhook auth model (verified against the Kargo 1.10.3 ProjectConfig CRD): the
// Kargo quay receiver does NOT validate a shared token in the request — the CRD
// documents that the receiver Secret's `secret` value is used ONLY by Kargo to
// derive a "complex, hard-to-guess URL, which implicitly serves as a shared
// secret" and "does NOT need to be shared directly with Quay".  So the Job reads
// the hard-to-guess receiver URL from ProjectConfig.status and registers it
// verbatim as the webhook config.url; the URL itself is the credential.  The Job
// still reads the my-project-quay-webhook Secret's token (AC compliance / a
// guard that the receiver Secret exists), but Quay's webhook config carries no
// token because the receiver ignores one.
//
// Polling: ProjectConfig.status.webhookReceivers[].url is populated
// asynchronously by Kargo after the ProjectConfig reconciles, so the script
// polls until the quay receiver's URL appears (bounded) before registering it.
let QUAY_BOOTSTRAP_SCRIPT = """
	set -eu

	QUAY_API="\(QUAY_API)"
	ORG="\(QUAY_ORG)"
	REPO="\(QUAY_REPO)"
	ROBOT_SHORT="\(ROBOT_SHORTNAME)"
	ROBOT="\(ROBOT_ACCOUNT)"
	REPO_PATH="${ORG}/${REPO}"

	# Admin OAuth token (quay-initial-admin Secret, key `token`, quay ns).  Quay's
	# API takes the Bearer token; basic auth is not accepted.
	TOKEN="$(kubectl -n \(QUAY_NAMESPACE) get secret quay-initial-admin -o jsonpath='{.data.token}' | base64 -d)"
	[ -n "${TOKEN}" ]

	# http METHOD PATH [JSON_BODY]: prints the numeric HTTP status to stdout and
	# leaves the response body in /tmp/resp.  The Bearer token rides in a 0600
	# config file (curl --config) so it never appears in the process list/argv.
	printf 'header = "Authorization: Bearer %s"\\n' "${TOKEN}" > /tmp/auth.curlrc
	chmod 600 /tmp/auth.curlrc
	http() {
	  _m="$1"; _p="$2"; _body="${3:-}"
	  if [ -n "${_body}" ]; then
	    curl -sS --max-time 30 -o /tmp/resp -w '%{http_code}' \\
	      --config /tmp/auth.curlrc -H 'Content-Type: application/json' \\
	      -X "${_m}" --data "${_body}" "${QUAY_API}${_p}"
	  else
	    curl -sS --max-time 30 -o /tmp/resp -w '%{http_code}' \\
	      --config /tmp/auth.curlrc -X "${_m}" "${QUAY_API}${_p}"
	  fi
	}

	# --- organization (create-if-404) ---
	st="$(http GET "/api/v1/organization/${ORG}")"
	if [ "${st}" = "200" ]; then
	  echo "Organization ${ORG} exists; skipping."
	elif [ "${st}" = "404" ]; then
	  st="$(http POST /api/v1/organization/ "$(jq -nc --arg n "${ORG}" --arg e "org+${ORG}@holos.localhost" '{name:$n,email:$e}')")"
	  [ "${st}" = "201" ] || { echo "ERROR: create org ${ORG} (HTTP ${st})"; cat /tmp/resp; exit 1; }
	  echo "Created organization ${ORG}."
	else
	  echo "ERROR: checking org ${ORG} (HTTP ${st})"; cat /tmp/resp; exit 1
	fi

	# --- repository (create-if-absent) ---
	st="$(http GET "/api/v1/repository/${REPO_PATH}")"
	if [ "${st}" = "200" ]; then
	  echo "Repository ${REPO_PATH} exists; skipping."
	elif [ "${st}" = "404" ]; then
	  st="$(http POST /api/v1/repository "$(jq -nc --arg ns "${ORG}" --arg r "${REPO}" '{namespace:$ns,repository:$r,visibility:"private",repo_kind:"image"}')")"
	  [ "${st}" = "201" ] || { echo "ERROR: create repo ${REPO_PATH} (HTTP ${st})"; cat /tmp/resp; exit 1; }
	  echo "Created repository ${REPO_PATH}."
	else
	  echo "ERROR: checking repo ${REPO_PATH} (HTTP ${st})"; cat /tmp/resp; exit 1
	fi

	# --- robot (create-if-absent; capture its token) ---
	ROBOT_TOKEN=""
	st="$(http GET "/api/v1/organization/${ORG}/robots/${ROBOT_SHORT}")"
	if [ "${st}" = "200" ]; then
	  echo "Robot ${ROBOT} exists; skipping create."
	  ROBOT_TOKEN="$(jq -r '.token' < /tmp/resp)"
	elif [ "${st}" = "400" ] || [ "${st}" = "404" ]; then
	  st="$(http PUT "/api/v1/organization/${ORG}/robots/${ROBOT_SHORT}" "$(jq -nc '{description:"ArgoCD pull robot for my-project-config (HOL-1272)."}')")"
	  [ "${st}" = "201" ] || { echo "ERROR: create robot ${ROBOT} (HTTP ${st})"; cat /tmp/resp; exit 1; }
	  echo "Created robot ${ROBOT}."
	  ROBOT_TOKEN="$(jq -r '.token' < /tmp/resp)"
	else
	  echo "ERROR: checking robot ${ROBOT} (HTTP ${st})"; cat /tmp/resp; exit 1
	fi
	[ -n "${ROBOT_TOKEN}" ] && [ "${ROBOT_TOKEN}" != "null" ] || { echo "ERROR: robot carried no token"; exit 1; }

	# --- grant the robot READ (pull) on the repository (upsert; idempotent) ---
	st="$(http PUT "/api/v1/repository/${REPO_PATH}/permissions/user/${ROBOT}" "$(jq -nc '{role:"read"}')")"
	[ "${st}" = "200" ] || { echo "ERROR: grant robot pull on ${REPO_PATH} (HTTP ${st})"; cat /tmp/resp; exit 1; }
	echo "Granted ${ROBOT} read access on ${REPO_PATH}."

	# --- ArgoCD repository Secret (create-if-absent; argocd ns) ---
	# Holds the robot pull credential so Argo CD's repo-server can pull the private
	# OCI artifact (holos/docs/argocd-application-source.md).  Job-owned and left
	# untouched if it already exists so the robot token stays stable across
	# re-applies (a regenerated token would break Argo CD until it re-syncs).
	if kubectl -n \(ArgoCDNamespace) get secret \(ARGOCD_REPO_SECRET) >/dev/null 2>&1; then
	  echo "Argo CD repository Secret \(ARGOCD_REPO_SECRET) exists; leaving it untouched."
	else
	  kubectl -n \(ArgoCDNamespace) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(ARGOCD_REPO_SECRET)
	  namespace: \(ArgoCDNamespace)
	  labels:
	    argocd.argoproj.io/secret-type: repository
	    app.kubernetes.io/name: \(NAME)
	stringData:
	  name: \(ARGOCD_REPO_SECRET)
	  url: \(CONFIG_REPO_OCI)
	  type: oci
	  username: ${ROBOT}
	  password: ${ROBOT_TOKEN}
	  insecure: "true"
	EOF
	  echo "Created Argo CD repository Secret \(ARGOCD_REPO_SECRET) in \(ArgoCDNamespace)."
	fi

	# --- read the Kargo receiver URL from ProjectConfig.status (poll) ---
	# Kargo fills status.webhookReceivers[].url asynchronously after the
	# ProjectConfig reconciles; poll until the quay receiver's URL appears.
	RECEIVER_URL=""
	i=0
	while [ "${i}" -lt 60 ]; do
	  RECEIVER_URL="$(kubectl -n \(NAMESPACE) get projectconfig \(NAME) -o jsonpath='{.status.webhookReceivers[?(@.name=="quay")].url}' 2>/dev/null || true)"
	  [ -n "${RECEIVER_URL}" ] && break
	  echo "Waiting for ProjectConfig \(NAME) quay receiver URL... (${i})"
	  i=$((i + 1))
	  sleep 5
	done
	[ -n "${RECEIVER_URL}" ] || { echo "ERROR: ProjectConfig \(NAME) quay receiver URL never populated"; exit 1; }
	echo "Kargo quay receiver URL: ${RECEIVER_URL}"

	# Read (and require) the receiver Secret's token.  The Kargo quay receiver does
	# NOT validate a token in the request — the hard-to-guess RECEIVER_URL is the
	# shared secret (Kargo 1.10.3 ProjectConfig CRD) — so it is NOT sent to Quay;
	# this read just asserts the receiver Secret exists (it backs the URL above).
	kubectl -n \(NAMESPACE) get secret \(WEBHOOK_SECRET) >/dev/null 2>&1 || { echo "ERROR: receiver Secret \(WEBHOOK_SECRET) missing"; exit 1; }

	# --- repo_push webhook notification (create-if-absent) ---
	# List existing notifications and skip if an equivalent webhook already points
	# at the receiver URL (idempotency); otherwise create it.
	st="$(http GET "/api/v1/repository/${REPO_PATH}/notification/")"
	[ "${st}" = "200" ] || { echo "ERROR: list notifications on ${REPO_PATH} (HTTP ${st})"; cat /tmp/resp; exit 1; }
	if jq -e --arg u "${RECEIVER_URL}" '.notifications // [] | .[] | select(.event=="repo_push" and .method=="webhook" and .config.url==$u)' < /tmp/resp >/dev/null; then
	  echo "repo_push webhook to the receiver URL already exists; skipping."
	else
	  st="$(http POST "/api/v1/repository/${REPO_PATH}/notification/" "$(jq -nc --arg u "${RECEIVER_URL}" '{event:"repo_push",method:"webhook",config:{url:$u},eventConfig:{},title:"kargo-my-project"}')")"
	  [ "${st}" = "201" ] || { echo "ERROR: create repo_push webhook on ${REPO_PATH} (HTTP ${st})"; cat /tmp/resp; exit 1; }
	  echo "Created repo_push webhook on ${REPO_PATH} -> ${RECEIVER_URL}."
	fi

	echo "Quay provisioning complete for ${REPO_PATH}."
	"""

// QUAY_BOOTSTRAP_METADATA labels the Job/SA/Role/RoleBinding in the quay
// namespace.  Its own app.kubernetes.io/name (NOT the Quay Deployment's) keeps
// the Service selector from ever matching this short-lived pod, the same caveat
// the quay secret-keys bootstrap documents.
let QUAY_BOOTSTRAP_METADATA = {
	name:      QUAY_BOOTSTRAP
	namespace: QUAY_NAMESPACE
	labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
}

let QUAY_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   QUAY_BOOTSTRAP_METADATA
}

// The three Role/RoleBinding pairs carry a namespace-suffixed metadata.name
// (<base>-<namespace>) so kubectl-slice writes a distinct file per resource:
// the slicer keys filenames on kind+metadata.name, so three Roles all named
// QUAY_BOOTSTRAP would collide into one file even though they live in different
// namespaces.  This mirrors the quay-oidc-bootstrap Roles
// (components/keycloak/realm-config), which span keycloak+quay the same way.
// The roleRef.name and the bound subject ServiceAccount stay constant — only the
// Role/RoleBinding object names are suffixed.

// Role in the quay namespace: read the admin OAuth token the Job authenticates
// with.  Scoped by resourceName to the one Secret it reads.  (The Job drives the
// REST API over the plain-HTTP Service, so it needs no local-CA cert — only
// quay-initial-admin is granted.)
let QUAY_BOOTSTRAP_ROLE_QUAY = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata: {
		name:      "\(QUAY_BOOTSTRAP)-quay"
		namespace: QUAY_NAMESPACE
		labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
	}
	rules: [{
		apiGroups: [""]
		resources: ["secrets"]
		verbs: ["get"]
		resourceNames: ["quay-initial-admin"]
	}]
}

let QUAY_BOOTSTRAP_ROLE_BINDING_QUAY = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata: {
		name:      "\(QUAY_BOOTSTRAP)-quay"
		namespace: QUAY_NAMESPACE
		labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
	}
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     "\(QUAY_BOOTSTRAP)-quay"
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      QUAY_BOOTSTRAP
		namespace: QUAY_NAMESPACE
	}]
}

// Role in the my-project namespace: get the ProjectConfig (to read the
// status receiver URL) and get the webhook receiver Secret.  Bound to the
// quay-namespace ServiceAccount.
let QUAY_BOOTSTRAP_ROLE_PROJECT = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata: {
		name:      "\(QUAY_BOOTSTRAP)-project"
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
	}
	rules: [
		{
			apiGroups: ["kargo.akuity.io"]
			resources: ["projectconfigs"]
			verbs: ["get"]
			resourceNames: [NAME]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [WEBHOOK_SECRET]
		},
	]
}

let QUAY_BOOTSTRAP_ROLE_BINDING_PROJECT = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata: {
		name:      "\(QUAY_BOOTSTRAP)-project"
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
	}
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     "\(QUAY_BOOTSTRAP)-project"
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      QUAY_BOOTSTRAP
		namespace: QUAY_NAMESPACE
	}]
}

// Role in the argocd namespace: get (to check existence) and create the
// repository Secret.  create cannot be restricted by resourceName (the API
// server does not evaluate resourceNames for create), so the create grant is
// namespace-wide on secrets — the same scoping the quay secret-keys bootstrap
// Role accepts; get IS scoped to the one Secret.  Bound to the quay-namespace
// ServiceAccount.
let QUAY_BOOTSTRAP_ROLE_ARGOCD = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata: {
		name:      "\(QUAY_BOOTSTRAP)-argocd"
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
	}
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [ARGOCD_REPO_SECRET]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let QUAY_BOOTSTRAP_ROLE_BINDING_ARGOCD = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata: {
		name:      "\(QUAY_BOOTSTRAP)-argocd"
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": QUAY_BOOTSTRAP
	}
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     "\(QUAY_BOOTSTRAP)-argocd"
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      QUAY_BOOTSTRAP
		namespace: QUAY_NAMESPACE
	}]
}

// CAVEAT (same as the webhook-token bootstrap): a completed Job's pod template is
// immutable, so scripts/apply cannot re-run it by re-applying the unchanged spec.
// pre_my_project() in scripts/apply deletes any prior Job before each apply so a
// fresh idempotent Job runs every time; the resources it created (org, repo,
// robot, Secret, webhook) are all check-then-mutate, so a re-run converges
// without duplicating anything.  ttlSecondsAfterFinished garbage-collects the Job
// a day after completion.
let QUAY_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   QUAY_BOOTSTRAP_METADATA
	spec: {
		// The receiver-URL poll can take a while (Kargo fills status
		// asynchronously); a higher activeDeadline guards a wedged poll, and
		// backoffLimit allows a couple of transient-API retries.
		backoffLimit:            3
		activeDeadlineSeconds:   600
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: QUAY_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: QUAY_BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// alpine/k8s declares no non-root USER; pick the
					// conventional "nobody" uid (mirrors the quay bootstrap).
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "bootstrap"
					image: K8S_IMAGE
					command: ["/bin/sh", "-c", QUAY_BOOTSTRAP_SCRIPT]
					// kubectl and curl write caches under $HOME; point it at the
					// writable emptyDir since the root filesystem is read-only.
					env: [{
						name:  "HOME"
						value: "/tmp"
					}]
					resources: {
						requests: {
							cpu:    "10m"
							memory: "64Mi"
						}
						limits: memory: "128Mi"
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
					Project: (NAME):        PROJECT_RESOURCE
					ProjectConfig: (NAME):  PROJECT_CONFIG_RESOURCE
					Warehouse: (WAREHOUSE): WAREHOUSE_RESOURCE
					Stage: (STAGE):         STAGE_RESOURCE
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

					// Quay-provisioning Job (HOL-1272).  Its ServiceAccount and
					// Job render into the quay namespace; its three Role/RoleBinding
					// pairs span the quay, my-project, and argocd namespaces.  Each
					// #Resources entry is keyed by a UNIQUE internal label per Kind
					// (the namespace alone does not disambiguate map keys), so the
					// three Roles/RoleBindings carry "-quay"/"-project"/"-argocd"
					// suffixes; the rendered metadata.name stays QUAY_BOOTSTRAP in
					// every namespace (one name, three namespaces).
					ServiceAccount: (QUAY_BOOTSTRAP):              QUAY_BOOTSTRAP_SERVICE_ACCOUNT
					Job: (QUAY_BOOTSTRAP):                         QUAY_BOOTSTRAP_JOB
					Role: (QUAY_BOOTSTRAP_ROLE_QUAY.metadata.name):                    QUAY_BOOTSTRAP_ROLE_QUAY
					RoleBinding: (QUAY_BOOTSTRAP_ROLE_BINDING_QUAY.metadata.name):      QUAY_BOOTSTRAP_ROLE_BINDING_QUAY
					Role: (QUAY_BOOTSTRAP_ROLE_PROJECT.metadata.name):                 QUAY_BOOTSTRAP_ROLE_PROJECT
					RoleBinding: (QUAY_BOOTSTRAP_ROLE_BINDING_PROJECT.metadata.name):   QUAY_BOOTSTRAP_ROLE_BINDING_PROJECT
					Role: (QUAY_BOOTSTRAP_ROLE_ARGOCD.metadata.name):                  QUAY_BOOTSTRAP_ROLE_ARGOCD
					RoleBinding: (QUAY_BOOTSTRAP_ROLE_BINDING_ARGOCD.metadata.name):    QUAY_BOOTSTRAP_ROLE_BINDING_ARGOCD
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

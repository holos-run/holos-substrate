package holos

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
//     projects (Phase 4, HOL-1377).  It is scoped to the tenant project OCI repos
//     (any Quay org/repo) and deliberately OMITS clusterResourceWhitelist — like
//     the per-project AppProject the project component emits — so the tenant
//     App-of-Apps cannot create cluster-scoped resources.
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

// MANIFESTS_REPO is the per-app rendered-manifests OCI repository the existing
// echo/Kargo delivery pipeline publishes to (components/kargo-echo).  The
// platform AppProject authorizes it too so a system-owned Application may also
// source from it; the oci:// form matches the Application source repoURL string.
let MANIFESTS_REPO_OCI = "oci://quay.holos.internal/holos/holos-paas-manifests"

// IN_CLUSTER is the in-cluster Kubernetes API server destination every Argo CD
// Application in these projects targets.
let IN_CLUSTER = "https://kubernetes.default.svc"

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
		// A '*' suffix on the config repo authorizes the oci:// repoURL whether a
		// child Application pins a tag or a digest; the manifests repo is listed in
		// full for the system-owned Application that may source from it.
		sourceRepos: [
			"\(CONFIG_REPO_OCI)*",
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
// Owns the single top-level App-of-Apps that bootstraps tenant projects (Phase
// 4, HOL-1377).  Scoped to the tenant project OCI repos (any Quay org/repo) with
// destinations covering tenant namespaces.  clusterResourceWhitelist is
// DELIBERATELY omitted — like the per-project AppProject the project component
// emits — so the tenant App-of-Apps cannot create cluster-scoped resources.
let PROJECTS_PROJECT = {
	apiVersion: "argoproj.io/v1alpha1"
	kind:       "AppProject"
	metadata: {
		name:      "projects"
		namespace: ArgoCDNamespace
		labels: "app.kubernetes.io/name": "projects"
	}
	spec: {
		// Any Quay org/repo under the in-cluster registry — the tenant projects'
		// OCI artifacts (the projects App-of-Apps and the per-project bundles).
		sourceRepos: ["oci://quay.holos.internal/*/*"]
		// Tenant namespaces on the in-cluster API server.  '*' admits the project
		// control and env-prefixed namespaces (ci-/qa-/prod-<name>, bare <name>)
		// the projects App-of-Apps fans out into; the absent clusterResourceWhitelist
		// is what actually confines tenants to namespaced resources.
		destinations: [{
			server:    IN_CLUSTER
			namespace: "*"
		}]
		namespaceResourceWhitelist: [{
			group: "*"
			kind:  "*"
		}]
	}
}

// --- Repository credential bootstrap --------------------------------------
//
// Registers the holos-paas-config OCI repository with Argo CD via a repository
// credential Secret in the argocd namespace (labeled
// argocd.argoproj.io/secret-type: repository).  The robot token is secret
// MATERIAL, so neither the credential nor a placeholder for it is committed (the
// Runtime Secret Handling guardrail): a create-if-absent bootstrap Job assembles
// it at runtime, modeled on the Quay OIDC client-secret bootstrap
// (components/keycloak/realm-config).
//
// The robot pull credential (holos+robot) is provisioned manually — the robot
// and this pull credential are NOT modeled by the quay.holos.run CRDs (ADR-19
// Out of scope) — into a SOURCE Secret in the argocd namespace
// (CONFIG_ROBOT_SECRET, keys username/password).  The Job reads that source and
// assembles the argocd-format repository Secret create-if-absent.  If the source
// Secret is absent, the Job logs and exits 0 WITHOUT creating the repository
// Secret, so nothing empty is ever materialized; the repository Secret appears
// on the first apply after an operator provisions the robot credential.

// REPO_SECRET is the argocd-format repository credential Secret the Job writes.
let REPO_SECRET = "holos-paas-config"

// CONFIG_ROBOT_SECRET is the manually-provisioned source holding the Quay
// pull-robot username/password (holos+robot).  Created by hand per the runtime
// secret posture; the Job reads it to assemble REPO_SECRET.
let CONFIG_ROBOT_SECRET = "holos-paas-config-robot"

// KUBECTL_IMAGE pins the image the bootstrap Job runs kubectl from (the
// quay-oidc-bootstrap precedent in components/keycloak/realm-config).
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

let REPO_BOOTSTRAP = "holos-paas-config-repo-bootstrap"

let REPO_BOOTSTRAP_METADATA = {
	name:      REPO_BOOTSTRAP
	namespace: ArgoCDNamespace
	labels: "app.kubernetes.io/name": REPO_BOOTSTRAP
}

// The create-if-absent bootstrap script.  It reads the robot username/password
// from CONFIG_ROBOT_SECRET and assembles the argocd-format repository Secret
// only if absent, never overwriting (generate-once = stable across re-applies).
// If the source Secret is missing it exits 0 without creating anything, so no
// empty-material Secret is ever committed or materialized.  The credential
// material is piped to kubectl create -f - on stdin so it never appears in the
// container's argv (/proc-visible).
let REPO_BOOTSTRAP_SCRIPT = """
	set -eu
	if kubectl -n \(ArgoCDNamespace) get secret \(REPO_SECRET) >/dev/null 2>&1; then
	  echo "Secret \(REPO_SECRET) already exists in \(ArgoCDNamespace); leaving it untouched."
	  exit 0
	fi
	if ! kubectl -n \(ArgoCDNamespace) get secret \(CONFIG_ROBOT_SECRET) >/dev/null 2>&1; then
	  echo "Source Secret \(CONFIG_ROBOT_SECRET) not found in \(ArgoCDNamespace);" >&2
	  echo "provision the holos+robot pull credential (keys username/password) and re-apply." >&2
	  echo "Skipping \(REPO_SECRET) creation for now (the repository is registered once the source exists)." >&2
	  exit 0
	fi
	USERNAME="$(kubectl -n \(ArgoCDNamespace) get secret \(CONFIG_ROBOT_SECRET) -o jsonpath='{.data.username}' | base64 -d)"
	PASSWORD="$(kubectl -n \(ArgoCDNamespace) get secret \(CONFIG_ROBOT_SECRET) -o jsonpath='{.data.password}' | base64 -d)"
	if [ -z "$USERNAME" ] || [ -z "$PASSWORD" ]; then
	  echo "ERROR: \(CONFIG_ROBOT_SECRET) is missing the username or password key." >&2
	  exit 1
	fi
	kubectl -n \(ArgoCDNamespace) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(REPO_SECRET)
	  namespace: \(ArgoCDNamespace)
	  labels:
	    argocd.argoproj.io/secret-type: repository
	stringData:
	  name: \(REPO_SECRET)
	  url: \(CONFIG_REPO_OCI)
	  type: oci
	  username: "${USERNAME}"
	  password: "${PASSWORD}"
	  insecure: "true"
	EOF
	echo "Secret \(REPO_SECRET) created in \(ArgoCDNamespace)."
	"""

let REPO_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   REPO_BOOTSTRAP_METADATA
}

// Role granting the Job get on the source/target Secrets and namespace-wide
// create on secrets (the API server does not evaluate resourceNames for create).
// Both the source and target Secrets live in the argocd namespace, so a single
// Role/RoleBinding pair suffices.
let REPO_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   REPO_BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [REPO_SECRET, CONFIG_ROBOT_SECRET]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let REPO_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   REPO_BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     REPO_BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      REPO_BOOTSTRAP
		namespace: ArgoCDNamespace
	}]
}

// A completed Job's pod template is immutable, so a plain re-apply of this
// unchanged spec is a no-op while the Job exists; ttlSecondsAfterFinished keeps
// its logs around for a day while still dissolving the immutable-pod-template
// caveat for routine re-applies (the quay-oidc-bootstrap precedent).  The Job is
// idempotent — it exits 0 leaving an existing repository Secret untouched (the
// create-if-absent script above) — and the Secret survives the Job deletion, so
// the generate-once guarantee holds across re-runs.
let REPO_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   REPO_BOOTSTRAP_METADATA
	spec: {
		backoffLimit:            3
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: REPO_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: REPO_BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// The alpine/kubectl image declares no non-root USER; pick the
					// conventional "nobody" uid (the quay-oidc-bootstrap precedent).
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:    "bootstrap"
					image:   KUBECTL_IMAGE
					command: ["/bin/sh", "-c", REPO_BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME; point it at the
					// writable emptyDir since the root filesystem is read-only.
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
				// (typed argoproj.io/appproject binding) and the bootstrap
				// Job/SA/Role/RoleBinding validate against the vendored schemas at
				// render time.
				resources: #Resources & {
					AppProject: {
						(PLATFORM_PROJECT.metadata.name): PLATFORM_PROJECT
						(PROJECTS_PROJECT.metadata.name): PROJECTS_PROJECT
					}
					Job: (REPO_BOOTSTRAP_JOB.metadata.name):                       REPO_BOOTSTRAP_JOB
					ServiceAccount: (REPO_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): REPO_BOOTSTRAP_SERVICE_ACCOUNT
					Role: (REPO_BOOTSTRAP_ROLE.metadata.name):                     REPO_BOOTSTRAP_ROLE
					RoleBinding: (REPO_BOOTSTRAP_ROLE_BINDING.metadata.name):       REPO_BOOTSTRAP_ROLE_BINDING
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

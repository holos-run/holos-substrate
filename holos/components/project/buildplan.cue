package holos

import (
	"strings"
	"regexp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/base64"

	kargowarehouse "kargo.akuity.io/warehouse/v1alpha1"
	kargostage "kargo.akuity.io/stage/v1alpha1"
)

// project is the collection-driven generalization of the hand-authored
// my-project scaffold (holos/components/my-project/buildplan.cue, HOL-1268..HOL-1348):
// it iterates the `projects` collection (holos/collections.cue, HOL-1354) and
// renders, FOR EACH projects.<name> entry, the full project-level resource set —
// the IAM primitive-role groups (owner/editor/viewer) + custodians, the owner
// KeycloakUser(s), the project KeycloakClient, the Quay Organization with its
// OIDC-synced teams, the Kargo control plane (Project/ProjectConfig/Warehouse/
// Stage + the webhook-token bootstrap Job), the Argo CD AppProject/Application,
// and the owner-access RoleBinding (ADR-3).  A Project STANDS ALONE with zero
// apps: this component renders a complete, applyable project on its own; per-app
// resources (HTTPRoute, Gateway ReferenceGrant, Deployment, …) are the
// Application component's concern (HOL-1356).  Read ADR-21 (the Project/
// Application component design) and the my-project buildplan (the literal
// template) alongside this file.
//
// --- THE CONTROL-NAMESPACE RESOLUTION (the trickiest interaction, HOL-1355) ---
//
// A project's keycloak.holos.run role KeycloakGroups confer the project-prefixed
// client role <name>-<role> on the platform Quay client
// (https://quay.holos.internal) DIRECTLY via clientRoles[].clientId — the
// ADR-20 Rev 4 "Quay use case" (HOL-1350) that surfaces <name>-<role> in Quay's
// groups claim (the already-deployed quay-client-roles mapper) so the
// Organization's spec.syncedTeams[].oidcGroup MEMBERSHIP populates.  That direct
// path is tightly guarded by the controller (validateDirectClientRole,
// internal/controller/keycloak/group_controller.go): it is allowed ONLY when the
// group path is projects/<project>/roles/<leaf>, the role is exactly
// <project>-<leaf>, AND the KeycloakGroup CR's metadata.namespace EQUALS the bare
// project name <project> (the project↔namespace ownership boundary — RBAC governs
// who creates a CR in a namespace).
//
// That guard is IMMOVABLE here (changing it is controller scope, not this
// component's) and it FORCES the project's keycloak.holos.run CRs — and, for a
// single co-located control plane, all the project-scoped control CRs — into a
// namespace named exactly <name> (the bare project name).  This DEVIATES from
// ADR-21 Revision 3's recorded control-namespace pick of prod-<name>
// (#ProjectControlEnvironment): that revision predates the as-built guard
// (HOL-1350) and chose prod-<name> for namespace-economy reasons, but a
// prod-<name>-namespaced role group fails validateDirectClientRole (project
// segment <name> ≠ namespace prod-<name>), which would silently BREAK the Quay
// claim population that AC #2/#3 require — the heart of the feature.  ADR-21
// itself flagged the "un-prefixed <name> control namespace" as the considered
// alternative; the controller guard settles the choice in its favor.  The bare
// <name> control namespace is also exactly what the working bespoke my-project
// component uses today (it lives in the bare my-project namespace), so this keeps
// the generalized and bespoke components on the same topology until HOL-1357
// migrates my-project onto this template.  ADR-21's revision to ratify this
// (Status stays Proposed) is HOL-1358 (the docs/ADR finalization phase); a
// Deferred Acceptance Criterion on this PR records it.
//
// Two registry/grant prerequisites follow from the bare-<name> control namespace
// and are wired in this same change (each a small, collection-derived edit):
//
//   - holos/namespaces.cue derives a bare <name> CONTROL namespace per project
//     (alongside the ci-/qa-/prod-<name> env namespaces HOL-1354 already
//     derives), so #RegisteredNamespace admits it and the namespaces component
//     renders it.
//   - holos/components/keycloak/keycloak-instance/buildplan.cue's ReferenceGrant
//     authorizes the bare <name> namespace's keycloak.holos.run referrers to
//     reference the central KeycloakInstance cross-namespace (the grant is owned
//     by the keycloak namespace, ADR-22 — this component does not re-emit it).
//
// --- my-project: now produced by this component (HOL-1357) ---------------------
//
// This component iterates EVERY registered project, including my-project.  The
// bespoke holos/components/my-project component — which formerly emitted that
// project's resources (the Quay org, the Keycloak group paths, the cluster-scoped
// Kargo Project) — was DELETED in HOL-1357, so this generalized component is the
// sole producer of the reference instance's project-level resource set.  The
// rendered my-project tree is behavior-equivalent to the bespoke one, with two
// deliberate, documented supersets: the owner-access RoleBinding (the bespoke
// component lacked it) and the env-derived hash suffix on the owner KeycloakUser's
// metadata.name (a DNS-safe, collision-resistant name derived from the email).
// See the PR description for the full pre/post diff rationale.

// ROLES is the GCP-style primitive role triad every project provisions
// (owner/editor/viewer), shared by the Keycloak role/custodian groups, the
// project client's client roles, and the Quay synced teams.
let ROLES = ["owner", "editor", "viewer"]

// ENV_CONTROL is the project's single control namespace ENVIRONMENT-FREE name:
// the bare project name <name> (see the control-namespace resolution above).
// Distinct from #ProjectControlEnvironment ("prod") — the as-built controller
// guard requires the bare name for the Quay-direct client-role path, so the
// control CRs land in <name>, not prod-<name>.

// QUAY_CLIENT_ID is the platform Quay client's reserved clientId (realm-config's
// QUAY_CLIENT_ID).  The role groups confer <name>-<role> on it directly via
// clientRoles[].clientId (no tenant KeycloakClient CR exists for the reserved
// client; the reserved-name guard forbids one), the ADR-20 Rev 4 Quay use case.
let QUAY_CLIENT_ID = "https://quay.holos.internal"

// KEYCLOAK_NAMESPACE is the central KeycloakInstance's namespace; the project CRs
// reference it cross-namespace (gated by the keycloak-instance component's
// ReferenceGrant).  KEYCLOAK_INSTANCE is that instance's name.
let KEYCLOAK_NAMESPACE = "keycloak" & #RegisteredNamespace
let KEYCLOAK_INSTANCE = "holos-keycloak"

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd); both
// the per-project AppProject and Application are namespaced into it.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// KUBECTL_IMAGE pins the image the webhook-token bootstrap Job runs kubectl from
// — the same manifest-list alpine image and rationale as the my-project scaffold.
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

// #ProjectResources derives, for ONE project (name NAME, owners OWNERS), the
// full project-level resource set as a #Resources-shaped struct.  Every literal
// the my-project scaffold hard-codes (the "my-project" name, "bob@example.com"
// owner, the namespace) is a parameter here.  Factored as a definition so the
// per-project artifact comprehension below stays legible.
#ProjectResources: {
	// NAME is the project name (a projects map key, DNS-label validated by the
	// collection schema).  It is the base of every resource name and the Quay
	// org / Keycloak group-path / Kargo Project identity.
	NAME: string

	// OWNERS is the project's owners map (projects.<name>.owners) keyed by email;
	// one KeycloakUser is rendered per entry, joined to the owner role group.
	OWNERS: {[string]: {email: string, ...}}

	// CTRL_NS is the project's CONTROL namespace: the bare project name (see the
	// control-namespace resolution in the file header).  Unified with
	// #RegisteredNamespace so a missing registry entry fails at render, not apply.
	let CTRL_NS = NAME & #RegisteredNamespace

	// STAGE is the Kargo Stage authorized to patch the Application's
	// targetRevision; WAREHOUSE is the Warehouse the Stage requests Freight from.
	let STAGE = "project-config"
	let WAREHOUSE = NAME

	// CONFIG_REPO is the rendered-manifests OCI repository the publish workflow
	// pushes to, scoped under the project's Quay org path so the AppProject can
	// constrain sourceRepos to oci://quay.holos.internal/<name>/*.  The oci://
	// form (CONFIG_REPO_OCI) must stay byte-identical between the Application
	// source and the Stage's argocd-update source (Kargo matches by exact string).
	let CONFIG_REPO = "quay.holos.internal/\(NAME)/\(NAME)-config"
	let CONFIG_REPO_OCI = "oci://\(CONFIG_REPO)"

	// CONFIG_TAG_REGEX scopes the Warehouse subscription to the input-addressed
	// render-<config12>-<appimage12> tags scripts/publish mints.
	let CONFIG_TAG_REGEX = "^render-[0-9a-f]{12}-[0-9a-f]{12}$"

	// INSTANCE_REF is the shared cross-namespace reference every project Keycloak
	// CR carries (ReferenceGrant-gated; namespace differs from the control ns).
	let INSTANCE_REF = {
		name:      KEYCLOAK_INSTANCE
		namespace: KEYCLOAK_NAMESPACE
	}

	// PROJECT_CLIENT_NAME is the project KeycloakClient CR's metadata.name (the
	// object name the role groups' clientRoles[].clientRef resolves — NOT the URL
	// clientId).  PROJECT_CLIENT_ID is its URL-shaped clientId.
	let PROJECT_CLIENT_NAME = NAME
	let PROJECT_CLIENT_ID = "https://\(NAME).holos.internal"

	// PROJECT_CLIENT_SECRET is where the controller delivers the confidential
	// client's generated secret (generate-once, in the control namespace, never
	// committed — the Runtime Secret Handling guardrail).
	let PROJECT_CLIENT_SECRET = "\(NAME)-oidc"

	// CLIENT_ROLE maps each primitive role to its flat client-role / claim value
	// <name>-<role> (the value the Organization's syncedTeams[].oidcGroup binds).
	let CLIENT_ROLE = {for r in ROLES {(r): "\(NAME)-\(r)"}}

	// WEBHOOK_SECRET is the Kargo Quay webhook receiver Secret; WEBHOOK_BOOTSTRAP
	// names the create-if-absent Job (+ SA/Role/RoleBinding) that generates its
	// token once.
	let WEBHOOK_SECRET = "\(NAME)-quay-webhook"
	let WEBHOOK_BOOTSTRAP = "\(WEBHOOK_SECRET)-bootstrap"

	// --- Argo CD: AppProject + Application -----------------------------------

	// APPPROJECT_RESOURCE scopes what the project's Application may deploy:
	// sourceRepos constrained to the project's Quay org OCI path, destinations to
	// the project's control namespace, namespaceResourceWhitelist permissive,
	// clusterResourceWhitelist deliberately omitted (no cluster-scoped deploys).
	let APPPROJECT_RESOURCE = {
		apiVersion: "argoproj.io/v1alpha1"
		kind:       "AppProject"
		metadata: {
			name:      NAME
			namespace: ArgoCDNamespace
			labels: "app.kubernetes.io/name": NAME
		}
		spec: {
			sourceRepos: ["oci://quay.holos.internal/\(NAME)/*"]
			destinations: [{
				server:    "https://kubernetes.default.svc"
				namespace: CTRL_NS
			}]
			namespaceResourceWhitelist: [{
				group: "*"
				kind:  "*"
			}]
		}
	}

	// APPLICATION_RESOURCE is the project-level Argo CD Application Kargo patches.
	// targetRevision is DELIBERATELY OMITTED so Kargo solely owns it (the
	// "imperative revision, declarative Application" posture — see the my-project
	// scaffold's full rationale).  The authorized-stage annotation authorizes the
	// project's project-config Stage to modify it.
	let APPLICATION_RESOURCE = {
		apiVersion: "argoproj.io/v1alpha1"
		kind:       "Application"
		metadata: {
			name:      NAME
			namespace: ArgoCDNamespace
			labels: "app.kubernetes.io/name":                NAME
			annotations: "kargo.akuity.io/authorized-stage": "\(CTRL_NS):\(STAGE)"
		}
		spec: {
			project: NAME
			source: {
				repoURL: CONFIG_REPO_OCI
				path:    "."
			}
			destination: {
				server:    "https://kubernetes.default.svc"
				namespace: CTRL_NS
			}
			syncPolicy: {
				automated: {
					prune:    true
					selfHeal: true
				}
				syncOptions: ["CreateNamespace=false"]
			}
		}
	}

	// --- Quay data plane: the Organization + synced teams --------------------

	// ORGANIZATION_RESOURCE is the quay.holos.run Organization the Holos
	// Controller reconciles into the in-cluster Quay registry (creates, does not
	// adopt — spec.adopt: false).  spec.syncedTeams maps the primitive-role OIDC
	// groups (<name>-{owner,editor,viewer}, produced by the role KeycloakGroups
	// below) to Quay teams: owner→admin, editor→creator+write, viewer→member+read
	// (ADR-19 Rev 6).  spec.caBundle carries the per-cluster local-ca PEM (the
	// trust anchor for Quay's mkcert-signed serving cert) only when _CABundlePEM
	// is injected at apply time (scripts/apply-projects); the committed tree omits
	// it (the runtime-secret posture).
	let ORGANIZATION_RESOURCE = {
		apiVersion: "quay.holos.run/v1alpha1"
		kind:       "Organization"
		metadata: {
			name:      NAME
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		spec: {
			name:  NAME
			email: "\(NAME)@holos.internal"
			credentialsSecretRef: name: "holos-controller-quay-creds"
			adopt: false
			syncedTeams: [
				{
					name:      "\(NAME)-owner"
					oidcGroup: CLIENT_ROLE["owner"]
					role:      "admin"
				},
				{
					name:                 "\(NAME)-editor"
					oidcGroup:            CLIENT_ROLE["editor"]
					role:                 "creator"
					repositoryPermission: "write"
				},
				{
					name:                 "\(NAME)-viewer"
					oidcGroup:            CLIENT_ROLE["viewer"]
					role:                 "member"
					repositoryPermission: "read"
				},
			]
			if _CABundlePEM != "" {
				caBundle: base64.Encode(null, _CABundlePEM)
			}
		}
	}

	// --- Kargo control plane -------------------------------------------------

	// PROJECT_RESOURCE is the cluster-scoped Kargo Project (1.10: no spec; it maps
	// NAME to the same-named namespace and adopts it).  Authored as a plain CUE
	// struct (the vendored #Project binding is stale for 1.10's cluster-scoped,
	// spec-less Project — see holos/resources.cue).  NB: the adopted namespace is
	// the bare <name> control namespace, which carries the kargo.akuity.io/project
	// adoption label + keep-namespace annotation (holos/namespaces.cue).
	let PROJECT_RESOURCE = {
		apiVersion: "kargo.akuity.io/v1alpha1"
		kind:       "Project"
		metadata: {
			name: NAME
			labels: "app.kubernetes.io/name": NAME
		}
	}

	// PROJECT_CONFIG_RESOURCE is the namespaced ProjectConfig: the auto-promotion
	// policies (the project-config Stage AND one per contained app's <app>-config
	// Stage) and the native Quay webhook receiver whose secretRef points at the
	// WEBHOOK_SECRET in this same namespace.
	//
	// The per-app auto-promotion policy is REQUIRED for app push-to-deploy to
	// close: the Application component (HOL-1356) emits an <app>-config Kargo Stage
	// per app, but a Stage only auto-promotes discovered Freight when a matching
	// promotionPolicies entry enables it — without one the app Warehouse discovers
	// Freight but argocd-update never runs.  Iterate apps where app.project == NAME
	// (empty for a zero-app project, so the policy list is just the project Stage),
	// adding {stage: <app>-config, autoPromotionEnabled: true} per app.
	let PROJECT_CONFIG_RESOURCE = {
		apiVersion: "kargo.akuity.io/v1alpha1"
		kind:       "ProjectConfig"
		metadata: {
			name:      NAME
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		spec: {
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

	// The Quay webhook receiver Secret is generated at runtime by a create-if-absent
	// Job (never committed — the receiver URL Kargo derives from it would silently
	// invalidate on a rotation).  The token is written under the single `secret`
	// key the Kargo quay receiver reads (verified against the vendored 1.10.3 CRD),
	// piped on stdin so it never appears in argv.  Includes the one-time migration
	// that prunes the legacy duplicate `secret-token` key.
	let WEBHOOK_BOOTSTRAP_SCRIPT = """
		set -eu
		if kubectl -n \(CTRL_NS) get secret \(WEBHOOK_SECRET) >/dev/null 2>&1; then
		  echo "Secret \(WEBHOOK_SECRET) already exists; leaving its generated token untouched."
		  if kubectl -n \(CTRL_NS) get secret \(WEBHOOK_SECRET) -o 'jsonpath={.data.secret-token}' | grep -q .; then
		    kubectl -n \(CTRL_NS) patch secret \(WEBHOOK_SECRET) --type=json -p='[{"op": "remove", "path": "/data/secret-token"}]'
		    echo "Removed legacy secret-token key from \(WEBHOOK_SECRET)."
		  fi
		  exit 0
		fi
		random_key() {
		  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
		}
		TOKEN="$(random_key)"
		[ "${#TOKEN}" -eq 48 ]
		kubectl -n \(CTRL_NS) create -f - <<EOF
		apiVersion: v1
		kind: Secret
		metadata:
		  name: \(WEBHOOK_SECRET)
		  namespace: \(CTRL_NS)
		stringData:
		  secret: "${TOKEN}"
		EOF
		echo "Secret \(WEBHOOK_SECRET) created."
		"""

	let WEBHOOK_BOOTSTRAP_METADATA = {
		name:      WEBHOOK_BOOTSTRAP
		namespace: CTRL_NS
		labels: "app.kubernetes.io/name": WEBHOOK_BOOTSTRAP
	}

	let WEBHOOK_BOOTSTRAP_SERVICE_ACCOUNT = {
		apiVersion: "v1"
		kind:       "ServiceAccount"
		metadata:   WEBHOOK_BOOTSTRAP_METADATA
	}

	// Scoped to the one Secret the Job manages (get/patch by resourceName);
	// create cannot be resourceName-restricted, so it is namespace-wide on
	// secrets (acceptable in a namespace whose Secrets all belong to this project).
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
			namespace: CTRL_NS
		}]
	}

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
						runAsUser:    65534
						runAsGroup:   65534
						seccompProfile: type: "RuntimeDefault"
					}
					containers: [{
						name:  "bootstrap"
						image: KUBECTL_IMAGE
						command: ["/bin/sh", "-c", WEBHOOK_BOOTSTRAP_SCRIPT]
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

	// WAREHOUSE_RESOURCE subscribes to the rendered-manifests OCI artifact (bare
	// registry/repo form, Lexical selection over the render-* tags, skip-TLS for
	// the mkcert-signed in-cluster registry).  See the my-project scaffold for the
	// full strategy rationale.
	let WAREHOUSE_RESOURCE = kargowarehouse.#Warehouse & {
		metadata: {
			name:      WAREHOUSE
			namespace: CTRL_NS
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

	// STAGE_RESOURCE requests Freight from the Warehouse and, on promotion, runs
	// argocd-update to repoint the Application's OCI source at the Freight digest.
	// sources[].repoURL is the oci:// form (byte-identical to the Application
	// source); desiredRevision uses imageFrom(<bare repoURL>).Digest.
	let STAGE_RESOURCE = kargostage.#Stage & {
		metadata: {
			name:      STAGE
			namespace: CTRL_NS
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

	// --- Keycloak data plane (ADR-20) ----------------------------------------

	// KEYCLOAK_CLIENT_RESOURCE is the project's confidential OIDC client.  Its
	// clientRoles declare the <name>-{owner,editor,viewer} roles (clientRef = this
	// client's own metadata.name); the reconciler ensures those roles and the
	// client-role→groups mapper exist, so a role group conferring one surfaces it
	// in this client's token (the ADR-20 "project's own service" path).  secretRef
	// is required for a confidential client (the CRD's CEL rule).
	let KEYCLOAK_CLIENT_RESOURCE = {
		apiVersion: "keycloak.holos.run/v1alpha1"
		kind:       "KeycloakClient"
		metadata: {
			name:      PROJECT_CLIENT_NAME
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		spec: {
			clientId:    PROJECT_CLIENT_ID
			type:        "confidential"
			instanceRef: INSTANCE_REF
			redirectUris: ["\(PROJECT_CLIENT_ID)/oauth2/callback"]
			webOrigins: [PROJECT_CLIENT_ID]
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
	// projects/<name>/roles/{owner,editor,viewer}.  Each confers <name>-<role> on
	// TWO clients and delegates membership management to the same-tier custodian:
	//
	//   - the platform Quay client (clientId: https://quay.holos.internal) — the
	//     ADR-20 Rev 4 Quay use case.  This direct-clientId path requires the CR's
	//     namespace to equal the bare project name <name> and the role to be
	//     exactly <name>-<role> (validateDirectClientRole); both hold because the
	//     control namespace IS the bare <name> (the control-namespace resolution
	//     in the file header).  Quay's quay-client-roles mapper then folds
	//     <name>-<role> into Quay's groups claim, populating the Organization's
	//     syncedTeams[].oidcGroup membership.
	//   - the project's own client (clientRef: <name>), so the role also reaches
	//     https://<name>.holos.internal's token.
	//   - EACH app contained by this project (apps where app.project == NAME): the
	//     `for app in apps` comprehension below appends a clientRoles entry
	//     {clientRef: <app-name>, role: <leaf>} per app, so the same project role
	//     also confers the matching role on every app's KeycloakClient (the
	//     Application component, HOL-1356, emits those app clients and defines the
	//     owner/editor/viewer roles on them).  With ZERO apps the comprehension
	//     contributes nothing and the role group confers only the Quay + project
	//     clients (the HOL-1355 zero-app behavior); each registered app adds its
	//     own clientRef entry.  The clientRef path is a SAME-NAMESPACE KeycloakClient
	//     resolution (NOT subject to validateDirectClientRole), which holds because
	//     the Application component places each app's KeycloakClient in this same
	//     bare <name> control namespace.  app.name (the leaf role) maps 1:1 to the
	//     app's owner/editor/viewer client roles.
	let KEYCLOAK_ROLE_GROUP_RESOURCES = {
		for r in ROLES {
			(r): {
				apiVersion: "keycloak.holos.run/v1alpha1"
				kind:       "KeycloakGroup"
				metadata: {
					name:      "\(NAME)-roles-\(r)"
					namespace: CTRL_NS
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
						// One entry per app contained by this project (empty for a
						// zero-app project).  The app's KeycloakClient CR metadata.name
						// is the app name (the Application component), resolved
						// same-namespace via clientRef; the app's matching client role
						// is named r (owner/editor/viewer, 1:1 with the primitive role).
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
	// projects/<name>/custodians/{owner,editor,viewer} the role groups delegate
	// membership management to (ADR-3's custodian model).  No client role, no
	// further custodian.
	let KEYCLOAK_CUSTODIAN_GROUP_RESOURCES = {
		for r in ROLES {
			(r): {
				apiVersion: "keycloak.holos.run/v1alpha1"
				kind:       "KeycloakGroup"
				metadata: {
					name:      "\(NAME)-custodians-\(r)"
					namespace: CTRL_NS
					labels: "app.kubernetes.io/name": NAME
				}
				spec: {
					path:        "projects/\(NAME)/custodians/\(r)"
					instanceRef: INSTANCE_REF
				}
			}
		}
	}

	// KEYCLOAK_USER_RESOURCES pre-provisions one KeycloakUser per project owner
	// (projects.<name>.owners), each joined to the owner role group with the IdP
	// federated-identity link so first federated login auto-links the pre-created
	// record (paired with the realm's first-broker-login auto-link flow).  The CR
	// metadata.name must be a DNS label, but an email is not — derive a stable
	// DNS-safe name from the email's local part (lowercased; @, ., + and any other
	// non-label char collapsed to "-"; bounded to a valid label by #DNSLabel via
	// the metadata.name validation).  Keyed by email so distinct owners never
	// collide.
	let KEYCLOAK_USER_RESOURCES = {
		for EMAIL, _ in OWNERS {
			(EMAIL): {
				apiVersion: "keycloak.holos.run/v1alpha1"
				kind:       "KeycloakUser"
				// _userName is the DNS-safe, COLLISION-RESISTANT, LENGTH-BOUNDED
				// metadata.name derived from the email.  A pure sanitize-the-email
				// scheme is neither: alice.foo@ and alice+foo@ both normalize to
				// "alice-foo-example-com" (a render conflict), and a long-but-valid
				// email overflows the 63-char DNS-label limit.  So the name is a
				// readable prefix (the email local part, lowercased, non-[a-z0-9] runs
				// collapsed to "-", trimmed, and truncated to ≤40 chars) plus a "-"
				// and the first 8 hex chars of sha256(EMAIL) — a deterministic suffix
				// that distinguishes any two distinct emails and keeps the whole name
				// ≤49 chars (well under 63).  The email itself is carried in
				// spec.email; this is only the object name.
				let _local = regexp.ReplaceAll("^-+|-+$", strings.ToLower(regexp.ReplaceAll("[^a-z0-9]+", strings.SplitN(EMAIL, "@", 2)[0], "-")), "")
				// Truncate the readable prefix to ≤40 chars; for an empty/all-symbol
				// local part fall back to "user" so the name still starts with a
				// letter (DNS-label valid).
				let _prefix = [
					if (regexp.FindSubmatch("^([a-z0-9][a-z0-9-]{0,39})", _local) != _|_) {regexp.FindSubmatch("^([a-z0-9][a-z0-9-]{0,39})", _local)[1]},
					"user",
				][0]
				let _hash8 = regexp.FindSubmatch("^(.{8})", hex.Encode(sha256.Sum256(EMAIL)))[1]
				let _userName = "\(_prefix)-\(_hash8)"
				metadata: {
					name:      _userName & =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"
					namespace: CTRL_NS
					labels: "app.kubernetes.io/name": NAME
				}
				spec: {
					email:       EMAIL
					instanceRef: INSTANCE_REF
					groups: ["projects/\(NAME)/roles/owner"]
					identityProviderLink: alias: "esso"
					// adopt: true — a Keycloak user is realm-GLOBAL by email, but
					// these KeycloakUser CRs are per-PROJECT (each project emits one
					// for its owner).  The same human owning two projects yields two
					// CRs for the same spec.email; with the default adopt: false the
					// second would hit Ready=False (reason Conflict) and fail
					// scripts/apply-projects — a normal multi-project ownership case.
					// adopt: true lets each project's CR converge the shared realm
					// user (same email → same user) without seizing-and-deleting it:
					// an adopted user is RELEASED, never deleted, on CR removal
					// (api/keycloak/v1alpha1 KeycloakUserSpec.Adopt), so two projects
					// adopting one owner is benign and non-destructive.  The group
					// memberships are additive (each CR adds its own
					// projects/<name>/roles/owner), so a shared owner ends up in both
					// projects' owner groups, which is the intended cross-project
					// ownership semantics.
					adopt: true
				}
			}
		}
	}

	// --- Owner access: a namespaced RoleBinding (ADR-3) ----------------------

	// OWNER_ACCESS_BINDING grants the project owners Kubernetes access in the
	// control namespace (ADR-3 group-membership access control): the subject is
	// the owner role's OIDC GROUP (the Keycloak group path projects/<name>/roles/
	// owner the realm's group-membership mapper emits into the token), bound to
	// the built-in namespace-admin ClusterRole via a RoleBinding (so it is scoped
	// to this namespace only).  Group-membership-driven, not per-user, so adding
	// an owner to the role group grants access without editing RBAC.
	let OWNER_ACCESS_BINDING = {
		apiVersion: "rbac.authorization.k8s.io/v1"
		kind:       "RoleBinding"
		metadata: {
			name:      "\(NAME)-owners"
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		roleRef: {
			apiGroup: "rbac.authorization.k8s.io"
			kind:     "ClusterRole"
			name:     "admin"
		}
		subjects: [{
			apiGroup: "rbac.authorization.k8s.io"
			kind:     "Group"
			name:     "projects/\(NAME)/roles/owner"
		}]
	}

	// The #Resources-shaped output: Kind → name → resource.  Per-app routing
	// (HTTPRoute) and the Gateway-API ReferenceGrant are DELIBERATELY NOT emitted
	// here — they are app-scoped (they need an app's Service/host/port) and belong
	// to the Application component (HOL-1356).  A project with zero apps exposes no
	// route, so for a standalone Project this is a documented no-op (ADR-21: "may
	// be a no-op for a zero-app project").
	resources: #Resources & {
		AppProject: (NAME):     APPPROJECT_RESOURCE
		Application: (NAME):    APPLICATION_RESOURCE
		Organization: (NAME):   ORGANIZATION_RESOURCE
		Project: (NAME):        PROJECT_RESOURCE
		ProjectConfig: (NAME):  PROJECT_CONFIG_RESOURCE
		Warehouse: (WAREHOUSE): WAREHOUSE_RESOURCE
		Stage: (STAGE):         STAGE_RESOURCE

		KeycloakClient: (PROJECT_CLIENT_NAME): KEYCLOAK_CLIENT_RESOURCE
		KeycloakUser: {
			for EMAIL, U in KEYCLOAK_USER_RESOURCES {
				(U.metadata.name): U
			}
		}
		KeycloakGroup: {
			for r in ROLES {
				(KEYCLOAK_ROLE_GROUP_RESOURCES[r].metadata.name):      KEYCLOAK_ROLE_GROUP_RESOURCES[r]
				(KEYCLOAK_CUSTODIAN_GROUP_RESOURCES[r].metadata.name): KEYCLOAK_CUSTODIAN_GROUP_RESOURCES[r]
			}
		}

		ServiceAccount: (WEBHOOK_BOOTSTRAP): WEBHOOK_BOOTSTRAP_SERVICE_ACCOUNT
		Role: (WEBHOOK_BOOTSTRAP):           WEBHOOK_BOOTSTRAP_ROLE
		RoleBinding: {
			(WEBHOOK_BOOTSTRAP):              WEBHOOK_BOOTSTRAP_ROLE_BINDING
			(OWNER_ACCESS_BINDING.metadata.name): OWNER_ACCESS_BINDING
		}
		Job: (WEBHOOK_BOOTSTRAP): WEBHOOK_BOOTSTRAP_JOB
	}
}

userDefinedBuildPlan: {
	metadata: name: "project"
	// One artifact directory per project (clusters/<cluster>/components/project/<name>/),
	// iterating the projects collection.  As of HOL-1357 this includes my-project
	// (the bespoke holos/components/my-project component was deleted and this
	// generalized component now produces the reference instance's project-level
	// resource set); every registered project renders its own subdirectory.
	spec: artifacts: manifests: {
		for PROJECT, P in projects {
			"clusters/\(clusterName)/components/project/\(PROJECT)": {
				artifact: _
				generators: [{
					kind: "Resources"
					// The generator + transformer output filenames are written into
					// the component's SHARED BuildContext.tempDir, so every artifact
					// entry in this multi-project component must use a DISTINCT
					// filename — a bare "resources.gen.yaml" reused across projects
					// fails render with "resources.gen.yaml already set".  Scope each
					// to the project name (PROJECT is DNS-label-bounded, so it is a
					// safe filename segment).
					output: "resources-\(PROJECT).gen.yaml"
					resources: (#ProjectResources & {
						NAME:   PROJECT
						OWNERS: P.owners
					}).resources
				}]
				transformers: [
					{
						kind: "Kustomize"
						inputs: [for G in generators {G.output}]
						output: "kustomize-output-bundle-\(PROJECT).yaml"
						kustomize: kustomization: resources: inputs
					},
					{
						kind: "Command"
						inputs: [transformers[0].output]
						output: artifact
						command: args: ["holos", "kubectl-slice", "-f", "\(BuildContext.tempDir)/\(inputs[0])", "-o", "\(BuildContext.tempDir)/\(artifact)"]
					},
				]
			}
		}
	}
}

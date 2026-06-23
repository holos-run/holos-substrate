package holos

import (
	"encoding/base64"

	kargowarehouse "kargo.akuity.io/warehouse/v1alpha1"
	kargostage "kargo.akuity.io/stage/v1alpha1"
)

// application is the collection-driven component that renders, FOR EACH
// apps.<name> entry (the `apps` collection, holos/collections.cue, HOL-1354),
// the full application-level resource set contained by the app's Project
// (ADR-21 *The Application component*).  An Application is, in its simplest
// form, an OCI image run as a k8s Deployment fronted by a Service and exposed
// via a Gateway-API HTTPRoute on the shared *.holos.internal wildcard
// certificate, delivered by Kargo + Argo CD.  But its PRIMARY PURPOSE is
// identity: it manages a keycloak.holos.run KeycloakClient for the app and maps
// the project's primitive roles (owner/editor/viewer) onto matching app client
// roles, so a project member's token for the app client carries the application
// role.  A Project supports ZERO-to-MANY apps; this component renders nothing
// when no apps are registered (the deploy tree stays diff-clean) and one app's
// resource set per apps.<name> when they are.  Read the echo component
// (the canonical Deployment/Service/HTTPRoute OCI app shape), the project
// component (HOL-1355, the per-project control plane this extends), and ADR-21
// alongside this file.
//
// --- NAMESPACE: the app lives in its project's BARE control namespace --------
//
// Every resource this component emits for an app lands in the app's project's
// BARE control namespace <project> (NAME & #RegisteredNamespace below) — the
// same namespace the Project component (HOL-1355) places the project's role
// KeycloakGroups, project KeycloakClient, Quay Organization, and Kargo control
// plane in.  Two constraints force this single namespace:
//
//   - The role-group → app-client-role binding uses the KeycloakGroup
//     clientRoles[].clientRef path (a SAME-NAMESPACE KeycloakClient resolution;
//     internal/controller/keycloak/group_controller.go).  The project's role
//     KeycloakGroups live in the bare <project> namespace (the
//     validateDirectClientRole guard forces it for their Quay direct-clientId
//     path — see the project component's control-namespace resolution), so the
//     app's KeycloakClient MUST live in that same <project> namespace for the
//     clientRef to resolve.  (The clientRef path is NOT subject to
//     validateDirectClientRole; same-namespace resolution is its only
//     constraint.)
//   - The Project component's Argo CD Application destination is the bare
//     <project> control namespace, and the project namespace doubles as the
//     workload namespace (the my-project topology HOL-1355 generalizes).  So the
//     app's Deployment/Service/HTTPRoute deploy into <project> too — the single
//     wired delivery environment for this foundational phase.  ADR-21's
//     env-prefixed (ci-/qa-/prod-<name>) multi-environment delivery is a future
//     extension; this phase wires one environment, the bare control namespace,
//     consistent with the Project component (a Deferred AC records the ADR-21
//     ratification, HOL-1358).
//
// --- APP CLIENT ROLES: owner/editor/viewer, 1:1 from the project primitives ---
//
// The app's KeycloakClient defines client roles named owner/editor/viewer — the
// same names as the project's primitive roles, the simplest faithful mapping.
// The per-client oidc-usermodel-client-role-mapper the KeycloakClient reconciler
// ensures (internal/controller/keycloak/client_controller.go) emits an assigned
// role into the shared groups claim — NO new mapper or Quay-side change is
// needed (ADR-20 *Claim value via a client role*).  Each project role group
// projects/<project>/roles/<leaf> confers the matching app role <leaf> on the
// app client via a clientRoles entry {clientRef: <app-client-CR-name>, role:
// <leaf>}; that contribution is ADDED to the project role groups by the Project
// component's `for app in apps where app.project == <project>` comprehension
// (HOL-1355's role groups, extended in this same change), so a project with zero
// apps confers only the Quay client role and each registered app additionally
// confers its app roles.  App-SPECIFIC role vocabularies (roles other than the
// owner/editor/viewer triad) are a future extension; the binding lives on the
// project role KeycloakGroup regardless (ADR-20).

// ROLES is the GCP-style primitive role triad, shared with the Project
// component: the app client defines a role per entry and the project role groups
// confer the matching one.
let ROLES = ["owner", "editor", "viewer"]

// KEYCLOAK_NAMESPACE / KEYCLOAK_INSTANCE identify the central KeycloakInstance
// the app's KeycloakClient references cross-namespace (ReferenceGrant-gated by
// the keycloak-instance component, not re-emitted here) — the same instance the
// Project component references.
let KEYCLOAK_NAMESPACE = "keycloak" & #RegisteredNamespace
let KEYCLOAK_INSTANCE = "holos-keycloak"

// ArgoCDNamespace is this platform's Argo CD namespace; the app's AppProject and
// Application are namespaced into it, like the project-level ones.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// #AppResources derives, for ONE app (name NAME, project PROJECT, image IMAGE,
// port PORT, optional host HOST), the full application-level resource set as a
// #Resources-shaped struct.  Factored as a definition so the per-app artifact
// comprehension below stays legible, mirroring the project component's
// #ProjectResources.
#AppResources: {
	// NAME is the app name (an apps map key, DNS-label validated by the
	// collection schema).  It is the base of every resource name.
	NAME: string

	// PROJECT is the project the app is contained by (apps.<name>.project, a
	// projects key, validated by the collection's #RegisteredProject reference).
	PROJECT: string

	// IMAGE is the container image the app's Deployment runs (apps.<name>.image,
	// a digest-pinned reference per the publish posture).  The image is per-APP, so
	// it is carried on the app's REGISTRATION (the apps.<name>.image field), NOT the
	// single platform-wide _AppImage tag (which can pin only ONE image and serves
	// the echo sample) — a one-image tag cannot parameterize many apps.  The
	// Deployment that runs this image is delivered to the cluster by the app's
	// Argo CD Application from the published <app>-config artifact (NOT by
	// scripts/apply-projects, which applies only the control plane — see the apply
	// script), so updating the running image is a publish of a new <app>-config
	// artifact built from an updated apps.<name>.image, the same render→publish→
	// promote loop scripts/publish drives for echo/my-project.  Per-app digest
	// injection at publish time is a publish-workflow extension; the registration's
	// digest-pinned image is the foundational-phase mechanism.
	IMAGE: string

	// PORT is the container port the app listens on (apps.<name>.port); the
	// Service and HTTPRoute target it.
	PORT: int

	// HOST is the OPTIONAL external hostname the HTTPRoute exposes the app at
	// (apps.<name>.host); when unset it defaults to <name>.holos.internal (the
	// convention; per-env hostname selection is a future extension).
	HOST: string | *"\(NAME).holos.internal"

	// An app MUST NOT be named the same as its project.  Every app resource here
	// is named with the bare app name in the project's control namespace, and the
	// PROJECT component names its own KeycloakClient / Warehouse / Argo CD
	// Application with the bare PROJECT name in that SAME namespace — so an app
	// named <project> would collide with the project's own client/warehouse/
	// application (a silent apply/reconcile-time failure).  Reject it at RENDER:
	// NAME unified with "not equal to PROJECT" is bottom when they match.
	NAME: !=PROJECT

	// An app MUST NOT be named "project" either.  The app's Kargo Stage is named
	// <name>-config, but the PROJECT component's project-level Stage is the fixed
	// name "project-config" in the SAME control namespace — so an app named exactly
	// "project" would render Stage "project-config" and COLLIDE with the project's
	// promotion Stage (which NAME != PROJECT does not catch when the project is not
	// itself named "project").  "project" is therefore a reserved app name; reject
	// it at RENDER.  (The project-level Stage is the fixed name "project-config" in
	// the same control namespace, so this reserved name is the standing guard.)
	NAME: !="project"

	// An app MUST NOT be named "control-plane" or "workload" either.  The per-app
	// Argo CD Application is named <project>-<app> in the SHARED argocd namespace,
	// and the per-project App-of-Apps roots (components/project-app-of-apps,
	// HOL-1382) are named <project>-control-plane and <project>-workload there — so
	// an app named "control-plane" or "workload" would render an Application
	// <project>-control-plane / <project>-workload that COLLIDES with the project's
	// own App-of-Apps root.  Both are therefore reserved app names; reject them at
	// RENDER.
	NAME: !="control-plane"
	NAME: !="workload"

	// CTRL_NS is the app's project's BARE control namespace (see the namespace
	// note in the file header): every app resource lands here.  Unified with
	// #RegisteredNamespace so a missing registry entry fails at render, not apply.
	let CTRL_NS = PROJECT & #RegisteredNamespace

	// METADATA is the shared object meta for the app's workload resources.
	let METADATA = {
		name:      NAME
		namespace: CTRL_NS
		labels: "app.kubernetes.io/name": NAME
	}

	// INSTANCE_REF is the cross-namespace reference the app's KeycloakClient
	// carries (ReferenceGrant-gated; namespace differs from the control ns).
	let INSTANCE_REF = {
		name:      KEYCLOAK_INSTANCE
		namespace: KEYCLOAK_NAMESPACE
	}

	// APP_CLIENT_NAME is the app KeycloakClient CR's metadata.name (the object
	// name the project role groups' clientRoles[].clientRef resolves — NOT the
	// URL clientId).  Namespaced under the app name so it never collides with the
	// project's own client (named <project>) in the shared control namespace.
	// APP_CLIENT_ID is its URL-shaped clientId.
	let APP_CLIENT_NAME = NAME
	let APP_CLIENT_ID = "https://\(HOST)"

	// APP_CLIENT_SECRET is where the controller delivers the confidential
	// client's generated secret (generate-once, in the control namespace, never
	// committed — the Runtime Secret Handling guardrail).
	let APP_CLIENT_SECRET = "\(NAME)-oidc"

	// CONFIG_REPO_NAME is the app's rendered-manifests artifact repository NAME
	// within the project's Quay org: <app>-config.  It is the repo the publish
	// workflow pushes the config artifact to, the Warehouse watches, the Argo CD
	// Application pulls from, AND the Quay Repository CR below manages (so the
	// managed repo, its repo_push webhook, the Warehouse subscription, and the
	// Application source are all the SAME repo — they must not drift).
	let CONFIG_REPO_NAME = "\(NAME)-config"

	// CONFIG_REPO is the full registry/org/repo path; CONFIG_REPO_OCI is its oci://
	// form, which must stay byte-identical between the Application source and the
	// Stage's argocd-update source (Kargo matches by exact string).
	let CONFIG_REPO = "quay.holos.internal/\(PROJECT)/\(CONFIG_REPO_NAME)"
	let CONFIG_REPO_OCI = "oci://\(CONFIG_REPO)"

	// CONFIG_TAG_REGEX scopes the Warehouse subscription to the input-addressed
	// render-<config12>-<appimage12> tags scripts/publish mints (the same regex
	// the project component uses).
	let CONFIG_TAG_REGEX = "^render-[0-9a-f]{12}-[0-9a-f]{12}$"

	// STAGE is the Kargo Stage authorized to patch the app Application's
	// targetRevision; WAREHOUSE is the Warehouse the Stage requests Freight from.
	// Both are NAMESPACED into the project control namespace, so the bare app name
	// is unique enough there (an app name is unique within its project).
	let STAGE = "\(NAME)-config"
	let WAREHOUSE = NAME

	// ARGOCD_APP_NAME is the app's Argo CD Application metadata.name.  Unlike the
	// namespaced Warehouse/Stage/Repository, the Argo CD Application lives in the
	// SHARED argocd namespace alongside every project's and app's Application — so
	// a bare app name would collide across projects (e.g. an app named "my-project"
	// in some other project would clash with project my-project's own Application
	// named "my-project").  Prefix with the project to make it GLOBALLY unique:
	// project names are unique and an app name is unique within its project, so
	// <project>-<app> is unique across the whole platform.  The project component's
	// own Application is the bare <project>, which never equals <project>-<app>
	// (the "-" separator and non-empty app name guarantee it).
	let ARGOCD_APP_NAME = "\(PROJECT)-\(NAME)"

	// --- Workload: Deployment + Service (the echo OCI app shape) -------------

	// DEPLOYMENT_RESOURCE runs the app's OCI image with the conventional
	// hardened pod posture from the echo component (runAsNonRoot, dropped
	// capabilities, read-only rootfs, a QoS floor, and HTTP probes on the app
	// port).  It mounts the non-secret ConfigMap below at /etc/config.
	let DEPLOYMENT_RESOURCE = {
		apiVersion: "apps/v1"
		kind:       "Deployment"
		metadata:   METADATA
		spec: {
			replicas: 1
			selector: matchLabels: METADATA.labels
			template: {
				metadata: labels: METADATA.labels
				spec: {
					serviceAccountName: NAME
					securityContext: {
						runAsNonRoot: true
						runAsUser:    65534
						runAsGroup:   65534
						seccompProfile: type: "RuntimeDefault"
					}
					containers: [{
						name:  NAME
						image: IMAGE
						ports: [{
							name:          "http"
							containerPort: PORT
							protocol:      "TCP"
						}]
						readinessProbe: httpGet: {
							path: "/healthz"
							port: PORT
						}
						livenessProbe: httpGet: {
							path: "/healthz"
							port: PORT
						}
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
							name:      "config"
							mountPath: "/etc/config"
							readOnly:  true
						}]
					}]
					volumes: [{
						name: "config"
						configMap: name: NAME
					}]
				}
			}
		}
	}

	let SERVICE_RESOURCE = {
		apiVersion: "v1"
		kind:       "Service"
		metadata:   METADATA
		spec: {
			selector: METADATA.labels
			ports: [{
				name:       "http"
				port:       PORT
				targetPort: PORT
				protocol:   "TCP"
			}]
		}
	}

	// --- Ingress: an HTTPRoute on the shared Gateway -------------------------

	// HTTPROUTE_RESOURCE attaches to the shared `default` Gateway in
	// istio-gateways (cross-namespace attachment is allowed because the listener
	// sets allowedRoutes.namespaces.from: All — istio-gateway component).  TLS is
	// the shared *.holos.internal wildcard certificate at the Gateway listener;
	// the app references NO cert of its own.  HOST (default <name>.holos.internal)
	// matches the wildcard listener hostname and resolves to 127.0.0.1 on the host
	// per docs/local-cluster.md.
	let HTTPROUTE_RESOURCE = {
		apiVersion: "gateway.networking.k8s.io/v1"
		kind:       "HTTPRoute"
		metadata:   METADATA
		spec: {
			parentRefs: [{
				name:      "default"
				namespace: "istio-gateways"
			}]
			hostnames: [HOST]
			rules: [{
				matches: [{path: {type: "PathPrefix", value: "/"}}]
				backendRefs: [{
					name: NAME
					port: PORT
				}]
			}]
		}
	}

	// --- Workload supporting objects: SA + RoleBinding + ConfigMap -----------

	// SERVICE_ACCOUNT_RESOURCE is the app's workload identity.  The
	// ExternalSecret for app secret material is DELIBERATELY NOT emitted: the
	// external-secrets store prerequisite has not landed (ADR-21), and a dangling
	// ExternalSecret with no SecretStore would never sync — so it is deferred, not
	// emitted empty.
	let SERVICE_ACCOUNT_RESOURCE = {
		apiVersion: "v1"
		kind:       "ServiceAccount"
		metadata:   METADATA
	}

	// ROLE_BINDING_RESOURCE binds the app ServiceAccount to the built-in `view`
	// ClusterRole in the control namespace (a minimal, read-only namespace grant
	// — the app reads its own namespace's objects, never mutates them).  Scoped
	// to this namespace via a RoleBinding (not a ClusterRoleBinding).
	let ROLE_BINDING_RESOURCE = {
		apiVersion: "rbac.authorization.k8s.io/v1"
		kind:       "RoleBinding"
		metadata: {
			name:      "\(NAME)-view"
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		roleRef: {
			apiGroup: "rbac.authorization.k8s.io"
			kind:     "ClusterRole"
			name:     "view"
		}
		subjects: [{
			kind:      "ServiceAccount"
			name:      NAME
			namespace: CTRL_NS
		}]
	}

	// CONFIG_MAP_RESOURCE carries the app's NON-SECRET configuration (mounted at
	// /etc/config).  Secret material is the deferred ExternalSecret's concern (see
	// SERVICE_ACCOUNT_RESOURCE above), not this ConfigMap.
	let CONFIG_MAP_RESOURCE = {
		apiVersion: "v1"
		kind:       "ConfigMap"
		metadata:   METADATA
		data: {
			"app.name":    NAME
			"app.project": PROJECT
			"app.host":    HOST
			"app.port":    "\(PORT)"
		}
	}

	// --- Keycloak client + role mapping (the primary purpose) ----------------

	// KEYCLOAK_CLIENT_RESOURCE is the app's confidential OIDC client.  Its
	// clientRoles declare the owner/editor/viewer roles (clientRef = this client's
	// own metadata.name, per the CRD's CEL rule forbidding clientId on a client's
	// own roles).  The reconciler ensures those roles and the
	// oidc-usermodel-client-role-mapper exist, so a project role group conferring
	// one (via clientRef to this client, added on the project role groups by the
	// Project component) surfaces it in this client's token.  secretRef is
	// required for a confidential client.
	let KEYCLOAK_CLIENT_RESOURCE = {
		apiVersion: "keycloak.holos.run/v1alpha1"
		kind:       "KeycloakClient"
		metadata: {
			name:      APP_CLIENT_NAME
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		spec: {
			clientId:    APP_CLIENT_ID
			type:        "confidential"
			instanceRef: INSTANCE_REF
			redirectUris: ["\(APP_CLIENT_ID)/oauth2/callback"]
			webOrigins: [APP_CLIENT_ID]
			clientRoles: [
				for r in ROLES {
					clientRef: APP_CLIENT_NAME
					role:      r
				},
			]
			secretRef: {
				name: APP_CLIENT_SECRET
				key:  "client_secret"
			}
		}
	}

	// --- Quay data plane: the app's Repository -------------------------------

	// REPOSITORY_RESOURCE is the quay.holos.run Repository within the project's
	// Quay org (organizationRef = the project Organization CR named <project> in
	// this same namespace — emitted by the Project component).  The Holos
	// Controller reconciles it into the in-cluster Quay registry.  spec.caBundle
	// carries the per-cluster local-ca PEM only when _CABundlePEM is injected at
	// apply time (scripts/apply-projects), like the Organization.
	//
	// The repo_push webhook (AC: "OPTIONAL repo_push webhook") is DELIBERATELY NOT
	// emitted in this foundational phase.  Kargo exposes the receiver URL in the
	// project ProjectConfig's status (status.webhookReceivers[].url), NOT in the
	// project's generated webhook Secret (the project's bootstrap Job writes only
	// the receiver TOKEN under key `secret`, never the URL) — and no copier yet
	// lands that status URL into a Secret key the Repository's urlSecretRef could
	// read.  A webhook urlSecretRef pointing at a non-existent `url` key would make
	// every app Repository fail webhook registration (WebhookURLNotFound), so the
	// webhook is omitted until that receiver-URL plumbing exists (a Project-
	// component concern).  Delivery still works WITHOUT the webhook: the app
	// Warehouse polls the config repo every minute (WAREHOUSE_RESOURCE interval),
	// the ADR-21-documented fallback.  Wiring the webhook is a follow-up once the
	// receiver URL is published into a Secret key.
	let REPOSITORY_RESOURCE = {
		apiVersion: "quay.holos.run/v1alpha1"
		kind:       "Repository"
		metadata: {
			// The CR is named for the repo it manages — the <app>-config artifact
			// repo, NOT the bare app name — so it never collides with another app
			// resource and so the managed Quay repo is unambiguously the config
			// repo.
			name:      CONFIG_REPO_NAME
			namespace: CTRL_NS
			labels: "app.kubernetes.io/name": NAME
		}
		spec: {
			organizationRef: PROJECT
			// spec.name is CONFIG_REPO_NAME (<app>-config): the SAME repo the
			// Warehouse subscribes to, the Argo CD Application pulls from, and
			// scripts/publish pushes the rendered config artifact to.  Managing the
			// bare <app> repo instead would leave the actual delivery repo
			// unmanaged (the codex round-1 finding).
			name:       CONFIG_REPO_NAME
			visibility: "private"
			credentialsSecretRef: name: "holos-controller-quay-creds"
			if _CABundlePEM != "" {
				caBundle: base64.Encode(null, _CABundlePEM)
			}
		}
	}

	// --- Kargo + Argo CD delivery --------------------------------------------

	// WAREHOUSE_RESOURCE subscribes to the app's rendered-manifests OCI artifact
	// (Lexical selection over the render-* tags, skip-TLS for the mkcert-signed
	// in-cluster registry), mirroring the project component's Warehouse.
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

	// STAGE_RESOURCE requests Freight from the app Warehouse and, on promotion,
	// runs argocd-update to repoint the app Application's OCI source at the
	// Freight digest.  sources[].repoURL is the oci:// form (byte-identical to the
	// Application source); desiredRevision uses imageFrom(<bare repoURL>).Digest.
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
						name:      ARGOCD_APP_NAME
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

	// APPLICATION_RESOURCE is the app's Argo CD Application Kargo patches.
	// targetRevision is DELIBERATELY OMITTED so Kargo solely owns it; the
	// authorized-stage annotation authorizes the app's <name>-config Stage to
	// modify it.  destination is the app's project control namespace.  It runs in
	// the project's AppProject (the Project component constrains that AppProject's
	// sourceRepos to the project's Quay org OCI path, which the app's
	// <name>-config repo is under).  metadata.name is the GLOBALLY-unique
	// <project>-<app> (ARGOCD_APP_NAME) because the argocd namespace is shared
	// across all projects/apps; a bare app name would collide cross-project.
	let APPLICATION_RESOURCE = {
		apiVersion: "argoproj.io/v1alpha1"
		kind:       "Application"
		metadata: {
			name:      ARGOCD_APP_NAME
			namespace: ArgoCDNamespace
			labels: "app.kubernetes.io/name":                NAME
			annotations: "kargo.akuity.io/authorized-stage": "\(CTRL_NS):\(STAGE)"
		}
		spec: {
			project: PROJECT
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

	// --- Two SEPARABLE bundles: workload (Argo-CD-synced) vs control plane ----
	//
	// The app's resources split by DELIVERY PATH, and the two bundles MUST render
	// into separate artifact directories so they are independently packageable:
	//
	//   - workloadResources — the Deployment/Service/HTTPRoute/ConfigMap/
	//     ServiceAccount/RoleBinding the app's Argo CD Application SYNCS from the
	//     published <app>-config OCI artifact.  scripts/publish packages ONLY this
	//     bundle as <app>-config, and the Application's source.path is "." (the
	//     workload bundle is the artifact root).
	//   - controlPlaneResources — the Quay Repository, the app KeycloakClient, the
	//     Kargo Warehouse/Stage, and the Argo CD Application ITSELF, applied by the
	//     operator path (scripts/apply-projects).  These must NOT be in the
	//     <app>-config artifact: if Argo CD synced them it would try to MANAGE the
	//     Repository/KeycloakClient/Warehouse/Stage and the Application object that
	//     points at itself — the second-manager / sync-scope problem (the codex
	//     round-3 confirming finding).  Keeping them in a SEPARATE artifact
	//     directory the publish workflow never packages is the fix.
	workloadResources: #Resources & {
		Deployment: (NAME):     DEPLOYMENT_RESOURCE
		Service: (NAME):        SERVICE_RESOURCE
		HTTPRoute: (NAME):      HTTPROUTE_RESOURCE
		ConfigMap: (NAME):      CONFIG_MAP_RESOURCE
		ServiceAccount: (NAME): SERVICE_ACCOUNT_RESOURCE
		RoleBinding: (ROLE_BINDING_RESOURCE.metadata.name): ROLE_BINDING_RESOURCE
	}

	controlPlaneResources: #Resources & {
		KeycloakClient: (APP_CLIENT_NAME): KEYCLOAK_CLIENT_RESOURCE
		Repository: (CONFIG_REPO_NAME):    REPOSITORY_RESOURCE
		Warehouse: (WAREHOUSE):            WAREHOUSE_RESOURCE
		Stage: (STAGE):                    STAGE_RESOURCE
		Application: (ARGOCD_APP_NAME):    APPLICATION_RESOURCE
	}
}

userDefinedBuildPlan: {
	metadata: name: "application"
	// TWO artifact directories per app, iterating the apps collection:
	//
	//   clusters/<cluster>/components/application/<app>/workload/       — the
	//     Argo-CD-synced workload bundle (Deployment/Service/HTTPRoute/ConfigMap/
	//     ServiceAccount/RoleBinding).  scripts/publish packages THIS subtree as
	//     the <app>-config OCI artifact the app's Argo CD Application syncs.
	//   clusters/<cluster>/components/application/<app>/control-plane/  — the
	//     operator-applied control plane (Repository/KeycloakClient/Warehouse/
	//     Stage/Application).  scripts/apply-projects applies THIS subtree; it is
	//     NEVER published into the <app>-config artifact (else Argo CD would try to
	//     manage these objects — the second-manager problem).
	//
	// Splitting the bundles into separate directories is what keeps Argo CD's sync
	// scope to the workload only.  A project with zero apps yields no apps entries,
	// so this renders no artifacts and the deploy tree stays diff-clean; each
	// registered app adds two artifact directories.
	spec: artifacts: manifests: {
		for APP, A in apps {
			let APP_RESOURCES = #AppResources & {
				NAME:    APP
				PROJECT: A.project
				IMAGE:   A.image
				PORT:    A.port
				if A.host != _|_ {
					HOST: A.host
				}
			}

			// #AppBundle renders one named bundle of an app's resources into its own
			// artifact directory under application/<app>/<bundle>/.  Factored so the
			// workload and control-plane bundles share the identical generator/
			// transformer shape and differ only by name and resource set.  Every
			// generator/transformer output filename is scoped by APP and BUNDLE so
			// the two bundles (and the multi-app iteration) never reuse a filename in
			// the component's shared tempDir (the "already set" trap).
			let mkBundle = {
				BUNDLE:    string
				RESOURCES: _
				out: {
					"clusters/\(clusterName)/components/application/\(APP)/\(BUNDLE)": {
						artifact: _
						generators: [{
							kind:      "Resources"
							output:    "resources-\(APP)-\(BUNDLE).gen.yaml"
							resources: RESOURCES
						}]
						transformers: [
							{
								kind: "Kustomize"
								inputs: [for G in generators {G.output}]
								output: "kustomize-output-bundle-\(APP)-\(BUNDLE).yaml"
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

			(mkBundle & {BUNDLE: "workload", RESOURCES:      APP_RESOURCES.workloadResources}).out
			(mkBundle & {BUNDLE: "control-plane", RESOURCES: APP_RESOURCES.controlPlaneResources}).out
		}
	}
}

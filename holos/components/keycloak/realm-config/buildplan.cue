// realm-config reconciles the Keycloak holos realm on every scripts/apply with
// an idempotent keycloak-config-cli Job.  It is a sibling leaf of the operator,
// operator-crds, and instance components and inherits KeycloakVersion and
// KeycloakNamespace from the shared ancestor ../keycloak.cue.
//
// Why a Job, not the KeycloakRealmImport CR: the operator's realm import is
// bootstrap-only — its import Job skips when the realm already exists, so
// post-bootstrap changes to a realm (new clients, roles, groups) never
// reconcile (the CAVEAT in ../instance/buildplan.cue's REALM_IMPORT and the
// "Keycloak realm reconciliation" placeholder this component closes).
// keycloak-config-cli runs adorsys/keycloak-config-cli against the live admin
// API and converges the realm declaratively on every run, so re-running
// scripts/apply is safe and reconciling.  The instance component's
// KeycloakRealmImport still bootstraps the realm shell (realm holos,
// enabled: true) on a clean cluster; this Job layers the platform's roles,
// the authenticated default group, and the Argo CD OIDC client onto it and
// keeps them converged thereafter.  The two never fight: the import file below
// carries realm: "holos" only — no enabled or identity-provider fields that
// would contend with the KeycloakRealmImport CR.
//
// The keycloak Namespace is registered in the central namespaces registry
// (holos/namespaces.cue) with _ambient: false — never emitted by components.
package holos

import "encoding/json"

let NAMESPACE = KeycloakNamespace

let NAME = "keycloak-config"

// KeycloakConfigCLIImage pins the keycloak-config-cli image.  adorsys publishes
// it as docker.io/adorsys/keycloak-config-cli:<cli-version>-<keycloak-version>;
// 6.5.1 is the latest CLI release (https://github.com/adorsys/keycloak-config-cli/releases,
// checked 2026-06-13) and 6.5.1-26.5.5 is its newest tag built for the
// Keycloak 26.x line this platform runs (KeycloakVersion 26.6.3 in
// ../keycloak.cue — same 26 major, so the admin-API contract matches).  The
// tag publishes a multi-arch manifest list including linux/arm64 (verified
// against the Docker Hub tag's image list 2026-06-13), required because the
// cluster is k3d on Apple Silicon (ADR-7).  Re-check the releases page and the
// tag's architectures, and that a tag for the current Keycloak 26.x patch
// still exists, before bumping KeycloakVersion or this pin.
let KeycloakConfigCLIImage = "docker.io/adorsys/keycloak-config-cli:6.5.1-26.5.5"

// The operator names the Keycloak Service "<cr-name>-service"; with CR name
// "keycloak" that is "keycloak-service", serving HTTPS on 8443 — the SAN the
// keycloak-tls certificate covers (../instance/buildplan.cue).  The Job runs in
// this namespace so the short Service name resolves.
let KEYCLOAK_URL = "https://keycloak-service:8443"

// The operator generates the initial admin credentials in this Secret (keys
// username/password) on first reconcile — ../instance/buildplan.cue's admin
// bootstrap note.  No plaintext credentials are committed; the Job reads them
// at runtime via secretKeyRef.
let ADMIN_SECRET = "keycloak-initial-admin"

let REALM = "holos"

// The Argo CD OIDC client (HOL-1211 wires Argo CD's SSO against it).  A public
// PKCE client: Argo CD's UI and CLI are public OAuth clients that cannot hold a
// secret, so they use the Authorization Code flow with PKCE (S256) instead.
let ARGOCD_CLIENT_ID = "argocd"

// auth.holos.localhost is the Keycloak hostname (../instance/buildplan.cue);
// argocd.holos.localhost is the Argo CD UI hostname (components/argocd).  Both
// resolve to 127.0.0.1 on the host per docs/local-cluster.md.
let ARGOCD_PUBLIC_URL = "https://argocd.holos.localhost"

// The two protocol mappers that populate the groups claim Argo CD's RBAC keys
// on (HOL-1211).  Both write claim.name "groups" into the ID and access
// tokens: the group-membership mapper emits the user's group names (full.path
// false → bare names like "authenticated", not "/authenticated"), and the
// realm-role mapper emits realm-role names (platform-owner, …) into the same
// claim, so a single groups claim carries both group and role membership.
let GROUPS_CLAIM = "groups"

// REALM_CONFIG is the keycloak-config-cli import document, marshalled to JSON
// in the ConfigMap below.  Authored in CUE so it stays reviewable and validated
// (encoding/json renders it deterministically — stable key order, no manual
// JSON to drift).
//
// Scope discipline: realm carries only "realm: holos" — no enabled or
// identity-provider fields — so it layers onto the realm shell the instance
// component's KeycloakRealmImport bootstraps without contending with it.
// keycloak-config-cli's default import.managed.* behavior is "no-delete" for
// objects it does not declare, so this never purges realm state it doesn't own
// (full-realm purge is deliberately NOT enabled).
let REALM_CONFIG = {
	realm: REALM

	// AC #3: the three platform realm roles.
	roles: realm: [
		{name: "platform-owner"},
		{name: "platform-editor"},
		{name: "platform-viewer"},
	]

	// AC #4: the authenticated group, registered as a realm default group so
	// every realm user is automatically bound to it on creation.
	groups: [{name: "authenticated"}]
	defaultGroups: ["/authenticated"]

	clients: [{
		clientId:            ARGOCD_CLIENT_ID
		name:                "Argo CD"
		enabled:             true
		protocol:            "openid-connect"
		publicClient:        true
		standardFlowEnabled: true
		// Confidential-only flows are off: a public client holds no secret, so
		// the client-credentials and direct-access-grant flows must not be
		// available.
		serviceAccountsEnabled:    false
		directAccessGrantsEnabled: false
		attributes: "pkce.code.challenge.method": "S256"
		redirectUris: [
			"\(ARGOCD_PUBLIC_URL)/auth/callback",
			// The Argo CD CLI's local callback listener for `argocd login --sso`.
			"http://localhost:8085/auth/callback",
		]
		webOrigins: [ARGOCD_PUBLIC_URL]
		protocolMappers: [
			{
				name:           "groups"
				protocol:       "openid-connect"
				protocolMapper: "oidc-group-membership-mapper"
				config: {
					"claim.name": GROUPS_CLAIM
					// Bare group names ("authenticated"), not paths
					// ("/authenticated"), so Argo CD RBAC policy matches on the
					// group name directly.
					"full.path":            "false"
					"id.token.claim":       "true"
					"access.token.claim":   "true"
					"userinfo.token.claim": "true"
				}
			},
			{
				name:           "realm-roles"
				protocol:       "openid-connect"
				protocolMapper: "oidc-usermodel-realm-role-mapper"
				config: {
					// Same claim as the group mapper: a single groups claim
					// carries both group membership and realm-role names, so
					// Argo CD RBAC can key on platform-owner/editor/viewer the
					// same way it keys on group names.
					"claim.name":           GROUPS_CLAIM
					"jsonType.label":       "String"
					"multivalued":          "true"
					"id.token.claim":       "true"
					"access.token.claim":   "true"
					"userinfo.token.claim": "true"
				}
			},
		]
	}]
}

// CONFIG_MAP holds the import document as holos.json for the Job to read from
// /config.  marshalled with encoding/json so the committed deploy file is a
// stable, reviewable JSON string.
let CONFIG_MAP = {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: {
		name:      "keycloak-realm-config"
		namespace: NAMESPACE
	}
	data: "holos.json": json.Marshal(REALM_CONFIG)
}

// CONFIG_JOB runs keycloak-config-cli against the live Keycloak admin API to
// converge the realm.  It mirrors the nats stream-bootstrap Job's hardened
// posture (components/nats/buildplan.cue): non-root, read-only root filesystem
// with a writable /tmp emptyDir, dropped capabilities, no service-account token
// (it talks to Keycloak over the network, never to the Kubernetes API), a
// backoffLimit, and a day-long ttlSecondsAfterFinished so a routine re-apply of
// the unchanged spec recreates and re-converges after GC.
//
// CAVEAT (the nats Job precedent): a completed Job's pod template is immutable.
// Re-applying this unchanged spec while the Job still exists is a no-op; the
// TTL garbage-collects it a day after completion, after which a re-apply
// recreates the Job and it re-converges the realm idempotently.  Only a
// pod-template change within the TTL window (e.g. a new image tag or import
// document) requires deleting the old Job first
// (kubectl -n keycloak delete job keycloak-config) — the realm state it created
// survives in Keycloak's database, and the new Job converges it.
let CONFIG_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		// The availability check inside keycloak-config-cli (env below) already
		// tolerates a Keycloak that is not yet serving; backoffLimit covers the
		// rarer case of the pod itself failing (e.g. an image-pull blip) before
		// the check runs.
		backoffLimit: 3
		// A day keeps the Job's logs around for debugging a fresh import while
		// still dissolving the immutable-pod-template caveat above for routine
		// re-applies (the nats BOOTSTRAP_JOB precedent).
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: "app.kubernetes.io/name": NAME
			spec: {
				restartPolicy: "Never"
				// The Job talks to Keycloak over the network, never to the
				// Kubernetes API, so it needs no ServiceAccount token.
				automountServiceAccountToken: false
				securityContext: {
					runAsNonRoot: true
					// The keycloak-config-cli image runs as a non-root user; pin
					// the conventional "nobody" uid explicitly (the nats Job
					// precedent) so the read-only-rootfs posture is unambiguous.
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "keycloak-config"
					image: KeycloakConfigCLIImage
					env: [
						{
							name:  "KEYCLOAK_URL"
							value: KEYCLOAK_URL
						},
						{
							name: "KEYCLOAK_USER"
							valueFrom: secretKeyRef: {
								name: ADMIN_SECRET
								key:  "username"
							}
						},
						{
							name: "KEYCLOAK_PASSWORD"
							valueFrom: secretKeyRef: {
								name: ADMIN_SECRET
								key:  "password"
							}
						},
						// Tolerate the apply-script gate polling before the
						// server is fully serving — the nats reachability-loop
						// equivalent.  keycloak-config-cli retries the admin API
						// until it answers (or the timeout elapses, after which
						// the pod fails and backoffLimit re-runs it).
						{
							name:  "KEYCLOAK_AVAILABILITYCHECK_ENABLED"
							value: "true"
						},
						{
							name:  "KEYCLOAK_AVAILABILITYCHECK_TIMEOUT"
							value: "120s"
						},
						// The keycloak namespace is _ambient: false
						// (holos/namespaces.cue), so this in-namespace HTTPS hop
						// to keycloak-service:8443 is not mesh-secured.  The
						// backend cert is issued by the per-machine local CA
						// (components/local-ca) and cannot be embedded at render
						// time deterministically, so the Job cannot pin a trust
						// anchor the way the Gateway→Keycloak DestinationRule
						// does.  Skip verification on this single in-cluster hop,
						// mirroring the reference platform's insecureSkipVerify on
						// the Keycloak DestinationRule.
						{
							name:  "KEYCLOAK_SSLVERIFY"
							value: "false"
						},
						{
							name:  "IMPORT_FILES_LOCATIONS"
							value: "/config/holos.json"
						},
						// keycloak-config-cli is a Spring Boot app; point its
						// writable temp directory at the /tmp emptyDir so the
						// read-only root filesystem does not block it.
						{
							name:  "JAVA_OPTS"
							value: "-Djava.io.tmpdir=/tmp"
						},
					]
					resources: {
						requests: {
							cpu:    "50m"
							memory: "256Mi"
						}
						limits: memory: "512Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					volumeMounts: [
						{
							name:      "config"
							mountPath: "/config"
							readOnly:  true
						},
						{
							name:      "tmp"
							mountPath: "/tmp"
						},
					]
				}]
				volumes: [
					{
						name: "config"
						configMap: name: CONFIG_MAP.metadata.name
					},
					{
						name: "tmp"
						emptyDir: {}
					},
				]
			}
		}
	}
}

userDefinedBuildPlan: {
	metadata: name: NAME
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored ConfigMap and Job validate against the vendored
				// Kubernetes schemas at render time.
				resources: #Resources & {
					ConfigMap: (CONFIG_MAP.metadata.name): CONFIG_MAP
					Job: (CONFIG_JOB.metadata.name):       CONFIG_JOB
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						namespace: NAMESPACE
						resources: inputs
					}
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

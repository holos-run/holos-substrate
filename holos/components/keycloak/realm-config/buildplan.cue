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

import (
	"encoding/json"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

let NAMESPACE = KeycloakNamespace

// NAME is the component name and the ConfigMap-mount/pod-label base.  The Job's
// own metadata.name is NAME plus a content hash (JOB_NAME below) so a realm
// import change always produces a fresh Job — see CONFIG_HASH.
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

// KUBECTL_IMAGE pins the image the QUAY_OIDC secret bootstrap Job runs kubectl
// from.  docker.io/alpine/kubectl:1.33.3 is a manifest list including
// linux/arm64 (checked 2026-06-13 via the Docker Hub registry API) and is
// alpine-based, providing the /bin/sh the Job script needs (the version-matched
// rancher/kubectl image is scratch-based — no shell).  This matches the pin the
// quay component uses for its quay-secret-keys bootstrap
// (components/quay/buildplan.cue).  1.33.3 is the oldest tag the alpine/kubectl
// repository publishes; it exceeds the kubectl +/-1 minor skew recommendation
// against the live server (v1.31.5+k3s1, two minors back) but the Job performs
// only core/v1 Secret get/create, which are version-stable.  Re-check available
// tags against the cluster version before bumping.
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

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

// The Kargo OIDC client (HOL-1250; the Kargo consumer side that authenticates
// against it is wired in HOL-1251).  Like Argo CD this is a public PKCE client:
// Kargo's web UI and CLI are public OAuth clients that cannot hold a secret, so
// they use the Authorization Code flow with PKCE (S256) — modeled on the argocd
// client above, NOT the confidential quay client.
let KARGO_CLIENT_ID = "kargo"

// kargo.holos.localhost is the Kargo UI hostname (components/kargo/buildplan.cue
// HOSTNAME) and resolves to 127.0.0.1 on the host per docs/local-cluster.md.
let KARGO_PUBLIC_URL = "https://kargo.holos.localhost"

// The two protocol mappers that populate the groups claim Argo CD's RBAC keys
// on (HOL-1211).  Both write claim.name "groups" into the ID and access
// tokens: the group-membership mapper emits the user's group names (full.path
// false → bare names like "authenticated", not "/authenticated"), and the
// realm-role mapper emits realm-role names (platform-owner, …) into the same
// claim, so a single groups claim carries both group and role membership.
let GROUPS_CLAIM = "groups"

// The Quay OIDC client (HOL-1218; Quay's OIDC login is wired in HOL-1219).
// Unlike Argo CD this is a *confidential* client: Quay holds a client secret
// and uses the Authorization Code flow authenticated by that secret.  PKCE is
// deliberately NOT required for this client (no pkce.code.challenge.method
// attribute) — Quay's confidential client-secret flow did not round-trip a
// matching PKCE code_verifier, causing a "code exchange: 400" SSO failure, so
// PKCE was dropped on both ends (HOL-1257).  The secret is generated once by the
// QUAY_OIDC bootstrap Job below and substituted into the realm import at run
// time by keycloak-config-cli's $(env:...) expansion, so no secret is ever
// committed.
let QUAY_CLIENT_ID = "quay"

// quay.holos.localhost is the Quay UI/registry hostname (components/quay) and
// resolves to 127.0.0.1 on the host per docs/local-cluster.md.
let QUAY_PUBLIC_URL = "https://quay.holos.localhost"

// QUAY_OIDC_SECRET is the Secret carrying the shared OIDC client secret.  The
// QUAY_OIDC bootstrap Job (below) generates it once and writes it to BOTH the
// keycloak namespace (read here, by the keycloak-config-cli Job, to set the
// client secret in the realm) AND the quay namespace (read in Phase 2,
// HOL-1219, by the Quay Deployment).  Generating it here — the phase that owns
// the Keycloak side — keeps the secret fully provisioned in both namespaces
// after this phase, so Phase 2 only consumes it.  QUAY_OIDC_SECRET_KEY is the
// data key both consumers read.
let QUAY_OIDC_SECRET = "quay-oidc"
let QUAY_OIDC_SECRET_KEY = "client_secret"

// The quay namespace the bootstrap Job also writes the secret into.  The
// #RegisteredNamespace constraint (holos/namespaces.cue) turns drift between
// this literal and the registry entry into a render failure rather than an
// apply-time NotFound.
let QUAY_NAMESPACE = "quay" & #RegisteredNamespace

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

	// HOL-1218: per-client roles for the quay client.  Quay's recommended
	// Keycloak integration drives team membership from a groups claim, and the
	// quay-client-roles protocol mapper below folds these client-role names
	// into that same claim — so granting a user the quay "platform-admin" role
	// surfaces "platform-admin" in their groups claim, which Quay's OIDC team
	// sync binds to a Quay team.  platform-admin is the Holos Platform Admin
	// (Quay superuser/org admin); project-admin is per-project administrative
	// access.  Additional per-project roles are granted by binding Keycloak
	// groups to Quay teams (documented in Phase 3, HOL-1220).
	roles: client: (QUAY_CLIENT_ID): [
		{name: "platform-admin", description: "Holos Platform Admin — Quay superuser/org admin"},
		{name: "project-admin", description: "Per-project administrative access in Quay"},
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
	}, {
		// HOL-1218: the Quay OIDC client.  Modeled on the argocd client above
		// but confidential — Quay sends a client secret — and using the
		// Authorization Code flow authenticated by that secret, without PKCE
		// (HOL-1257; see the no-pkce.code.challenge.method note below).
		//
		// HOL-1245: the realm-role mapper below also emits the platform-owner
		// realm role into the groups claim (mirroring the argocd client), so
		// the privileged platform-owner role is recognizable to Quay's team
		// sync and any future relying party the same way group names are.  See
		// HOL-1242 for the platform-owner role; Quay superuser grants stay out
		// of scope (SUPER_USERS is a static config list).
		clientId:            QUAY_CLIENT_ID
		name:                "Quay"
		enabled:             true
		protocol:            "openid-connect"
		publicClient:        false // confidential: Quay sends a client secret
		standardFlowEnabled: true
		// Confidential client, but the additional confidential-only flows are
		// off: Quay uses only the browser Authorization Code flow, authenticated
		// by the client secret (no PKCE — see the attributes note below).
		serviceAccountsEnabled:    false
		directAccessGrantsEnabled: false
		// keycloak-config-cli substitutes the generated secret at run time from
		// the QUAY_OIDC_CLIENT_SECRET env var (CONFIG_JOB below), so no secret
		// is committed.  The bootstrap Job generates it once and never rotates
		// it, so the value here stays stable across reconciles.
		secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"
		// No pkce.code.challenge.method attribute: Keycloak treats a client that
		// sets it as *requiring* PKCE, but Quay authenticates as a plain
		// confidential client with the client secret above and does not reliably
		// send a matching code_verifier — which produced the "code exchange: 400"
		// SSO failure (HOL-1257).  Omitting the attribute leaves PKCE optional, so
		// the confidential client-secret flow succeeds.  The argocd and kargo
		// public clients DO keep pkce.code.challenge.method — only quay drops it.
		redirectUris: ["\(QUAY_PUBLIC_URL)/*"]
		webOrigins: [QUAY_PUBLIC_URL]
		protocolMappers: [
			{
				name:           "groups"
				protocol:       "openid-connect"
				protocolMapper: "oidc-group-membership-mapper"
				config: {
					"claim.name": GROUPS_CLAIM
					// Bare group names, not paths, so Quay's OIDC team sync
					// matches on the group name directly (the argocd precedent).
					"full.path":            "false"
					"id.token.claim":       "true"
					"access.token.claim":   "true"
					"userinfo.token.claim": "true"
				}
			},
			{
				name:           "quay-client-roles"
				protocol:       "openid-connect"
				protocolMapper: "oidc-usermodel-client-role-mapper"
				config: {
					// Fold the quay client roles (platform-admin, project-admin)
					// into the same groups claim, so Quay's team sync picks up
					// both group membership and client-role grants uniformly.
					"claim.name":                          GROUPS_CLAIM
					"usermodel.clientRoleMapping.clientId": QUAY_CLIENT_ID
					"jsonType.label":                      "String"
					"multivalued":                         "true"
					"id.token.claim":                      "true"
					"access.token.claim":                  "true"
					"userinfo.token.claim":                "true"
				}
			},
			{
				name:           "realm-roles"
				protocol:       "openid-connect"
				protocolMapper: "oidc-usermodel-realm-role-mapper"
				config: {
					// Same claim as the group/client-role mappers: a single groups
					// claim carries group membership, the quay client roles, and the
					// platform realm roles (platform-owner/editor/viewer), so Quay's
					// team sync and any future relying party key on platform-owner the
					// same way they key on group names.  Emitted unconditionally — set
					// in the ID, access, and userinfo tokens, not gated by an optional
					// client scope.
					"claim.name":           GROUPS_CLAIM
					"jsonType.label":       "String"
					"multivalued":          "true"
					"id.token.claim":       "true"
					"access.token.claim":   "true"
					"userinfo.token.claim": "true"
				}
			},
			{
				// Phase 2 (HOL-1219) sets Quay's PREFERRED_USERNAME_CLAIM_NAME
				// to preferred_username.  Keycloak's default "profile" client
				// scope already emits that claim, but declare an explicit
				// property mapper here so the quay client surfaces it
				// independently of default-scope assignment — making the
				// Phase 2 contract self-contained and robust to scope changes.
				name:           "preferred_username"
				protocol:       "openid-connect"
				protocolMapper: "oidc-usermodel-property-mapper"
				config: {
					"user.attribute":       "username"
					"claim.name":           "preferred_username"
					"jsonType.label":       "String"
					"id.token.claim":       "true"
					"access.token.claim":   "true"
					"userinfo.token.claim": "true"
				}
			},
		]
	}, {
		// HOL-1250: the Kargo OIDC client.  Mirrors the argocd public PKCE
		// client above (NOT the confidential quay client): a public client that
		// holds no secret and uses the Authorization Code flow with PKCE (S256).
		// No client secret and no bootstrap Job are involved.
		//
		// The realm-role mapper below emits the platform realm roles
		// (platform-owner/platform-editor/platform-viewer) into the groups claim
		// unconditionally — set in the ID, access, and userinfo tokens, not gated
		// by an optional client scope — which satisfies HOL-1249 AC #3.
		clientId:            KARGO_CLIENT_ID
		name:                "Kargo"
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
		// The /* wildcard covers Kargo's web-UI OAuth callback (the quay client
		// uses the same form).  The Kargo CLI's loopback SSO redirect URI
		// (http://localhost:<port>/...) is intentionally NOT registered yet — the
		// web UI is the path AC #2 is verified through; a CLI redirect URI can be
		// added once the CLI's callback port/path is confirmed.
		redirectUris: ["\(KARGO_PUBLIC_URL)/*"]
		webOrigins: [KARGO_PUBLIC_URL]
		protocolMappers: [
			{
				name:           "groups"
				protocol:       "openid-connect"
				protocolMapper: "oidc-group-membership-mapper"
				config: {
					"claim.name": GROUPS_CLAIM
					// Bare group names ("authenticated"), not paths
					// ("/authenticated"), matching the argocd precedent.
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
					// carries both group membership and the platform realm-role
					// names (platform-owner/editor/viewer).  This mapper satisfies
					// HOL-1249 AC #3 — it folds the realm roles into the groups
					// claim unconditionally.
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

// CONFIG_HASH is a short content hash of everything that determines what the Job
// converges: the realm import document and the image tag.  It is suffixed onto
// the Job's metadata.name (JOB_NAME) so the rendered manifest is self-describing
// — the name reveals which config a given Job ran — and so the deploy file name
// changes visibly in review when the import document or image changes.
//
// Note: the hash is NOT what guarantees a reconcile runs.  A completed Job's pod
// template is immutable and kubectl apply never re-runs an existing Complete Job,
// so the actual "reconcile on every apply" guarantee comes from scripts/apply's
// pre_keycloak_config hook, which deletes every keycloak-config Job (by the
// app.kubernetes.io/name label) before the apply — covering forward edits AND
// reverts to a previously-applied config within the Job's TTL window, which a
// content-hash name alone would miss (the old hash's Complete Job would linger).
// keycloak-config-cli converges idempotently, so re-running on every apply is the
// intended behavior.  8 hex chars (32 bits) is ample for the naming role.
let CONFIG_HASH = strings.SliceRunes(hex.Encode(sha256.Sum256(
CONFIG_MAP.data["holos.json"]+"\n"+KeycloakConfigCLIImage)), 0, 8)

// JOB_NAME embeds the content hash so an import-document or image change renders
// a distinct Job (see CONFIG_HASH).  scripts/apply's wait_keycloak_config gate
// resolves the current Job by reading this rendered name from the committed
// manifest, so the gate always waits on exactly the Job that was just applied.
let JOB_NAME = "\(NAME)-\(CONFIG_HASH)"

// CONFIG_JOB runs keycloak-config-cli against the live Keycloak admin API to
// converge the realm.  It uses a hardened bootstrap-Job
// posture: non-root, read-only root filesystem
// with a writable /tmp emptyDir, dropped capabilities, no service-account token
// (it talks to Keycloak over the network, never to the Kubernetes API), a
// backoffLimit, and a day-long ttlSecondsAfterFinished so a routine re-apply of
// the unchanged spec recreates and re-converges after GC.
//
// The metadata.name carries the CONFIG_HASH suffix (JOB_NAME) so a realm import
// or image change always renders a new Job and reconciles on the next apply
// rather than being masked by a stale Complete Job — see CONFIG_HASH.  The
// app.kubernetes.io/name label stays the stable NAME so the apply-script gate and
// any operator queries can select the current Job regardless of its hash.
let CONFIG_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata: {
		name:      JOB_NAME
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
		// re-applies (the bootstrap-Job precedent).
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
					// the conventional "nobody" uid explicitly so the
					// read-only-rootfs posture is unambiguous.
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
						// HOL-1218: the shared Quay OIDC client secret.
						// keycloak-config-cli substitutes $(env:QUAY_OIDC_CLIENT_SECRET)
						// into the realm import's quay client at run time.  The
						// QUAY_OIDC bootstrap Job below generates this Secret once
						// (in this namespace and in quay) and never rotates it, so
						// the client secret stays stable across reconciles.  The
						// secretKeyRef holds the Job's pod in a pending state until
						// the bootstrap Job has created the Secret — the same
						// level-triggered convergence the rest of the platform relies
						// on — so no explicit ordering between the two Jobs is needed.
						{
							name: "QUAY_OIDC_CLIENT_SECRET"
							valueFrom: secretKeyRef: {
								name: QUAY_OIDC_SECRET
								key:  QUAY_OIDC_SECRET_KEY
							}
						},
						// Tolerate the apply-script gate polling before the
						// server is fully serving.  keycloak-config-cli retries the admin API
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
						// HOL-1218: enable keycloak-config-cli's $(env:...)
						// variable substitution so the quay client's
						// secret: "$(env:QUAY_OIDC_CLIENT_SECRET)" is replaced
						// with the bootstrapped value at import time.  The CLI
						// defaults this to false (import.var-substitution.enabled),
						// which would otherwise import the literal placeholder
						// string as the confidential client secret.  Substitution
						// only touches $(...) tokens, so the rest of the realm JSON
						// (which contains none) is unaffected.
						{
							name:  "IMPORT_VARSUBSTITUTION_ENABLED"
							value: "true"
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

// QUAY_OIDC_BOOTSTRAP is the generate-once bootstrap for the shared Quay OIDC
// client secret, mirroring the quay-secret-keys-bootstrap pattern in
// components/quay/buildplan.cue.  It runs in the keycloak namespace and writes
// the quay-oidc Secret (key client_secret) into BOTH the keycloak namespace
// (read by the keycloak-config-cli Job here to set the client secret in the
// realm) AND the quay namespace (read by the Quay Deployment in Phase 2,
// HOL-1219).  Because the keycloak-config component applies before quay
// (scripts/apply COMPONENTS order) and the namespaces component creates every
// namespace first, the quay namespace already exists when this Job runs.
//
// Generate-once discipline: the script creates the Secret only if it does not
// already exist in a given namespace, and never overwrites an existing one, so
// the client secret is stable across re-applies and is never regenerated (AC
// #3).  It is generated at run time and never committed.
let QUAY_OIDC_BOOTSTRAP = "quay-oidc-bootstrap"

// The bootstrap resources carry their own app.kubernetes.io/name — NOT the
// keycloak-config Job's NAME — so the wait_keycloak_config gate's
// label-independent name resolution and the keycloak-config-Job delete in
// pre_keycloak_config (which selects app.kubernetes.io/name=keycloak-config)
// never touch this Job.  pre_keycloak_config issues a SEPARATE delete selecting
// app.kubernetes.io/name=quay-oidc-bootstrap so this Job, too, re-runs on every
// apply (its name is constant, with no config hash to force a fresh Job), which
// re-runs a previously Failed bootstrap and lets the gate observe its outcome.
let QUAY_OIDC_BOOTSTRAP_METADATA = {
	name:      QUAY_OIDC_BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": QUAY_OIDC_BOOTSTRAP
}

// The create-if-absent bootstrap script.  The OIDC client secret must be stable
// across restarts (it is set on the Keycloak client and consumed by Quay) and
// MUST NOT be committed: the script generates it once and writes it to each
// namespace only if absent there, never overwriting.  The value is alphanumeric
// (base64 with +/=/newlines stripped) so it is safe in the realm JSON and any
// downstream context.  The length check guards against an improbable pipeline
// failure under set -eu (no pipefail in busybox sh) silently creating an empty
// secret — which create-if-absent would otherwise make permanent.  The Secret
// is piped as a manifest on stdin so the key material never appears in the
// container's argv (/proc-visible).
let QUAY_OIDC_BOOTSTRAP_SCRIPT = """
	set -eu
	random_secret() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	read_secret() {
	  # Echo the decoded client_secret if the Secret exists in $1, else nothing.
	  if kubectl -n "$1" get secret \(QUAY_OIDC_SECRET) >/dev/null 2>&1; then
	    kubectl -n "$1" get secret \(QUAY_OIDC_SECRET) \\
	      -o jsonpath='{.data.\(QUAY_OIDC_SECRET_KEY)}' | base64 -d
	  fi
	}
	KC_SECRET="$(read_secret \(NAMESPACE))"
	QUAY_SECRET="$(read_secret \(QUAY_NAMESPACE))"
	# If both copies already exist they MUST carry the same value — Keycloak and
	# Quay authenticate with the same client secret.  A mismatch (e.g. the two
	# namespaces were seeded independently) cannot be silently reconciled by a
	# create-if-absent Job that never overwrites, so fail loudly and let an
	# operator delete the wrong copy rather than leaving the two sides
	# permanently disagreeing.
	if [ -n "$KC_SECRET" ] && [ -n "$QUAY_SECRET" ] && [ "$KC_SECRET" != "$QUAY_SECRET" ]; then
	  echo "ERROR: \(QUAY_OIDC_SECRET) differs between the \(NAMESPACE) and \(QUAY_NAMESPACE) namespaces." >&2
	  echo "       Delete the incorrect copy so this Job can re-create it from the surviving one." >&2
	  exit 1
	fi
	# Reuse whichever copy exists (they match by the check above) so a partial
	# prior run is completed with the same value; otherwise generate once.
	if [ -n "$KC_SECRET" ]; then
	  CLIENT_SECRET="$KC_SECRET"
	elif [ -n "$QUAY_SECRET" ]; then
	  CLIENT_SECRET="$QUAY_SECRET"
	else
	  CLIENT_SECRET="$(random_secret)"
	fi
	[ "${#CLIENT_SECRET}" -eq 48 ]
	for ns in \(NAMESPACE) \(QUAY_NAMESPACE); do
	  if kubectl -n "$ns" get secret \(QUAY_OIDC_SECRET) >/dev/null 2>&1; then
	    echo "Secret \(QUAY_OIDC_SECRET) already exists in $ns; leaving it untouched."
	    continue
	  fi
	  kubectl -n "$ns" create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(QUAY_OIDC_SECRET)
	  namespace: $ns
	stringData:
	  \(QUAY_OIDC_SECRET_KEY): "${CLIENT_SECRET}"
	EOF
	  echo "Secret \(QUAY_OIDC_SECRET) created in $ns."
	done
	"""

let QUAY_OIDC_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   QUAY_OIDC_BOOTSTRAP_METADATA
}

// Per-namespace Role granting the Job get/create on the one quay-oidc Secret.
// get is restricted to the quay-oidc resourceName; create cannot be restricted
// by resourceName (the API server does not evaluate resourceNames for create),
// so the create grant is namespace-wide on secrets.  One Role per namespace the
// Job writes into (keycloak and quay), each bound to the bootstrap
// ServiceAccount that lives in the keycloak namespace.
let QUAY_OIDC_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	#namespace: string
	// Name carries the target namespace so the two Roles (one per namespace the
	// Job writes into) render to distinct one-file-per-resource artifacts under
	// the default kubectl-slice <kind>-<name> template — they would otherwise
	// collide on a shared name.
	metadata: {
		name:      "\(QUAY_OIDC_BOOTSTRAP)-\(#namespace)"
		namespace: #namespace
		labels: "app.kubernetes.io/name": QUAY_OIDC_BOOTSTRAP
	}
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [QUAY_OIDC_SECRET]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let QUAY_OIDC_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	#namespace: string
	// Name carries the target namespace for the same one-file-per-resource
	// reason as the Role above; roleRef binds the same-namespace Role, which
	// shares the suffix.
	metadata: {
		name:      "\(QUAY_OIDC_BOOTSTRAP)-\(#namespace)"
		namespace: #namespace
		labels: "app.kubernetes.io/name": QUAY_OIDC_BOOTSTRAP
	}
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     "\(QUAY_OIDC_BOOTSTRAP)-\(#namespace)"
	}
	// The ServiceAccount lives in the keycloak namespace; a RoleBinding in the
	// quay namespace may still reference it by namespace, granting the Job
	// cross-namespace write of the one Secret.
	subjects: [{
		kind:      "ServiceAccount"
		name:      QUAY_OIDC_BOOTSTRAP
		namespace: NAMESPACE
	}]
}

let QUAY_OIDC_BOOTSTRAP_ROLE_KEYCLOAK = QUAY_OIDC_BOOTSTRAP_ROLE & {#namespace: NAMESPACE}
let QUAY_OIDC_BOOTSTRAP_ROLE_QUAY = QUAY_OIDC_BOOTSTRAP_ROLE & {#namespace: QUAY_NAMESPACE}
let QUAY_OIDC_BOOTSTRAP_ROLE_BINDING_KEYCLOAK = QUAY_OIDC_BOOTSTRAP_ROLE_BINDING & {#namespace: NAMESPACE}
let QUAY_OIDC_BOOTSTRAP_ROLE_BINDING_QUAY = QUAY_OIDC_BOOTSTRAP_ROLE_BINDING & {#namespace: QUAY_NAMESPACE}

// A completed Job's pod template is immutable, so a plain re-apply of this
// unchanged spec is a no-op while the Job exists.  Unlike the quay
// quay-secret-keys-bootstrap, this Job is deleted and recreated on every apply
// by pre_keycloak_config (see scripts/apply), so it always re-runs: a forward
// spec change, a previously Failed run, and a routine re-apply all get a fresh
// Job, and wait_keycloak_config can observe its outcome.  The Job is idempotent
// — it exits 0 leaving existing Secrets untouched (the create-if-absent script
// above) — and the Secrets are separate objects that survive the Job deletion,
// so the generate-once guarantee holds across re-runs.
let QUAY_OIDC_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   QUAY_OIDC_BOOTSTRAP_METADATA
	spec: {
		backoffLimit: 3
		// A day keeps the Job's logs around for debugging a fresh bootstrap
		// while still dissolving the immutable-pod-template caveat for routine
		// re-applies (the quay BOOTSTRAP_JOB precedent).
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: QUAY_OIDC_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: QUAY_OIDC_BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// The alpine/kubectl image declares no non-root USER; pick
					// the conventional "nobody" uid (the quay bootstrap
					// precedent).
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:    "bootstrap"
					image:   KUBECTL_IMAGE
					command: ["/bin/sh", "-c", QUAY_OIDC_BOOTSTRAP_SCRIPT]
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
					Job: {
						(CONFIG_JOB.metadata.name):              CONFIG_JOB
						(QUAY_OIDC_BOOTSTRAP_JOB.metadata.name): QUAY_OIDC_BOOTSTRAP_JOB
					}
					ServiceAccount: (QUAY_OIDC_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): QUAY_OIDC_BOOTSTRAP_SERVICE_ACCOUNT
					// One Role/RoleBinding per namespace the bootstrap Job
					// writes the quay-oidc Secret into (keycloak and quay).
					// Keyed by namespace so the two same-named objects do not
					// collide in this map.
					Role: {
						(NAMESPACE):      QUAY_OIDC_BOOTSTRAP_ROLE_KEYCLOAK
						(QUAY_NAMESPACE): QUAY_OIDC_BOOTSTRAP_ROLE_QUAY
					}
					RoleBinding: {
						(NAMESPACE):      QUAY_OIDC_BOOTSTRAP_ROLE_BINDING_KEYCLOAK
						(QUAY_NAMESPACE): QUAY_OIDC_BOOTSTRAP_ROLE_BINDING_QUAY
					}
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					// No blanket namespace: directive — the cross-namespace
					// bootstrap RBAC (Role/RoleBinding in the quay namespace)
					// must keep its own metadata.namespace.  Every resource
					// here sets metadata.namespace explicitly.
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

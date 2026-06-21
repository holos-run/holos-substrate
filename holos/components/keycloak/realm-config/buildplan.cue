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
// carries realm: "holos" only — no enabled field — so it does not contend with
// the KeycloakRealmImport CR, which owns enabled.  HOL-1369: this Job now also
// owns the holos realm's identityProviders[] (the esso OIDC broker), so the IdP's
// confidential clientSecret is injected at runtime; the KeycloakRealmImport
// declares NO identityProviders, so there is still no contention.
//
// The keycloak Namespace is registered in the central namespaces registry
// (holos/namespaces.cue) with _ambient: true — never emitted by components.
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
// "keycloak" that is "keycloak-service".  Keycloak runs HTTP-only behind the
// shared Gateway (HOL-1362), so this Service exposes the port named "http"
// (8080).  The Job runs in this namespace so the short Service name resolves,
// and reaches the admin API over plaintext HTTP — the keycloak namespace is now
// ambient-enrolled, so ztunnel HBONE mTLS secures this in-namespace wire hop.
let KEYCLOAK_URL = "http://keycloak-service:8080"

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

// auth.holos.internal is the Keycloak hostname (../instance/buildplan.cue);
// argocd.holos.internal is the Argo CD UI hostname (components/argocd).  Both
// resolve to 127.0.0.1 on the host per docs/local-cluster.md.
let ARGOCD_PUBLIC_URL = "https://argocd.holos.internal"

// The Kargo OIDC client (HOL-1250; the Kargo consumer side that authenticates
// against it is wired in HOL-1251).  Like Argo CD this is a public PKCE client:
// Kargo's web UI and CLI are public OAuth clients that cannot hold a secret, so
// they use the Authorization Code flow with PKCE (S256) — modeled on the argocd
// client above, NOT the confidential quay client.
let KARGO_CLIENT_ID = "kargo"

// kargo.holos.internal is the Kargo UI hostname (components/kargo/buildplan.cue
// HOSTNAME) and resolves to 127.0.0.1 on the host per docs/local-cluster.md.
let KARGO_PUBLIC_URL = "https://kargo.holos.internal"

// The two protocol mappers that populate the groups claim Argo CD's RBAC keys
// on (HOL-1211).  Both write claim.name "groups" into the ID and access
// tokens: the group-membership mapper emits the user's group names (full.path
// false → bare names like "authenticated", not "/authenticated"), and the
// realm-role mapper emits realm-role names (platform-owner, …) into the same
// claim, so a single groups claim carries both group and role membership.
let GROUPS_CLAIM = "groups"

// The Quay OIDC client (HOL-1218; Quay's OIDC login is wired in HOL-1219).
// Unlike Argo CD this is a *confidential* client: Quay holds a client secret
// and uses the Authorization Code flow authenticated by that secret WITHOUT
// PKCE (HOL-1317 — Quay 3.17.3 mishandles PKCE state across logout; see the
// client definition below).  The secret is generated once by the QUAY_OIDC
// bootstrap Job below and substituted into the realm import at run time by
// keycloak-config-cli's $(env:...) expansion, so no secret is ever committed.
//
// The clientId is the Quay public URL (https://quay.holos.internal), matching
// the production example's QUAY_CLIENT_ID.  The three protocol mappers and the
// client-role declarations key off this same QUAY_CLIENT_ID let, so the value is
// changed in exactly one place.  Quay's HOL-1293 OIDC phase sets its CLIENT_ID to
// the same value.
let QUAY_CLIENT_ID = "https://quay.holos.internal"

// HOL-1294: the quay client's clientId is changed from "quay" to the public-URL
// form above.  keycloak-config-cli runs in no-delete mode (see the header), so on
// a cluster that previously imported the old "quay" client that client is NOT
// removed by the rename — it would linger, still enabled, with its old no-PKCE
// config and redirect URIs, as a stale relying party that could still
// authenticate.  Declare the old clientId explicitly as a managed but DISABLED
// tombstone so the reconcile converges it to disabled on upgrade.  On a fresh
// cluster this just creates an inert disabled client; the cost is one tidy
// disabled entry, and it can be dropped once no cluster carries the old client.
let QUAY_LEGACY_CLIENT_ID = "quay"

// quay.holos.internal is the Quay UI/registry hostname (components/quay) and
// resolves to 127.0.0.1 on the host per docs/local-cluster.md.
let QUAY_PUBLIC_URL = "https://quay.holos.internal"

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

// HOL-1294: the two holos-realm users that become Quay's superusers when Quay
// switches to OIDC-backend auth (HOL-1293's Quay phase).  Keycloak is the
// identity source; each user's password is generated once at runtime by the
// PASSWORD_BOOTSTRAP Job below and published as a Secret in the keycloak
// namespace (never committed, never rotated), then substituted into the realm
// import via keycloak-config-cli's $(env:...) expansion.
//
// Naming convention: realm users that represent SERVICE accounts carry an "svc-"
// prefix (svc-quay-resource-controller — the future Quay Resource Controller's
// service identity), clearly distinguishing them from HUMAN users (quay-admin,
// unprefixed — human administration).  The durable repo-wide statement of this
// convention is in AGENTS.md (Conventions).  The prefix is part of the
// username and flows through to Quay's superuser match (preferred_username ==
// username) and the SSO login name, so it MUST NOT change without updating the
// SUPER_USERS list set in HOL-1293's Quay phase.
//
// Each user object pairs a Secret name (the generated password lives there under
// PASSWORD_SECRET_KEY) with the env var keycloak-config-cli reads it from.
let SVC_QUAY_RC_USERNAME = "svc-quay-resource-controller"
let QUAY_ADMIN_USERNAME = "quay-admin"

// The data key both password Secrets carry.
let PASSWORD_SECRET_KEY = "password"

// Each generated password Secret is named after the user it belongs to (in the
// keycloak namespace), and the keycloak-config Job reads it through the matching
// env var below — wired exactly like QUAY_OIDC_CLIENT_SECRET.
let SVC_QUAY_RC_PASSWORD_SECRET = SVC_QUAY_RC_USERNAME
let QUAY_ADMIN_PASSWORD_SECRET = QUAY_ADMIN_USERNAME
let SVC_QUAY_RC_PASSWORD_ENV = "SVC_QUAY_RESOURCE_CONTROLLER_PASSWORD"
let QUAY_ADMIN_PASSWORD_ENV = "QUAY_ADMIN_PASSWORD"

// HOL-1348: the Holos Controller's Keycloak admin credential (ADR-20 "Admin
// credential", preferred shape #1 — a confidential service-account client with
// scoped realm-management roles).  The controller authenticates to the Keycloak
// Admin REST API with a client_credentials grant using this client's id +
// secret to reconcile the keycloak.holos.run Kinds (KeycloakInstance,
// KeycloakGroup, KeycloakUser, KeycloakClient).  This is a dedicated,
// least-privileged machine identity — NOT the bootstrap keycloak-initial-admin
// credential keycloak-config-cli itself uses (ADR-20).
//
// The clientId is "svc-"-prefixed to mark it a service account (the platform's
// svc-quay-resource-controller convention; AGENTS.md Conventions).  The
// generated client secret is delivered at runtime by the
// CONTROLLER_CREDS_BOOTSTRAP Job below into the holos-controller-keycloak-creds
// Secret (keys clientId/clientSecret) in the holos-controller namespace — the
// exact Secret name + keys internal/controller/keycloak/credentials.go reads
// (DefaultCredentialsSecretName, credentialKeyClientID/credentialKeyClientSecret)
// — and into the keycloak namespace so keycloak-config-cli can substitute it
// into this client's realm import via $(env:...).  It is generated once and
// never rotated, so the value stays stable across reconciles (the QUAY_OIDC
// precedent), and never committed (the Runtime Secret Handling guardrail).
let CONTROLLER_CLIENT_ID = "svc-holos-controller"

// The holos-controller namespace the controller resolves its credential Secret
// from (api/keycloak/v1alpha1 DefaultControllerNamespace, set on the manager
// Deployment via the downward-API POD_NAMESPACE).  Unifying with
// #RegisteredNamespace turns drift between this literal and the registry entry
// into a render failure rather than an apply-time NotFound.
let CONTROLLER_NAMESPACE = "holos-controller" & #RegisteredNamespace

// CONTROLLER_CREDS_SECRET is the Secret the bootstrap Job writes the controller's
// Keycloak admin credential into.  Its name and keys are the contract
// internal/controller/keycloak/credentials.go reads: DefaultCredentialsSecretName
// "holos-controller-keycloak-creds" with keys clientId and clientSecret (the
// optional tokenUrl is omitted — the in-cluster token endpoint derives from the
// KeycloakInstance's url + realm).
let CONTROLLER_CREDS_SECRET = "holos-controller-keycloak-creds"
let CONTROLLER_CREDS_CLIENT_ID_KEY = "clientId"
let CONTROLLER_CREDS_CLIENT_SECRET_KEY = "clientSecret"

// The Secret (in the keycloak namespace) the bootstrap Job ALSO writes the same
// generated client secret into, under CONTROLLER_CLIENT_SECRET_KEY, so the
// keycloak-config-cli Job can read it via secretKeyRef and substitute it into
// the svc-holos-controller client's realm import — mirroring how the QUAY_OIDC
// secret is shared between the keycloak and quay namespaces.
let CONTROLLER_CLIENT_SECRET = "svc-holos-controller-oidc"
let CONTROLLER_CLIENT_SECRET_KEY = "client_secret"
let CONTROLLER_CLIENT_SECRET_ENV = "HOLOS_CONTROLLER_CLIENT_SECRET"

// The realm-management client roles the controller's service account holds —
// the scoped set ADR-20 recommends (NOT blanket realm-admin).  manage-clients
// covers KeycloakClient (create/update clients, client roles, protocol mappers);
// manage-users covers KeycloakUser (create users, group membership,
// federated-identity links) and KeycloakGroup membership; query-groups +
// query-clients let the reconcilers look objects up by name/path before acting.
let CONTROLLER_REALM_MGMT_ROLES = [
	"manage-clients",
	"manage-users",
	"query-groups",
	"query-clients",
]

// HOL-1369: the esso OIDC identity provider brokered into the holos realm.  The
// esso realm (HOL-1368, the sibling realm-esso-config component) models an
// upstream Enterprise SSO; this IdP makes the holos realm authenticate users
// against it.  ESSO_IDP_ALIAS is the broker alias — the {provider} path segment
// in the broker endpoint https://auth.holos.internal/realms/holos/broker/esso/endpoint
// the esso confidential client registers as its redirect URI — and the alias the
// project component's KeycloakUser identityProviderLink references (HOL-1369
// repoints it from the placeholder "holos" to "esso").
let ESSO_IDP_ALIAS = "esso"

// The esso realm's issuer base; the IdP discovers its OIDC endpoints from
// .well-known under it.  Served at this URL by the shared Keycloak CR + HTTPRoute
// (no new route is added — HOL-1368).
let ESSO_ISSUER_URL = "https://auth.holos.internal/realms/esso"

// The clientId the holos realm's broker authenticates AS at the esso realm — the
// holos realm's own issuer URL, by Keycloak's broker convention.  It MUST equal
// the confidential client the esso realm-config component declares
// (realm-esso-config ESSO_IDP_CLIENT_ID), so both sides of the broker agree.
let ESSO_IDP_CLIENT_ID = "https://auth.holos.internal/realms/holos"

// ESSO_IDP_OIDC_SECRET is the Secret carrying the shared esso broker client
// secret.  It is generated ONCE by the esso realm-config component's
// ESSO_BOOTSTRAP Job (realm-esso-config, HOL-1368) into the keycloak namespace —
// the SINGLE SOURCE of the client secret; this holos-side IdP reads the SAME
// Secret name + key so both sides of the broker authenticate with one value.  Do
// not rename without updating realm-esso-config to match.  keycloak-config-cli
// substitutes $(env:ESSO_IDP_CLIENT_SECRET) into the IdP at import time from the
// ESSO_IDP_CLIENT_SECRET_ENV env var (CONFIG_JOB below).
let ESSO_IDP_OIDC_SECRET = "esso-idp-oidc"
let ESSO_IDP_OIDC_SECRET_KEY = "client_secret"
let ESSO_IDP_CLIENT_SECRET_ENV = "ESSO_IDP_CLIENT_SECRET"

// HOL-1369: the first-broker-login flow alias the esso IdP points at.  This is a
// CUSTOM (builtIn: false) flow, NOT Keycloak's built-in "first broker login" — see
// the long comment on authenticationFlows below for why redefining the built-in
// fails under keycloak-config-cli.
let FIRST_BROKER_LOGIN_FLOW = "first broker login auto-link"
let USER_CREATION_OR_LINKING_FLOW = "User creation or linking auto-link"

// REALM_CONFIG is the keycloak-config-cli import document, marshalled to JSON
// in the ConfigMap below.  Authored in CUE so it stays reviewable and validated
// (encoding/json renders it deterministically — stable key order, no manual
// JSON to drift).
//
// Scope discipline: realm carries only "realm: holos" — no enabled field — so it
// layers onto the realm shell the instance component's KeycloakRealmImport
// bootstraps without contending with it (the import owns enabled; HOL-1369 this
// Job owns identityProviders[], which the import does not declare).
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

	// HOL-1294: the two realm users that become Quay's superusers under
	// OIDC-backend auth (HOL-1293).  Keycloak is the identity source; each carries
	// the platform-owner realm role so the groups claim surfaces it for team-sync
	// visibility (Quay *superuser* itself still comes from the static SUPER_USERS
	// list set in HOL-1293's Quay phase, matched by preferred_username ==
	// username).  Passwords are generated once at runtime (PASSWORD_BOOTSTRAP Job
	// below) and substituted from env at import time, so none is committed.
	//
	// Naming convention (see the SVC_QUAY_RC_USERNAME let above): "svc-" marks a
	// SERVICE account (svc-quay-resource-controller — the future Quay Resource
	// Controller's machine identity); the unprefixed quay-admin is a HUMAN
	// administrator.
	users: [
		{
			// Service identity for the future Quay Resource Controller (machine,
			// not a human) — hence the "svc-" prefix.
			username:      SVC_QUAY_RC_USERNAME
			enabled:       true
			email:         "\(SVC_QUAY_RC_USERNAME)@holos.internal"
			emailVerified: true
			credentials: [{type: "password", value: "$(env:\(SVC_QUAY_RC_PASSWORD_ENV))", temporary: false}]
			realmRoles: ["platform-owner"]
		},
		{
			// Human administrator (unprefixed username) for Quay administration.
			username:      QUAY_ADMIN_USERNAME
			enabled:       true
			email:         "\(QUAY_ADMIN_USERNAME)@holos.internal"
			emailVerified: true
			credentials: [{type: "password", value: "$(env:\(QUAY_ADMIN_PASSWORD_ENV))", temporary: false}]
			realmRoles: ["platform-owner"]
		},
		{
			// HOL-1348: the synthetic service-account user backing the
			// svc-holos-controller confidential client.  Keycloak auto-creates a
			// service-account-<clientId> user when serviceAccountsEnabled is true;
			// keycloak-config-cli assigns that user's roles via this users[] entry
			// (serviceAccountClientId binds it to the client; clientRoles grants the
			// scoped realm-management roles).  This is the supported keycloak-config-cli
			// path for service-account role assignment — there is no
			// serviceAccountClientRoles field on the client object.  No password (a
			// service account authenticates by the client secret, not a password).
			username:              "service-account-\(CONTROLLER_CLIENT_ID)"
			enabled:               true
			serviceAccountClientId: CONTROLLER_CLIENT_ID
			clientRoles: "realm-management": CONTROLLER_REALM_MGMT_ROLES
		},
	]

	clients: [{
		// HOL-1294: retirement tombstone for the renamed quay client (see
		// QUAY_LEGACY_CLIENT_ID).  enabled: false so a previously-imported "quay"
		// client cannot keep authenticating after the rename under no-delete
		// reconciliation.  No secret, mappers, or redirect URIs — a disabled client
		// needs none, and omitting them narrows what the stale client could ever do.
		clientId: QUAY_LEGACY_CLIENT_ID
		name:     "Quay (retired — renamed to the public-URL clientId)"
		enabled:  false
		protocol: "openid-connect"
	}, {
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
		// Authorization Code flow authenticated by that secret, WITHOUT PKCE
		// (HOL-1317; pkce.code.challenge.method is set to the empty/"none" method
		// — see the no-PKCE note below).
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
		// by the client secret alone (no PKCE — see the no-PKCE note below).
		serviceAccountsEnabled:    false
		directAccessGrantsEnabled: false
		// keycloak-config-cli substitutes the generated secret at run time from
		// the QUAY_OIDC_CLIENT_SECRET env var (CONFIG_JOB below), so no secret
		// is committed.  The bootstrap Job generates it once and never rotates
		// it, so the value here stays stable across reconciles.
		secret: "$(env:QUAY_OIDC_CLIENT_SECRET)"
		// HOL-1317: PKCE is intentionally NOT required for the quay client.
		// Quay 3.17.3 does not properly support PKCE: it stores the
		// code_challenge state in the _csrf_token and never clears it on logout,
		// so a stale code_verifier is replayed on the next login and Keycloak
		// rejects the code exchange.  Quay's KEYCLOAK_LOGIN_CONFIG correspondingly
		// sets USE_PKCE: false (components/quay), so no code_verifier is sent and
		// Keycloak must not require one.
		//
		// The attribute is set to the EMPTY method (Keycloak's "none") rather
		// than omitted: keycloak-config-cli merges client attributes on update,
		// so a key absent from the import is NOT removed from a client that
		// previously carried pkce.code.challenge.method: "S256" (HOL-1293/HOL-1294)
		// — it would linger as S256 and keep PKCE required, re-breaking login.
		// Setting it to "" overwrites any prior value on every reconcile, so the
		// no-PKCE state is enforced on a fresh cluster and a previously-PKCE one
		// alike.  The argocd/kargo public clients above keep S256; only quay
		// disables it.  Do NOT restore "S256" here without re-enabling USE_PKCE
		// on the Quay side and confirming the logout-state bug is fixed.
		attributes: "pkce.code.challenge.method": ""
		// redirectUris are the three explicit Quay OAuth callback paths from the
		// HOL-1317 client JSON (replacing the earlier /* wildcard).  webOrigins is
		// an empty list: Quay's server-side Authorization Code flow needs no CORS
		// origin, and an empty list avoids persisting a blank-string origin entry.
		redirectUris: [
			"\(QUAY_PUBLIC_URL)/oauth2/keycloak/callback/attach",
			"\(QUAY_PUBLIC_URL)/oauth2/keycloak/callback/cli",
			"\(QUAY_PUBLIC_URL)/oauth2/keycloak/callback",
		]
		webOrigins: []
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
					"claim.name":                           GROUPS_CLAIM
					"usermodel.clientRoleMapping.clientId": QUAY_CLIENT_ID
					"jsonType.label":                       "String"
					"multivalued":                          "true"
					"id.token.claim":                       "true"
					"access.token.claim":                   "true"
					"userinfo.token.claim":                 "true"
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
	}, {
		// HOL-1348: the Holos Controller's confidential service-account client
		// (ADR-20 "Admin credential", preferred shape).  Unlike argocd/kargo
		// (public, browser/CLI login) and quay (confidential, browser login),
		// this client never logs a human in: it has NO standard browser flow
		// and NO redirect URIs, only the client_credentials grant
		// (serviceAccountsEnabled) the controller uses to call the Admin REST
		// API as its own machine identity.  The generated secret is substituted
		// at import time from $(env:HOLOS_CONTROLLER_CLIENT_SECRET), provisioned
		// once by CONTROLLER_CREDS_BOOTSTRAP below (never committed).
		clientId:     CONTROLLER_CLIENT_ID
		name:         "Holos Controller (service account)"
		enabled:      true
		protocol:     "openid-connect"
		publicClient: false // confidential: holds a client secret
		// Browser Authorization Code flow OFF — this client never logs a user
		// in; it only authenticates itself.  serviceAccountsEnabled turns on the
		// client_credentials grant the controller authenticates with.
		standardFlowEnabled:       false
		directAccessGrantsEnabled: false
		serviceAccountsEnabled:    true
		secret:                    "$(env:\(CONTROLLER_CLIENT_SECRET_ENV))"
		// A service-account client has no browser redirect.
		redirectUris: []
		webOrigins: []
		// The scoped realm-management roles the controller's service account holds
		// are NOT granted on the client object — keycloak-config-cli has no
		// serviceAccountClientRoles client field.  They are assigned via the
		// synthetic service-account user (service-account-svc-holos-controller) in
		// the users[] list below (serviceAccountClientId + clientRoles), the
		// keycloak-config-cli convention for service-account role assignment.
	}]

	// HOL-1348/HOL-1369: a first-broker-login flow configured for AUTO-LINK, so a
	// pre-provisioned KeycloakUser (a record the controller creates by email,
	// e.g. bob@example.com) is linked to its federated identity on the user's
	// FIRST login through an upstream IdP — rather than Keycloak creating a
	// duplicate account or prompting the user to manually link.  This is the
	// realm half of the auto-link mechanism ADR-20's KeycloakUser relies on; the
	// IdP half (trustEmail: true and firstBrokerLoginFlowAlias) is set on the
	// esso identityProvider in identityProviders[] below.
	//
	// Why a CUSTOM flow, not a redefinition of the built-in "first broker login"
	// (HOL-1369 fixes the earlier HOL-1348 failure):
	// keycloak-config-cli REFUSES to add executions to a built-in flow.  Keycloak
	// 26.x's built-in "first broker login" → "User creation or linking" subflow
	// contains idp-create-user-if-unique + a "Handle Existing Account" subflow,
	// but NO idp-auto-link execution (DefaultAuthenticationFlows.firstBrokerLoginFlow).
	// When a builtIn: true flow's import differs, config-cli takes the
	// updateBuiltInFlow path (it never recreates a built-in flow — recreateTopLevelFlow
	// throws "Deletion or creation of built-in flows is not possible") and then
	// looks each imported execution up in the STORED built-in flow by authenticator
	// (ExecutionFlowRepository.getExecutionsByAuthFlow → filter providerId == authenticator).
	// idp-auto-link is absent from the stored built-in subflow, so the lookup is
	// empty and config-cli throws "Cannot find stored execution by authenticator
	// 'idp-auto-link' in top-level flow 'User creation or linking'" — the failure
	// this issue fixes.  (The message says "top-level flow" because config-cli
	// reuses the topLevelFlowAlias variable name when configuring a subflow's
	// executions; the flow is the topLevel: false subflow.)
	//
	// The fix: declare a brand-new CUSTOM top-level flow (builtIn: false) with its
	// own custom subflow and unique aliases (FIRST_BROKER_LOGIN_FLOW /
	// USER_CREATION_OR_LINKING_FLOW — distinct from the built-in names so config-cli
	// does not match the built-in entries), then point the esso IdP's
	// firstBrokerLoginFlowAlias at it (identityProviders[] below).  Because the flow
	// is not built-in, config-cli CREATES it — executions and all, including
	// idp-auto-link — via createTopLevelFlow/createExecutionsAndExecutionFlows, and
	// converges it idempotently on every apply.  The built-in "first broker login"
	// is left untouched.
	//
	// Subflow shape required by config-cli (AuthenticationFlowUtil.getSubFlow):
	// the parent execution references the subflow by flowAlias + authenticatorFlow:
	// true, and the subflow's executions live in a SEPARATE authenticationFlows[]
	// entry whose alias matches and topLevel: false.
	//
	// Executions: idp-review-profile (REQUIRED) keeps the profile-review step on a
	// genuinely new federated user; then the custom subflow runs Detect Existing
	// Broker User (idp-create-user-if-unique, ALTERNATIVE — links when a unique
	// existing user matches) followed by Automatically Set Existing User
	// (idp-auto-link, ALTERNATIVE — sets the matched user as the authenticated
	// account with NO prompt).  Combined with the esso IdP's trustEmail: true, a
	// login whose asserted email matches a pre-provisioned user auto-links silently.
	//
	// Authored as a CUE struct on REALM_CONFIG, which is json.Marshal'd into the
	// import ConfigMap below — a marshalled struct, never a hand-written JSON blob
	// (the "No raw inline YAML/JSON in CUE" guardrail).
	authenticationFlows: [{
		alias:       FIRST_BROKER_LOGIN_FLOW
		description: "First broker login with silent auto-link to a pre-provisioned user of the same (IdP-trusted) email (HOL-1348/HOL-1369/ADR-20)."
		providerId:  "basic-flow"
		topLevel:    true
		builtIn:     false
		authenticationExecutions: [
			{
				// Review profile stays user-decided so a genuinely new federated
				// user can still complete their profile.
				authenticator:     "idp-review-profile"
				requirement:       "REQUIRED"
				priority:          10
				authenticatorFlow: false
				userSetupAllowed:  false
			},
			{
				// The custom "Handle Existing Account" subflow that auto-links.
				flowAlias:         USER_CREATION_OR_LINKING_FLOW
				requirement:       "REQUIRED"
				priority:          20
				authenticatorFlow: true
				userSetupAllowed:  false
			},
		]
	}, {
		alias:       USER_CREATION_OR_LINKING_FLOW
		description: "Detect an existing broker user and set it automatically (auto-link), no manual confirmation (HOL-1348/HOL-1369)."
		providerId:  "basic-flow"
		topLevel:    false
		builtIn:     false
		authenticationExecutions: [
			{
				// Detect Existing Broker User: find a unique local user matching
				// the federated identity (by trusted email); ALTERNATIVE so the
				// flow can fall through to plain creation for a brand-new user.
				authenticator:     "idp-create-user-if-unique"
				requirement:       "ALTERNATIVE"
				priority:          10
				authenticatorFlow: false
				userSetupAllowed:  false
			},
			{
				// Automatically Set Existing User: when an existing user was
				// detected, set it as the authenticated account and link the
				// federated identity with NO interactive confirm/verify step.
				authenticator:     "idp-auto-link"
				requirement:       "ALTERNATIVE"
				priority:          20
				authenticatorFlow: false
				userSetupAllowed:  false
			},
		]
	}]

	// HOL-1369: the esso OIDC identity provider.  This is the holos-realm half of
	// the brokering: the holos realm authenticates users against the esso realm
	// (HOL-1368) over OIDC.  trustEmail: true lets the auto-link flow above match a
	// federated login to a pre-provisioned holos user by the esso-asserted (and
	// esso-verified) email; firstBrokerLoginFlowAlias points at the custom auto-link
	// flow so that match links silently.
	//
	// Ownership / scope discipline: this IdP is a holos-realm object, so it lives
	// here in the holos realm-config Job (NOT the esso realm-config component, which
	// is scoped to realm: "esso").  HOL-1369 moves identityProviders[] ownership to
	// this Job (so the IdP's clientSecret is injected at runtime via
	// $(env:ESSO_IDP_CLIENT_SECRET)); the operator's KeycloakRealmImport still owns
	// the realm's enabled flag and declares NO identityProviders, so the two
	// reconciliation paths do not contend (see AGENTS.md "Keycloak Configuration as
	// Code").
	//
	// The client secret is the shared esso broker secret — generated ONCE by the
	// esso realm-config component's ESSO_BOOTSTRAP Job (the single source) and read
	// here from the SAME esso-idp-oidc Secret (CONFIG_JOB below), substituted at
	// import time by keycloak-config-cli's $(env:...) expansion, so no secret is
	// committed.  The clientId is the holos realm's issuer URL, matching the
	// confidential client the esso realm registered for this broker.
	//
	// config carries OIDC endpoints discovered from the esso realm's
	// .well-known/openid-configuration (validateSignature off — Keycloak fetches
	// the JWKS from the issuer directly via the discovery document).  useJwksUrl
	// true so signature keys are fetched from the JWKS endpoint rather than pinned.
	identityProviders: [{
		alias:                     ESSO_IDP_ALIAS
		displayName:               "Enterprise SSO (esso)"
		providerId:                "oidc"
		enabled:                   true
		trustEmail:                true
		storeToken:                false
		addReadTokenRoleOnCreate:  false
		linkOnly:                  false
		firstBrokerLoginFlowAlias: FIRST_BROKER_LOGIN_FLOW
		config: {
			// Discover the OIDC endpoints from the esso realm's well-known document.
			// keycloak-config-cli passes this through to Keycloak's OIDC IdP, which
			// resolves authorizationUrl/tokenUrl/userInfoUrl/jwksUrl/issuer from it.
			discoveryEndpoint: "\(ESSO_ISSUER_URL)/.well-known/openid-configuration"
			issuer:            ESSO_ISSUER_URL
			authorizationUrl:  "\(ESSO_ISSUER_URL)/protocol/openid-connect/auth"
			tokenUrl:          "\(ESSO_ISSUER_URL)/protocol/openid-connect/token"
			userInfoUrl:       "\(ESSO_ISSUER_URL)/protocol/openid-connect/userinfo"
			jwksUrl:           "\(ESSO_ISSUER_URL)/protocol/openid-connect/certs"
			logoutUrl:         "\(ESSO_ISSUER_URL)/protocol/openid-connect/logout"
			useJwksUrl:        "true"
			validateSignature: "true"
			clientId:          ESSO_IDP_CLIENT_ID
			clientSecret:      "$(env:\(ESSO_IDP_CLIENT_SECRET_ENV))"
			clientAuthMethod:  "client_secret_post"
			syncMode:          "IMPORT"
			defaultScope:      "openid profile email"
		}
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
						// HOL-1294: the generated passwords for the two Quay
						// superuser realm users.  keycloak-config-cli substitutes
						// $(env:...) into the users' credentials at import time.  The
						// PASSWORD_BOOTSTRAP Job below generates each Secret once and
						// never rotates it, so the passwords stay stable across
						// reconciles.  Like QUAY_OIDC_CLIENT_SECRET, the secretKeyRef
						// holds the Job's pod pending until the bootstrap Job has
						// created the Secret — the same level-triggered convergence —
						// so no explicit ordering between the Jobs is needed.
						{
							name: SVC_QUAY_RC_PASSWORD_ENV
							valueFrom: secretKeyRef: {
								name: SVC_QUAY_RC_PASSWORD_SECRET
								key:  PASSWORD_SECRET_KEY
							}
						},
						{
							name: QUAY_ADMIN_PASSWORD_ENV
							valueFrom: secretKeyRef: {
								name: QUAY_ADMIN_PASSWORD_SECRET
								key:  PASSWORD_SECRET_KEY
							}
						},
						// HOL-1348: the generated secret for the svc-holos-controller
						// service-account client.  keycloak-config-cli substitutes
						// $(env:HOLOS_CONTROLLER_CLIENT_SECRET) into that client's
						// realm import at import time.  CONTROLLER_CREDS_BOOTSTRAP
						// below generates it once into the keycloak-namespace Secret
						// CONTROLLER_CLIENT_SECRET (and the holos-controller-namespace
						// credential Secret the controller reads) and never rotates
						// it, so the value stays stable across reconciles.  The
						// secretKeyRef holds this Job's pod pending until the
						// bootstrap Job has created the Secret — the same
						// level-triggered convergence the other two secrets rely on.
						{
							name: CONTROLLER_CLIENT_SECRET_ENV
							valueFrom: secretKeyRef: {
								name: CONTROLLER_CLIENT_SECRET
								key:  CONTROLLER_CLIENT_SECRET_KEY
							}
						},
						// HOL-1369: the shared esso broker client secret.
						// keycloak-config-cli substitutes $(env:ESSO_IDP_CLIENT_SECRET)
						// into the esso identityProvider's clientSecret at import time.
						// The Secret is generated ONCE by the esso realm-config
						// component's ESSO_BOOTSTRAP Job (realm-esso-config, HOL-1368) —
						// the single source of the broker secret — and never rotated, so
						// the value stays stable across reconciles.  The secretKeyRef holds
						// this Job's pod pending until that Secret exists (the same
						// level-triggered convergence the Quay/controller secrets rely on),
						// so no explicit ordering between the two components' Jobs is needed.
						{
							name: ESSO_IDP_CLIENT_SECRET_ENV
							valueFrom: secretKeyRef: {
								name: ESSO_IDP_OIDC_SECRET
								key:  ESSO_IDP_OIDC_SECRET_KEY
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
					name:  "bootstrap"
					image: KUBECTL_IMAGE
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

// PASSWORD_BOOTSTRAP is the generate-once bootstrap for the two Quay-superuser
// realm-user passwords (HOL-1294), modeled on QUAY_OIDC_BOOTSTRAP above.  It runs
// in the keycloak namespace and writes one Secret per user
// (svc-quay-resource-controller, quay-admin), each carrying the generated
// password under PASSWORD_SECRET_KEY.  keycloak-config-cli reads each via the
// SVC_QUAY_RC_PASSWORD_ENV / QUAY_ADMIN_PASSWORD_ENV env vars on CONFIG_JOB and
// substitutes it into the realm import at run time, so no password is committed.
//
// Generate-once discipline: the script creates each Secret only if it does not
// already exist, and never overwrites, so the passwords are stable across
// re-applies and are never rotated (the QUAY_OIDC_BOOTSTRAP precedent).
let PASSWORD_BOOTSTRAP = "quay-user-password-bootstrap"

// Own app.kubernetes.io/name (NOT the keycloak-config Job's NAME) so the
// pre_keycloak_config delete and wait_keycloak_config gate never touch this Job.
// pre_keycloak_config issues a SEPARATE delete selecting this label so the Job
// re-runs on every apply (its name is constant), re-running a previously Failed
// bootstrap and letting the gate observe its outcome.
let PASSWORD_BOOTSTRAP_METADATA = {
	name:      PASSWORD_BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": PASSWORD_BOOTSTRAP
}

// The two password Secrets the Job manages, paired with the username whose
// password each holds.  Iterated by the create-if-absent script below and used
// to scope the Role's get grant to exactly these names.
let PASSWORD_SECRETS = [SVC_QUAY_RC_PASSWORD_SECRET, QUAY_ADMIN_PASSWORD_SECRET]

// The create-if-absent bootstrap script.  Each user's password must be stable
// across restarts (it is set on the Keycloak user and is the Quay SSO login
// credential) and MUST NOT be committed: the script generates a fresh 48-char
// alphanumeric value for each Secret only if absent, never overwriting.  The
// value is alphanumeric (base64 with +/=/newlines stripped) so it is safe in the
// realm JSON.  The length check guards against an improbable pipeline failure
// under set -eu (no pipefail in busybox sh) silently creating an empty Secret —
// which create-if-absent would otherwise make permanent.  Each Secret is piped
// as a manifest on stdin so the password never appears in the container's argv
// (/proc-visible).
let PASSWORD_BOOTSTRAP_SCRIPT = """
	set -eu
	random_secret() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	for name in \(SVC_QUAY_RC_PASSWORD_SECRET) \(QUAY_ADMIN_PASSWORD_SECRET); do
	  if kubectl -n \(NAMESPACE) get secret "$name" >/dev/null 2>&1; then
	    echo "Secret $name already exists in \(NAMESPACE); leaving it untouched."
	    continue
	  fi
	  PASSWORD="$(random_secret)"
	  [ "${#PASSWORD}" -eq 48 ]
	  kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: $name
	  namespace: \(NAMESPACE)
	stringData:
	  \(PASSWORD_SECRET_KEY): "${PASSWORD}"
	EOF
	  echo "Secret $name created in \(NAMESPACE)."
	done
	"""

let PASSWORD_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   PASSWORD_BOOTSTRAP_METADATA
}

// Role granting the Job get on exactly the two password Secrets and namespace-wide
// create on secrets (the API server does not evaluate resourceNames for create).
// Both Secrets live in the keycloak namespace, so a single Role/RoleBinding pair
// suffices (unlike the cross-namespace QUAY_OIDC bootstrap).
let PASSWORD_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   PASSWORD_BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: PASSWORD_SECRETS
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let PASSWORD_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   PASSWORD_BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     PASSWORD_BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      PASSWORD_BOOTSTRAP
		namespace: NAMESPACE
	}]
}

// Deleted and recreated on every apply by pre_keycloak_config (its own label), so
// it always re-runs; idempotent (exits 0 leaving existing Secrets untouched), and
// the Secrets survive the Job deletion, so the generate-once guarantee holds
// across re-runs — the QUAY_OIDC_BOOTSTRAP_JOB precedent.
let PASSWORD_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   PASSWORD_BOOTSTRAP_METADATA
	spec: {
		backoffLimit:            3
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: PASSWORD_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: PASSWORD_BOOTSTRAP
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
					command: ["/bin/sh", "-c", PASSWORD_BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME; point it at
					// the writable emptyDir since the root filesystem is read-only.
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

// CONTROLLER_CREDS_BOOTSTRAP is the generate-once bootstrap for the Holos
// Controller's Keycloak admin credential (HOL-1348), modeled on
// QUAY_OIDC_BOOTSTRAP above (which writes one shared secret into two
// namespaces).  It runs in the keycloak namespace and writes the SAME generated
// client secret into two Secrets:
//
//   - keycloak namespace, CONTROLLER_CLIENT_SECRET (key client_secret): read by
//     the keycloak-config-cli Job here to set the svc-holos-controller client's
//     secret in the realm via $(env:HOLOS_CONTROLLER_CLIENT_SECRET).
//   - holos-controller namespace, CONTROLLER_CREDS_SECRET (keys clientId +
//     clientSecret): the credential the shipped Holos Controller reads
//     (api/keycloak/v1alpha1 DefaultCredentialsSecretName; the keys
//     internal/controller/keycloak/credentials.go reads).  clientId is the
//     non-secret CONTROLLER_CLIENT_ID; clientSecret is the generated value.
//
// Generate-once discipline: the script creates each Secret only if absent and
// never overwrites, so the client secret is stable across re-applies and never
// rotated (the generate-once guarantee Keycloak's stored client secret and the
// controller's credential both depend on).  Generated at runtime, never
// committed (the Runtime Secret Handling guardrail).
let CONTROLLER_CREDS_BOOTSTRAP = "holos-controller-keycloak-creds-bootstrap"

let CONTROLLER_CREDS_BOOTSTRAP_METADATA = {
	name:      CONTROLLER_CREDS_BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": CONTROLLER_CREDS_BOOTSTRAP
}

// The create-if-absent bootstrap script.  The client secret is set on the
// Keycloak svc-holos-controller client AND read by the controller, so the two
// copies MUST carry the same value; a mismatch is fatal (the QUAY_OIDC_BOOTSTRAP
// precedent) since a create-if-absent Job that never overwrites cannot
// reconcile divergence.  The value is alphanumeric (base64 stripped to A-Za-z0-9)
// so it is safe both in the realm JSON and as a client secret.  The length check
// guards against an empty-secret pipeline failure under set -eu.  Each Secret is
// piped on stdin so the material never appears in the container's argv.
let CONTROLLER_CREDS_BOOTSTRAP_SCRIPT = """
	set -eu
	random_secret() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	# Echo the decoded client secret from the keycloak-namespace copy, else from
	# the holos-controller-namespace credential Secret, else nothing.
	KC_SECRET=""
	if kubectl -n \(NAMESPACE) get secret \(CONTROLLER_CLIENT_SECRET) >/dev/null 2>&1; then
	  KC_SECRET="$(kubectl -n \(NAMESPACE) get secret \(CONTROLLER_CLIENT_SECRET) \\
	    -o jsonpath='{.data.\(CONTROLLER_CLIENT_SECRET_KEY)}' | base64 -d)"
	fi
	CTRL_SECRET=""
	if kubectl -n \(CONTROLLER_NAMESPACE) get secret \(CONTROLLER_CREDS_SECRET) >/dev/null 2>&1; then
	  CTRL_SECRET="$(kubectl -n \(CONTROLLER_NAMESPACE) get secret \(CONTROLLER_CREDS_SECRET) \\
	    -o jsonpath='{.data.\(CONTROLLER_CREDS_CLIENT_SECRET_KEY)}' | base64 -d)"
	fi
	if [ -n "$KC_SECRET" ] && [ -n "$CTRL_SECRET" ] && [ "$KC_SECRET" != "$CTRL_SECRET" ]; then
	  echo "ERROR: \(CONTROLLER_CLIENT_SECRET) (\(NAMESPACE)) and \(CONTROLLER_CREDS_SECRET) (\(CONTROLLER_NAMESPACE)) client secrets differ." >&2
	  echo "       Delete the incorrect copy so this Job can re-create it from the surviving one." >&2
	  exit 1
	fi
	if [ -n "$KC_SECRET" ]; then
	  CLIENT_SECRET="$KC_SECRET"
	elif [ -n "$CTRL_SECRET" ]; then
	  CLIENT_SECRET="$CTRL_SECRET"
	else
	  CLIENT_SECRET="$(random_secret)"
	fi
	[ "${#CLIENT_SECRET}" -eq 48 ]
	# keycloak namespace: the single client_secret key keycloak-config-cli reads.
	if kubectl -n \(NAMESPACE) get secret \(CONTROLLER_CLIENT_SECRET) >/dev/null 2>&1; then
	  echo "Secret \(CONTROLLER_CLIENT_SECRET) already exists in \(NAMESPACE); leaving it untouched."
	else
	  kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(CONTROLLER_CLIENT_SECRET)
	  namespace: \(NAMESPACE)
	stringData:
	  \(CONTROLLER_CLIENT_SECRET_KEY): "${CLIENT_SECRET}"
	EOF
	  echo "Secret \(CONTROLLER_CLIENT_SECRET) created in \(NAMESPACE)."
	fi
	# holos-controller namespace: clientId + clientSecret, the credential keys the
	# controller reads (credentials.go).  clientId is the non-secret client id.
	if kubectl -n \(CONTROLLER_NAMESPACE) get secret \(CONTROLLER_CREDS_SECRET) >/dev/null 2>&1; then
	  echo "Secret \(CONTROLLER_CREDS_SECRET) already exists in \(CONTROLLER_NAMESPACE); leaving it untouched."
	else
	  kubectl -n \(CONTROLLER_NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(CONTROLLER_CREDS_SECRET)
	  namespace: \(CONTROLLER_NAMESPACE)
	stringData:
	  \(CONTROLLER_CREDS_CLIENT_ID_KEY): "\(CONTROLLER_CLIENT_ID)"
	  \(CONTROLLER_CREDS_CLIENT_SECRET_KEY): "${CLIENT_SECRET}"
	EOF
	  echo "Secret \(CONTROLLER_CREDS_SECRET) created in \(CONTROLLER_NAMESPACE)."
	fi
	"""

let CONTROLLER_CREDS_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   CONTROLLER_CREDS_BOOTSTRAP_METADATA
}

// One Role/RoleBinding per namespace the Job writes into (keycloak and
// holos-controller).  get is scoped to the one Secret name per namespace; create
// cannot be scoped by resourceName, so it is namespace-wide on secrets — the
// QUAY_OIDC_BOOTSTRAP_ROLE precedent.  The #secret parameter scopes the get
// grant to exactly the Secret this Job manages in that namespace.
let CONTROLLER_CREDS_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	#namespace: string
	#secret:    string
	metadata: {
		name:      "\(CONTROLLER_CREDS_BOOTSTRAP)-\(#namespace)"
		namespace: #namespace
		labels: "app.kubernetes.io/name": CONTROLLER_CREDS_BOOTSTRAP
	}
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [#secret]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	#namespace: string
	metadata: {
		name:      "\(CONTROLLER_CREDS_BOOTSTRAP)-\(#namespace)"
		namespace: #namespace
		labels: "app.kubernetes.io/name": CONTROLLER_CREDS_BOOTSTRAP
	}
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     "\(CONTROLLER_CREDS_BOOTSTRAP)-\(#namespace)"
	}
	// The ServiceAccount lives in the keycloak namespace; the holos-controller
	// RoleBinding references it cross-namespace, granting the Job write of the one
	// credential Secret there (the QUAY_OIDC cross-namespace precedent).
	subjects: [{
		kind:      "ServiceAccount"
		name:      CONTROLLER_CREDS_BOOTSTRAP
		namespace: NAMESPACE
	}]
}

let CONTROLLER_CREDS_BOOTSTRAP_ROLE_KEYCLOAK = CONTROLLER_CREDS_BOOTSTRAP_ROLE & {#namespace: NAMESPACE, #secret: CONTROLLER_CLIENT_SECRET}
let CONTROLLER_CREDS_BOOTSTRAP_ROLE_CONTROLLER = CONTROLLER_CREDS_BOOTSTRAP_ROLE & {#namespace: CONTROLLER_NAMESPACE, #secret: CONTROLLER_CREDS_SECRET}
let CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING_KEYCLOAK = CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING & {#namespace: NAMESPACE}
let CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING_CONTROLLER = CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING & {#namespace: CONTROLLER_NAMESPACE}

// Deleted and recreated on every apply by pre_keycloak_config (its own
// app.kubernetes.io/name label) so it always re-runs — a forward change, a prior
// Failed run, and a routine re-apply all get a fresh Job; the Secrets survive the
// Job deletion, so the generate-once guarantee holds (the QUAY_OIDC_BOOTSTRAP_JOB
// precedent).
let CONTROLLER_CREDS_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   CONTROLLER_CREDS_BOOTSTRAP_METADATA
	spec: {
		backoffLimit:            3
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: CONTROLLER_CREDS_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: CONTROLLER_CREDS_BOOTSTRAP
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
					command: ["/bin/sh", "-c", CONTROLLER_CREDS_BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME; point it at
					// the writable emptyDir since the root filesystem is read-only.
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
						// HOL-1294: the realm-user password bootstrap.
						(PASSWORD_BOOTSTRAP_JOB.metadata.name): PASSWORD_BOOTSTRAP_JOB
						// HOL-1348: the controller's Keycloak admin credential bootstrap.
						(CONTROLLER_CREDS_BOOTSTRAP_JOB.metadata.name): CONTROLLER_CREDS_BOOTSTRAP_JOB
					}
					ServiceAccount: {
						(QUAY_OIDC_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): QUAY_OIDC_BOOTSTRAP_SERVICE_ACCOUNT
						(PASSWORD_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name):  PASSWORD_BOOTSTRAP_SERVICE_ACCOUNT
						// HOL-1348: the controller-creds bootstrap ServiceAccount.
						(CONTROLLER_CREDS_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): CONTROLLER_CREDS_BOOTSTRAP_SERVICE_ACCOUNT
					}
					// The QUAY_OIDC bootstrap needs one Role/RoleBinding per
					// namespace it writes the quay-oidc Secret into (keycloak and
					// quay); the password bootstrap writes only in keycloak, so it
					// adds a single same-namespace pair; the controller-creds
					// bootstrap (HOL-1348) writes into keycloak and holos-controller,
					// so it adds one pair per namespace.  Keyed by a distinct name so
					// the entries do not collide in this map.
					Role: {
						(NAMESPACE):          QUAY_OIDC_BOOTSTRAP_ROLE_KEYCLOAK
						(QUAY_NAMESPACE):     QUAY_OIDC_BOOTSTRAP_ROLE_QUAY
						(PASSWORD_BOOTSTRAP): PASSWORD_BOOTSTRAP_ROLE
						(CONTROLLER_CREDS_BOOTSTRAP_ROLE_KEYCLOAK.metadata.name):   CONTROLLER_CREDS_BOOTSTRAP_ROLE_KEYCLOAK
						(CONTROLLER_CREDS_BOOTSTRAP_ROLE_CONTROLLER.metadata.name): CONTROLLER_CREDS_BOOTSTRAP_ROLE_CONTROLLER
					}
					RoleBinding: {
						(NAMESPACE):          QUAY_OIDC_BOOTSTRAP_ROLE_BINDING_KEYCLOAK
						(QUAY_NAMESPACE):     QUAY_OIDC_BOOTSTRAP_ROLE_BINDING_QUAY
						(PASSWORD_BOOTSTRAP): PASSWORD_BOOTSTRAP_ROLE_BINDING
						(CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING_KEYCLOAK.metadata.name):   CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING_KEYCLOAK
						(CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING_CONTROLLER.metadata.name): CONTROLLER_CREDS_BOOTSTRAP_ROLE_BINDING_CONTROLLER
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

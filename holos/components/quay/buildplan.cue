package holos

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// quay renders the Quay registry as a plain Deployment backed by the
// quay-db CNPG Postgres Cluster (components/cnpg-clusters) and a minimal
// single-pod Redis, with registry blob storage on a local-path PVC, exposed
// at https://quay.holos.localhost through the shared Gateway
// (components/istio-gateway).  This component brings up the UI and the v2
// registry API.  The superuser credential is bootstrapped automatically by
// the admin-bootstrap Job below (HOL-1276), which seeds the first user through
// the /api/v1/user/initialize endpoint (enabled by FEATURE_USER_INITIALIZE)
// and stores the returned superuser OAuth token in the quay-initial-admin
// Secret — the credential declarative automation Jobs authenticate with.  The
// org/robot/team scaffolding is bootstrapped separately by scripts/quay-init
// (HOL-1177), which reuses that same Secret.
//
// Quay reads /conf/stack/config.yaml and performs no environment-variable
// substitution, and the database credentials are CNPG-generated (never
// committed), so the committed ConfigMap holds a config *template* and an
// initContainer renders the real config at pod start: it substitutes the
// DB URI (from the quay-db-app Secret — the contract in holos/README.md
// "Postgres credentials and connection contract") and the two secret keys
// (from the quay-secret-keys Secret the bootstrap Job below creates) into
// the template and writes the result to an emptyDir shared with the main
// container at /conf/stack.
//
// The quay Namespace — including its ambient mesh enrollment label — is
// registered in the central namespaces registry (holos/namespaces.cue) and
// rendered by the namespaces component.

// VERSION pins the Quay registry image tag.  quay.io/projectquay/quay:3.17.3
// is an OCI index whose platforms are linux/{amd64,arm64,ppc64le,s390x} —
// linux/arm64 is required because the cluster is k3d on OrbStack/Apple
// silicon — verified 2026-06-12 via the quay.io registry API (the manifest
// list for the tag).  The image config declares USER 1001 (non-root) and
// exposes 8080, the plain-HTTP port the Service and probes below use.
// Before bumping, re-check the new tag's manifest list still includes
// linux/arm64 and that the image still serves HTTP on 8080 with the
// /health/instance endpoint.
let VERSION = "3.17.3"

// REDIS_VERSION pins the Redis image for Quay's ephemeral state.  The
// official docker.io/library/redis 8.6.4-alpine tag is a manifest list
// including linux/arm64 (checked 2026-06-12 via the Docker Hub registry
// API); 8.6 is the current mature stable line.  Quay uses Redis only for
// build logs and user events — both ephemeral — so no persistence is
// configured and any stable multi-arch tag works.  Re-check the tag's
// manifest list includes linux/arm64 before bumping.
let REDIS_VERSION = "8.6.4-alpine"

// KUBECTL_IMAGE pins the image the secret-keys bootstrap Job runs kubectl
// from.  docker.io/alpine/kubectl:1.33.3 is a manifest list including
// linux/arm64 (checked 2026-06-12 via the Docker Hub registry API) and is
// alpine-based, providing the /bin/sh the Job script needs (the
// version-matched rancher/kubectl image is scratch-based — no shell).
// 1.33.3 is the oldest tag the repository publishes; it exceeds the
// kubectl +/-1 minor skew recommendation against the live server
// (v1.31.5+k3s1) but the Job performs only core/v1 Secret get/create,
// which are version-stable.  Re-check available tags against the cluster
// version before bumping.
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure:
// if "quay" is ever removed or renamed in holos/namespaces.cue, rendering
// fails here instead of at apply time with a NotFound namespace error.
let NAMESPACE = "quay" & #RegisteredNamespace
let NAME = "quay"
let PORT = 8080

// quay.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md, and the Keycloak realm's reconciled quay client
// (managed by the keycloak-config Job) lists https://quay.holos.localhost/*
// as a redirect URI.
// k3d-registry.holos.localhost is deliberately NOT used: that name belongs to
// the k3d bootstrap registry on port 5000 (scripts/local-k3d).
let HOSTNAME = "quay.holos.localhost"

// The shared Gateway's namespace and name (components/istio-gateway).
// GATEWAY_NAME feeds both the HTTPRoute parentRefs and the ServiceEntry
// endpoint below, keeping this component's references to the Gateway
// mutually consistent.  Nothing ties the literal to the istio-gateway
// component at render time, so a Gateway rename still surfaces only at
// runtime — update both components together.
let GATEWAY_NAMESPACE = "istio-gateways"
let GATEWAY_NAME = "default"

// OIDC / Keycloak SSO wiring (HOL-1219).  ISSUER_HOSTNAME is the Keycloak
// hostname (components/keycloak/instance); OIDC_SERVER is the holos realm
// issuer URL.  The trailing slash is REQUIRED: Quay's config validator
// normalises the issuer to TrimSuffix(issuer,"/")+"/" and compares it against
// Keycloak's published issuer, so the value here must carry the slash to match.
// OIDC_CLIENT_ID is the confidential "quay" client managed in the realm
// (components/keycloak/realm-config; HOL-1218).
let ISSUER_HOSTNAME = "auth.holos.localhost"
let OIDC_SERVER = "https://\(ISSUER_HOSTNAME)/realms/holos/"
let OIDC_CLIENT_ID = "quay"

// OIDC_SECRET is the shared client-secret Secret HOL-1218's bootstrap Job
// provisioned into BOTH the keycloak and quay namespaces; OIDC_SECRET_KEY is
// the data key holding the client secret.  The initContainer reads it and
// substitutes it into the config template's __OIDC_CLIENT_SECRET__ placeholder
// at pod start, so the secret value is never committed.
let OIDC_SECRET = "quay-oidc"
let OIDC_SECRET_KEY = "client_secret"

// CA_CERT_SECRET carries the local-ca root certificate in its ca.crt key.
// Quay performs its OIDC discovery/JWKS/token calls to OIDC_SERVER
// (https://auth.holos.localhost) server-side with TLS verification ON and has
// no per-OIDC "insecure skip verify" knob (unlike Argo CD), so it must trust
// the local CA that signed the shared Gateway's *.holos.localhost certificate.
// A cert-manager Certificate issued by the local-ca ClusterIssuer
// (components/local-ca) writes this Secret into the quay namespace with the
// signing CA in ca.crt; the Quay container mounts that key under
// /conf/stack/extra_ca_certs so the Quay entrypoint installs it into the
// system trust bundle on start.  Mounting a per-namespace cert-manager Secret
// (rather than the Gateway's wildcard-holos-localhost Secret, which lives in
// the istio-gateways namespace) keeps the trust anchor local to this pod's
// namespace — a pod can only mount Secrets from its own namespace.
let CA_CERT_SECRET = "quay-local-ca"
let CA_CERT_KEY = "ca.crt"

let REDIS_NAME = "quay-redis"
let REDIS_PORT = 6379

// SECRET_KEYS is the Secret the bootstrap Job creates and the Quay pod's
// initContainer reads; PVC_NAME is the registry blob storage claim.
let SECRET_KEYS = "quay-secret-keys"
let PVC_NAME = "quay-datastorage"
let CONFIG_TEMPLATE = "quay-config-template"
let BOOTSTRAP = "quay-secret-keys-bootstrap"

// The Quay superuser bootstrap credential (HOL-1276).  ADMIN_SECRET is the
// Secret the admin-bootstrap Job below creates once and never rotates: its
// `token` key holds a non-expiring superuser OAuth token, the credential that
// declarative automation Jobs (e.g. the my-project-quay-bootstrap Job that
// provisions an org/repo/webhook) authenticate to Quay's REST API with.  The
// Job is the in-cluster, automated equivalent of step 1 of scripts/quay-init:
// it POSTs to the one-shot /api/v1/user/initialize endpoint (enabled by
// FEATURE_USER_INITIALIZE in the config above) — Quay's recommended way to
// seed a first superuser headlessly in Kubernetes — and stores the returned
// token.  This is what fixes "secrets quay-initial-admin not found": the
// credential is now created by `scripts/apply` rather than by a manual
// scripts/quay-init run.  See CLAUDE.md "Quay Superuser Bootstrap Credential".
let ADMIN_SECRET = "quay-initial-admin"
let ADMIN_BOOTSTRAP = "quay-admin-bootstrap"
let ADMIN_USER = "admin"

// Matches the ADMIN_EMAIL in scripts/quay-init so both bootstrap paths agree
// on the admin identity (the domain is the bare local domain, not the quay
// subdomain in HOSTNAME).
let ADMIN_EMAIL = "admin@holos.localhost"

// The Job reaches Quay over the plain-HTTP in-cluster Service (no local-CA
// trust needed — the quay-local-ca cert is only for callers using the public
// https://quay.holos.localhost hostname), the same convention declarative
// bootstrap Jobs follow (CLAUDE.md, the Quay org/repo/webhook bootstrap
// convention).
let QUAY_SERVICE_URL = "http://\(NAME).\(NAMESPACE).svc.cluster.local:\(PORT)"

let METADATA = {
	name:      NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": NAME
}

let REDIS_METADATA = {
	name:      REDIS_NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": REDIS_NAME
}

// The config template the initContainer renders into /conf/stack/config.yaml.
// The __TOKEN__ placeholders are replaced with JSON-encoded values (JSON
// strings are valid YAML scalars), so credential values never need to be
// YAML-escaped by hand and never appear in this repository.
//
// Field notes:
//   - EXTERNAL_TLS_TERMINATION: the shared Gateway terminates TLS; Quay
//     itself serves plain HTTP on 8080.
//   - BUILDLOGS_REDIS and USER_EVENTS_REDIS are both mandatory even though
//     the build feature is unused.
//   - FEATURE_USER_INITIALIZE enables the one-shot /api/v1/user/initialize
//     endpoint the admin-bootstrap Job (HOL-1276) uses to create the admin user.
//   - SETUP_COMPLETE skips the interactive setup flow.
//
// Database backend + federated Keycloak SSO (HOL-1280, refining HOL-1219 /
// Phase 2 of HOL-1217):
//   - AUTHENTICATION_TYPE Database keeps Quay's own database as the identity
//     store while KEYCLOAK_LOGIN_CONFIG layers Keycloak SSO on top as a
//     *federated login provider* — it does NOT replace the backend.  This is
//     the key distinction from AUTHENTICATION_TYPE OIDC, which makes the OIDC
//     provider the sole identity store: under OIDC the local "admin" user and
//     the /api/v1/user/initialize + /api/v1/superuser/* APIs are unavailable,
//     so the quay-admin-bootstrap Job (HOL-1276) cannot mint the superuser
//     OAuth token.  Database auth restores those endpoints, and the very same
//     KEYCLOAK_LOGIN_CONFIG block still drives "Holos SSO" login (Quay treats
//     a <PREFIX>_LOGIN_CONFIG block as a federated SSO provider regardless of
//     the backend) — so SSO and the headless superuser bootstrap coexist.
//   - KEYCLOAK_LOGIN_CONFIG points Quay at the holos realm's confidential
//     "quay" client (HOL-1218, in components/keycloak/realm-config), which
//     authenticates with a client secret and does NOT use PKCE (see the PKCE
//     field note below).  OIDC_SERVER is the realm issuer URL with a REQUIRED
//     trailing slash — Quay's config validator normalises the issuer to
//     TrimSuffix(issuer,"/")+"/", so the slash must be present here to match
//     Keycloak's issuer exactly.
//   - CLIENT_SECRET is the __OIDC_CLIENT_SECRET__ placeholder; the
//     initContainer substitutes it from the shared quay-oidc Secret (the
//     client_secret key) that Phase 1's bootstrap Job provisioned into BOTH
//     the keycloak and quay namespaces — so this phase only consumes it and
//     no secret value is ever committed.
//   - FEATURE_DIRECT_LOGIN false removes the local username/password form so
//     Keycloak SSO ("Holos SSO") is the only interactive login path even though
//     the backend is Database (the local "admin" superuser stays reachable for
//     the API/bootstrap path — see SUPER_USERS below); FEATURE_USER_CREATION
//     true lets first SSO login auto-provision the user's account namespace
//     (a Quay user's personal namespace IS their per-user org scope).
//   - FEATURE_USERNAME_CONFIRMATION false is the key requirement from the
//     issue: the username is taken verbatim from PREFERRED_USERNAME_CLAIM_NAME
//     (preferred_username) with no prompt to choose or edit it.
//   - PKCE is deliberately NOT used: Quay authenticates as a confidential
//     client with a client secret (CLIENT_SECRET above), Red Hat's recommended
//     baseline OIDC integration, so USE_PKCE/PKCE_METHOD are omitted (Quay
//     defaults USE_PKCE to false and sends no code_challenge).  Quay's OIDC
//     client did not reliably round-trip a PKCE code_verifier at the token
//     endpoint, producing a "code exchange: 400" SSO failure; dropping PKCE on
//     both ends (the Keycloak quay client no longer sets
//     pkce.code.challenge.method either) removes that failure mode.  This is the
//     documented exception to the platform's PKCE default — see the
//     Quay↔Keycloak OIDC runbook (HOL-1256 docs phase / HOL-1233).
//   - FEATURE_TEAM_SYNCING is false (and TEAM_RESYNC_STALE_TIME is dropped):
//     team sync from the OIDC groups claim REQUIRES a federated auth backend
//     that owns sync_user_groups.  Under AUTHENTICATION_TYPE Database the active
//     handler is DatabaseUsers, which has no sync_user_groups method (only
//     OIDCUsers/LDAPUsers do).  Quay's OAuth login path calls
//     sync_oidc_groups() on every SSO callback when FEATURE_TEAM_SYNCING is
//     true with a groups claim present, so leaving it on would AttributeError →
//     500 on every "Holos SSO" login (Quay v3.17.3 oauth/login_utils.py
//     sync_oidc_groups → auth_system.sync_user_groups; data/users/database.py
//     DatabaseUsers).  Federated group→team sync therefore returns only if a
//     future phase moves to a federated backend; for now superuser comes solely
//     from SUPER_USERS below, and Quay teams are managed directly.
//   - SUPER_USERS keeps the local "admin" entry the admin-bootstrap Job
//     (HOL-1276) seeds — it stays reachable because Quay always
//     permits superuser local login even with FEATURE_DIRECT_LOGIN false.  The
//     holos realm seeds no users yet, so there is no realm preferred_username to
//     grant SSO superuser to; a Keycloak-backed platform admin is bootstrapped
//     by seeding a realm user and appending their preferred_username here (the
//     deferred item in the PR).  The quay "platform-admin" client role drives
//     Quay *team* membership via the groups claim, not superuser.
let CONFIG_YAML = """
	SERVER_HOSTNAME: \(HOSTNAME)
	PREFERRED_URL_SCHEME: https
	EXTERNAL_TLS_TERMINATION: true
	SETUP_COMPLETE: true
	DB_URI: __DB_URI__
	SECRET_KEY: __SECRET_KEY__
	DATABASE_SECRET_KEY: __DATABASE_SECRET_KEY__
	BUILDLOGS_REDIS:
	  host: \(REDIS_NAME)
	  port: \(REDIS_PORT)
	USER_EVENTS_REDIS:
	  host: \(REDIS_NAME)
	  port: \(REDIS_PORT)
	DISTRIBUTED_STORAGE_CONFIG:
	  default:
	    - LocalStorage
	    - storage_path: /datastorage/registry
	DISTRIBUTED_STORAGE_PREFERENCE:
	  - default
	FEATURE_USER_INITIALIZE: true
	FEATURE_USER_CREATION: true
	FEATURE_DIRECT_LOGIN: false
	FEATURE_USERNAME_CONFIRMATION: false
	FEATURE_TEAM_SYNCING: false
	AUTHENTICATION_TYPE: Database
	KEYCLOAK_LOGIN_CONFIG:
	  OIDC_SERVER: \(OIDC_SERVER)
	  CLIENT_ID: \(OIDC_CLIENT_ID)
	  CLIENT_SECRET: __OIDC_CLIENT_SECRET__
	  SERVICE_NAME: Holos SSO
	  LOGIN_SCOPES:
	    - openid
	    - profile
	    - email
	    - groups
	    - offline_access
	  PREFERRED_USERNAME_CLAIM_NAME: preferred_username
	  VERIFIED_EMAIL_CLAIM_NAME: email
	  PREFERRED_GROUP_CLAIM_NAME: groups
	SUPER_USERS:
	  - admin
	  - jeff
	  - platform-owner
	FEATURE_MAILING: false
	"""

// The initContainer script: render the config, then prepare the database.
// json.dumps the env values so any character CNPG or the bootstrap Jobs
// might generate (DB URI, secret keys, and the OIDC client secret) stays a
// valid YAML scalar; python3 and psycopg2 ship in the Quay image, which the
// initContainer reuses to avoid a second pin.
//
// Quay's config validator refuses to start without the pg_trgm extension
// in its database.  pg_trgm is a *trusted* extension (PostgreSQL 13+), so
// the CNPG-generated app user — owner of the quay database via initdb
// (components/cnpg-clusters) — can CREATE it without superuser.  Creating
// it here keeps the dependency level-triggered: it converges on an
// already-bootstrapped Cluster exactly like a fresh one, with no manual
// step and no bootstrap-only hook.
let INIT_SCRIPT = """
	import json, os

	import psycopg2

	template = open("/conf/template/config.yaml").read()
	for key in ("DB_URI", "SECRET_KEY", "DATABASE_SECRET_KEY", "OIDC_CLIENT_SECRET"):
	    template = template.replace("__%s__" % key, json.dumps(os.environ[key]))
	open("/conf/stack/config.yaml", "w").write(template)

	conn = psycopg2.connect(os.environ["DB_URI"])
	conn.autocommit = True
	conn.cursor().execute("CREATE EXTENSION IF NOT EXISTS pg_trgm")
	conn.close()
	"""

// The create-if-absent bootstrap script for the quay-secret-keys Secret.
// DATABASE_SECRET_KEY encrypts fields inside Postgres, so it MUST be stable
// across restarts and MUST NOT be committed: the Job generates it once and
// never touches an existing Secret.  Keys are alphanumeric (base64 with
// +/=/newlines stripped) so they are safe in any downstream context.  The
// length checks guard against an improbable pipeline failure under set -eu
// (no pipefail in busybox sh) silently creating empty keys — empty keys
// would otherwise become permanent by the create-if-absent design.  The
// Secret is piped as a manifest on stdin so the key material never appears
// in the container's argv (/proc-visible).
let BOOTSTRAP_SCRIPT = """
	set -eu
	if kubectl -n \(NAMESPACE) get secret \(SECRET_KEYS) >/dev/null 2>&1; then
	  echo "Secret \(SECRET_KEYS) already exists; leaving it untouched."
	  exit 0
	fi
	random_key() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	SECRET_KEY="$(random_key)"
	DATABASE_SECRET_KEY="$(random_key)"
	[ "${#SECRET_KEY}" -eq 48 ]
	[ "${#DATABASE_SECRET_KEY}" -eq 48 ]
	kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(SECRET_KEYS)
	  namespace: \(NAMESPACE)
	stringData:
	  SECRET_KEY: "${SECRET_KEY}"
	  DATABASE_SECRET_KEY: "${DATABASE_SECRET_KEY}"
	EOF
	echo "Secret \(SECRET_KEYS) created."
	"""

let CONFIG_TEMPLATE_CM = {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: {
		name:      CONFIG_TEMPLATE
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	data: "config.yaml": CONFIG_YAML
}

// CONFIG_HASH is a short content hash of the rendered quay-config-template
// ConfigMap (the CONFIG_YAML template the initContainer renders into
// /conf/stack/config.yaml).  It is stamped onto the Quay Deployment pod
// template as the app.holos.run/config-hash annotation (on DEPLOYMENT below) so
// any edit to CONFIG_YAML changes the pod template, forcing a new ReplicaSet and
// a rollout on the next scripts/apply.
//
// Without it the ConfigMap name is static and the pod template is byte-identical
// across a config-only change, so kubectl apply updates the ConfigMap but never
// rolls the Deployment — the running pod keeps serving the stale config until it
// is manually restarted (HOL-1260).  This mirrors the CONFIG_HASH precedent in
// the keycloak-config Job (components/keycloak/realm-config) but stamps an
// annotation rather than renaming a resource, the more common Kubernetes idiom,
// which avoids leaving orphaned old-named ConfigMaps behind.
//
// The hash is over CONFIG_TEMPLATE_CM.data["config.yaml"] alone — the only
// content the volume mounts — so it is stable across re-renders (scripts/render
// stays diff-clean) and changes only when the config content does.  8 hex chars
// (32 bits) is ample for a change-detection annotation.
let CONFIG_HASH = strings.SliceRunes(hex.Encode(sha256.Sum256(
CONFIG_TEMPLATE_CM.data["config.yaml"])), 0, 8)

// The bootstrap resources carry their own app.kubernetes.io/name — NOT the
// Quay Deployment's — because the quay Service selects on that label: a
// probe-less bootstrap pod labeled like the Quay pod would become a dead
// Service endpoint for the seconds it runs whenever the Job re-runs after
// TTL garbage collection.
let BOOTSTRAP_METADATA = {
	name:      BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": BOOTSTRAP
}

let BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   BOOTSTRAP_METADATA
}

// Scoped to the one Secret the Job manages: get is restricted to the
// quay-secret-keys resourceName; create cannot be restricted by
// resourceName (the API server does not evaluate resourceNames for create
// requests), so the create grant is namespace-wide on secrets — acceptable
// in a namespace whose Secrets all belong to this service.
let BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [SECRET_KEYS]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      BOOTSTRAP
		namespace: NAMESPACE
	}]
}

// CAVEAT: a completed Job's pod template is immutable.  Server-side
// re-apply of this unchanged spec is a no-op while the Job exists, and
// ttlSecondsAfterFinished garbage-collects it a day after completion —
// after that a re-apply recreates the Job, which exits 0 against the
// existing Secret.  Only a pod-template change within the TTL window
// requires deleting the old Job first
// (kubectl -n quay delete job quay-secret-keys-bootstrap) — the Secret it
// created survives, and the new Job exits 0 without touching it.
let BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   BOOTSTRAP_METADATA
	spec: {
		backoffLimit: 3
		// A day keeps the Job's logs around for debugging a fresh
		// bootstrap while still dissolving the immutable-pod-template
		// caveat above for routine re-applies.
		ttlSecondsAfterFinished: 86400
		template: {
			// The distinct label matters here most of all: the quay
			// Service must never select this pod (see BOOTSTRAP_METADATA).
			metadata: labels: BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// The alpine/kubectl image declares no non-root USER;
					// pick the conventional "nobody" uid.
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "bootstrap"
					image: KUBECTL_IMAGE
					command: ["/bin/sh", "-c", BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME; point
					// it at the writable emptyDir since the root filesystem
					// is read-only.
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

// The admin-bootstrap script: create the quay-initial-admin Secret holding a
// superuser OAuth token (HOL-1276).  It is the automated, in-cluster
// equivalent of step 1 of scripts/quay-init — Quay has no operator to seed a
// first user, so the upstream-recommended path is the one-shot
// /api/v1/user/initialize endpoint (enabled by FEATURE_USER_INITIALIZE).
//
// Generate-once and idempotent, mirroring BOOTSTRAP_SCRIPT: the initialize
// endpoint only answers while the registry has no users, so the Secret is
// never rotated — an existing Secret is left untouched and a re-run exits 0.
// Unlike the secret-keys Job (which must run BEFORE the Quay pod, supplying
// keys the initContainer needs), this Job runs AFTER Quay is serving, so it
// self-gates on /health/instance before initializing — the first pod start
// runs the database migrations and can take minutes.
//
// The alpine/kubectl image ships curl but not jq, so the OAuth token is
// extracted from the flat JSON response with grep/sed; Quay tokens are
// alphanumeric, carrying no commas or quotes to confuse the parse.  The
// request body and the Secret are staged via heredocs (no values in argv,
// /proc-visible) and the password rides in stringData (no manual base64).
//
// Crash window: the initialize endpoint is one-shot, so a token obtained but
// not stored is lost for good (recovery is a registry reset).  The Secret is
// written immediately after the token is obtained; if that write fails the Job
// fails with reset guidance but does NOT print the credential — the token is a
// non-expiring superuser credential and pod logs (readable by any principal
// with log access) would bypass the Secret's RBAC.  This is the one place this
// Job is deliberately stricter than scripts/quay-init, whose last-resort dump
// targets an operator's terminal rather than durable cluster logs.
let ADMIN_BOOTSTRAP_SCRIPT = """
	set -eu
	# --ignore-not-found prints nothing and exits 0 when the Secret is absent,
	# while any OTHER kubectl failure (RBAC denial, API outage) exits non-zero
	# and aborts via set -e — so "missing" is never conflated with "broken",
	# which matters before consuming the one-shot initialize endpoint (the
	# scripts/quay-init precedent).  Suppressing stderr (2>&1 >/dev/null) inside
	# an `if` would mask that distinction.
	EXISTING="$(kubectl -n \(NAMESPACE) get secret \(ADMIN_SECRET) -o name --ignore-not-found)"
	if [ -n "${EXISTING}" ]; then
	  echo "Secret \(ADMIN_SECRET) already exists; leaving it untouched."
	  exit 0
	fi
	echo "Waiting for Quay to answer at \(QUAY_SERVICE_URL)/health/instance ..."
	i=0
	until curl -fsS --max-time 10 -o /dev/null "\(QUAY_SERVICE_URL)/health/instance"; do
	  i=$((i + 1))
	  if [ "${i}" -ge 120 ]; then
	    echo "ERROR: Quay did not become healthy in time." >&2
	    exit 1
	  fi
	  sleep 10
	done
	# Superuser local login stays reachable even with FEATURE_DIRECT_LOGIN
	# false, so this 48-char alphanumeric password is the break-glass UI
	# credential; the OAuth token below is what bootstrap Jobs consume.
	PASSWORD="$(head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48)"
	[ "${#PASSWORD}" -eq 48 ]
	cat > /tmp/initialize-request.json <<EOF
	{"username": "\(ADMIN_USER)", "password": "${PASSWORD}", "email": "\(ADMIN_EMAIL)", "access_token": true}
	EOF
	# The canonical Host header makes any server-side host validation see Quay's
	# SERVER_HOSTNAME even though the request rides the in-cluster Service.
	HTTP="$(curl -sS --max-time 30 -o /tmp/initialize.json -w '%{http_code}' \\
	  -X POST -H 'Content-Type: application/json' -H 'Host: \(HOSTNAME)' \\
	  --data @/tmp/initialize-request.json \\
	  "\(QUAY_SERVICE_URL)/api/v1/user/initialize")"
	if [ "${HTTP}" != 200 ]; then
	  echo "ERROR: /api/v1/user/initialize returned HTTP ${HTTP}:" >&2
	  cat /tmp/initialize.json >&2 || true
	  echo >&2
	  echo "       The endpoint only works while the registry has no users.  If Quay" >&2
	  echo "       was initialized outside this Job there is no recoverable token —" >&2
	  echo "       reset the registry state and re-run (see docs/local-cluster.md)." >&2
	  exit 1
	fi
	TOKEN="$(grep -o '"access_token"[^,}]*' /tmp/initialize.json | head -1 | sed 's/^.*"access_token"//; s/^[^"]*"//; s/".*$//')"
	if [ -z "${TOKEN}" ]; then
	  echo "ERROR: initialize succeeded but no access_token was in the response." >&2
	  exit 1
	fi
	# Store the credential immediately.  Piped on stdin as stringData so the
	# values never appear in the container's argv (/proc-visible) and need no
	# manual base64.  The token must NEVER be echoed to the pod log: it is a
	# non-expiring superuser credential, and pod logs are readable by any
	# principal with log access, bypassing the Secret's RBAC.  So on a write
	# failure we surface the failure and the recovery path (a registry reset,
	# since the one-shot token is unrecoverable) WITHOUT printing the secret —
	# unlike scripts/quay-init, whose output goes to an operator's terminal, not
	# to durable cluster logs.
	if kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(ADMIN_SECRET)
	  namespace: \(NAMESPACE)
	stringData:
	  username: "\(ADMIN_USER)"
	  password: "${PASSWORD}"
	  token: "${TOKEN}"
	EOF
	then
	  echo "Created admin user '\(ADMIN_USER)' and stored Secret \(ADMIN_SECRET)."
	else
	  cat >&2 <<RECOVERY
	ERROR: the admin user was initialized but storing Secret \(ADMIN_SECRET) failed.
	       The /api/v1/user/initialize endpoint is one-shot, so the superuser token
	       cannot be recovered — and it must not be printed to the pod log.  Reset
	       the registry state and re-run scripts/apply (see docs/local-cluster.md).
	RECOVERY
	  exit 1
	fi
	"""

let ADMIN_BOOTSTRAP_METADATA = {
	name:      ADMIN_BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": ADMIN_BOOTSTRAP
}

let ADMIN_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   ADMIN_BOOTSTRAP_METADATA
}

// Scoped to the one Secret the Job manages, like BOOTSTRAP_ROLE: get is
// restricted to the quay-initial-admin resourceName; create cannot be
// restricted by resourceName (the API server does not evaluate resourceNames
// for create), so the create grant is namespace-wide on secrets — acceptable
// in a namespace whose Secrets all belong to this service.
let ADMIN_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   ADMIN_BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [ADMIN_SECRET]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let ADMIN_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   ADMIN_BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     ADMIN_BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      ADMIN_BOOTSTRAP
		namespace: NAMESPACE
	}]
}

// Same immutable-pod-template caveat as BOOTSTRAP_JOB: a re-apply is a no-op
// while the Job exists and ttlSecondsAfterFinished GCs it a day after
// completion.  scripts/apply's pre_quay hook deletes this Job before every
// apply so a Failed run (which would leave no Secret) is always re-attempted
// rather than skipped — the pre_keycloak_config precedent.  backoffLimit is
// higher than the secret-keys Job's because this one races Quay's first start;
// each pod self-waits on /health/instance, but a pod evicted mid-wait should
// still get fresh attempts.
let ADMIN_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   ADMIN_BOOTSTRAP_METADATA
	spec: {
		backoffLimit:            6
		ttlSecondsAfterFinished: 86400
		template: {
			// The distinct label keeps this pod out of the quay Service's
			// endpoints (see BOOTSTRAP_METADATA).
			metadata: labels: ADMIN_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: ADMIN_BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// The alpine/kubectl image declares no non-root USER;
					// pick the conventional "nobody" uid.
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "bootstrap"
					image: KUBECTL_IMAGE
					command: ["/bin/sh", "-c", ADMIN_BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME, and the
					// script stages request/response files there; point HOME at
					// the writable emptyDir since the root filesystem is
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

let REDIS_DEPLOYMENT = {
	apiVersion: "apps/v1"
	kind:       "Deployment"
	metadata:   REDIS_METADATA
	spec: {
		replicas: 1
		selector: matchLabels: REDIS_METADATA.labels
		template: {
			metadata: labels: REDIS_METADATA.labels
			spec: {
				// Redis never talks to the Kubernetes API; don't mount a
				// ServiceAccount token it has no use for.
				automountServiceAccountToken: false
				securityContext: {
					runAsNonRoot: true
					// The official redis alpine image creates the redis user
					// as uid 999 in group 1000.
					runAsUser:  999
					runAsGroup: 1000
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "redis"
					image: "docker.io/library/redis:\(REDIS_VERSION)"
					// Quay uses Redis only for build logs and user events —
					// both ephemeral — so snapshotting and AOF are disabled
					// explicitly; nothing needs to survive a restart.  No
					// auth: the Service is in-cluster only and nothing
					// sensitive transits it.
					args: ["redis-server", "--save", "", "--appendonly", "no"]
					ports: [{
						name:          "redis"
						containerPort: REDIS_PORT
						protocol:      "TCP"
					}]
					// TCP probes suffice for a single-purpose cache: redis
					// accepts connections only once it is serving.
					readinessProbe: tcpSocket: port: REDIS_PORT
					livenessProbe: tcpSocket: port:  REDIS_PORT
					// Laptop sizing: an ephemeral cache for one Quay
					// instance idles far below these.
					resources: {
						requests: {
							cpu:    "25m"
							memory: "32Mi"
						}
						limits: memory: "128Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					// /data is the image's working directory; with
					// persistence disabled redis writes nothing there, but
					// an emptyDir keeps the read-only root filesystem viable
					// if a future flag change re-enables snapshots.
					volumeMounts: [{
						name:      "data"
						mountPath: "/data"
					}]
				}]
				volumes: [{
					name: "data"
					emptyDir: {}
				}]
			}
		}
	}
}

let REDIS_SERVICE = {
	apiVersion: "v1"
	kind:       "Service"
	metadata:   REDIS_METADATA
	spec: {
		selector: REDIS_METADATA.labels
		ports: [{
			name:       "redis"
			port:       REDIS_PORT
			targetPort: REDIS_PORT
			protocol:   "TCP"
		}]
	}
}

// The Quay Deployment's dedicated identity.  It exists for precision, not
// permissions: it carries no role bindings (the mounted token satisfies
// Quay's KubernetesConfigProvider, which only needs the token file to
// exist), and the Redis AuthorizationPolicy below pins to this principal,
// so a future pod that omits serviceAccountName never silently gains
// Redis access via the namespace default ServiceAccount.
let QUAY_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   METADATA
}

// Redis runs without auth, so make the in-cluster-only claim true by
// construction: the quay namespace is ambient-enrolled
// (holos/namespaces.cue), ztunnel enforces L4 authorization, and this
// policy allows only the Quay pod's identity — the dedicated quay
// ServiceAccount above — to connect.  Kubelet health probes are exempt
// from ambient capture, so the TCP probes above keep working.
let REDIS_AUTHZ = {
	apiVersion: "security.istio.io/v1"
	kind:       "AuthorizationPolicy"
	metadata: {
		name:      REDIS_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": REDIS_NAME
	}
	spec: {
		selector: matchLabels: REDIS_METADATA.labels
		action: "ALLOW"
		rules: [{
			from: [{source: principals: ["cluster.local/ns/\(NAMESPACE)/sa/\(QUAY_SERVICE_ACCOUNT.metadata.name)"]}]
		}]
	}
}

// Registry blob storage.  storageClassName is deliberately omitted: the
// claim binds to the k3s default local-path StorageClass on the local
// cluster (the same pattern as the cnpg-clusters component's Cluster
// storage).  The local-path provisioner creates the backing directory
// world-writable, so the Quay container's uid 1001 can write without an
// fsGroup.
let PVC = {
	apiVersion: "v1"
	kind:       "PersistentVolumeClaim"
	metadata: {
		name:      PVC_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		accessModes: ["ReadWriteOnce"]
		resources: requests: storage: "5Gi"
	}
}

let DEPLOYMENT = {
	apiVersion: "apps/v1"
	kind:       "Deployment"
	metadata:   METADATA
	spec: {
		replicas: 1
		// The RWO local-path volume cannot be shared by the old and new pod
		// of a rolling update; Recreate stops the old pod before the new
		// one claims the volume.
		strategy: type:        "Recreate"
		selector: matchLabels: METADATA.labels
		template: {
			metadata: {
				labels: METADATA.labels
				// Stamp the config content hash onto the pod template so a
				// CONFIG_YAML/ConfigMap-only change rolls the Deployment on the
				// next scripts/apply instead of leaving the running pod on the
				// stale /conf/stack/config.yaml (HOL-1260) — see CONFIG_HASH.
				annotations: "app.holos.run/config-hash": CONFIG_HASH
			}
			spec: {
				// The dedicated ServiceAccount pins the pod's mesh identity
				// for the Redis AuthorizationPolicy.  Its token IS mounted
				// (the default), unlike the other pods in this component:
				// Quay selects its KubernetesConfigProvider whenever
				// KUBERNETES_SERVICE_HOST is set — which the kubelet always
				// injects — and that provider refuses to start without a
				// token file (verified on the live cluster: "Cannot load
				// Kubernetes service account token").  The token is inert:
				// the ServiceAccount has no role bindings, so it grants no
				// API access.
				serviceAccountName: QUAY_SERVICE_ACCOUNT.metadata.name
				// The Quay image config declares USER 1001 (verified with
				// the VERSION pin above), so runAsNonRoot validates without
				// forcing a uid the image layout doesn't expect.
				securityContext: {
					runAsNonRoot: true
					seccompProfile: type: "RuntimeDefault"
				}
				initContainers: [{
					name: "init"
					// Reuse the Quay image: it ships the python3 and
					// psycopg2 the init script needs, avoiding a second pin.
					image: "quay.io/projectquay/quay:\(VERSION)"
					command: ["python3", "-c", INIT_SCRIPT]
					env: [
						{
							name: "DB_URI"
							// The CNPG-generated connection URI:
							// postgresql://user:pass@quay-db-rw.quay:5432/quay
							// per the contract in holos/README.md "Postgres
							// credentials and connection contract".
							valueFrom: secretKeyRef: {
								name: "quay-db-app"
								key:  "uri"
							}
						},
						{
							name: "SECRET_KEY"
							valueFrom: secretKeyRef: {
								name: SECRET_KEYS
								key:  "SECRET_KEY"
							}
						},
						{
							name: "DATABASE_SECRET_KEY"
							valueFrom: secretKeyRef: {
								name: SECRET_KEYS
								key:  "DATABASE_SECRET_KEY"
							}
						},
						{
							// The shared OIDC client secret from the quay-oidc
							// Secret HOL-1218's bootstrap Job wrote into the quay
							// namespace; the init script substitutes it into the
							// config template's __OIDC_CLIENT_SECRET__ placeholder.
							name: "OIDC_CLIENT_SECRET"
							valueFrom: secretKeyRef: {
								name: OIDC_SECRET
								key:  OIDC_SECRET_KEY
							}
						},
					]
					resources: {
						requests: {
							cpu:    "10m"
							memory: "32Mi"
						}
						limits: memory: "128Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					volumeMounts: [
						{
							name:      "config-template"
							mountPath: "/conf/template"
							readOnly:  true
						},
						{
							name:      "config"
							mountPath: "/conf/stack"
						},
					]
				}]
				containers: [{
					name:  NAME
					image: "quay.io/projectquay/quay:\(VERSION)"
					// Quay sizes its gunicorn worker pools from the node's
					// CPU count by default, which on a many-core dev box
					// multiplies its already large per-process footprint.
					// Pin the pools to laptop sizing (ADR-7: a single local
					// instance) — the env vars are the upstream
					// quay-entrypoint contract, the same knobs the Red Hat
					// operator sets.
					env: [
						{
							name:  "WORKER_COUNT_WEB"
							value: "2"
						},
						{
							name:  "WORKER_COUNT_REGISTRY"
							value: "2"
						},
						{
							name:  "WORKER_COUNT_SECSCAN"
							value: "1"
						},
						{
							// The WORKER_COUNT_* pins above are clamped to
							// per-pool minimums in Quay's util/workers.py —
							// the registry pool's minimum is 8, so without
							// this knob WORKER_COUNT_REGISTRY=2 silently runs
							// 8 gunicorn registry workers (~140Mi anon each),
							// which pushed the container to ~4.1Gi anon and
							// repeated OOMKills against the 4Gi limit
							// (observed on the live cluster during the
							// HOL-1178 webhook/restart verification).
							// Lowering the floor to 1 makes the pins above
							// authoritative; "UNSUPPORTED" is upstream's
							// naming for sub-minimum sizing, acceptable for a
							// single-user laptop registry (ADR-7).
							name:  "WORKER_COUNT_UNSUPPORTED_MINIMUM"
							value: "1"
						},
					]
					ports: [{
						name:          "http"
						containerPort: PORT
						protocol:      "TCP"
					}]
					// The first start runs the database schema migrations
					// before the HTTP endpoints serve; the startupProbe
					// gives that up to 10 minutes before the liveness probe
					// takes over.  /health/instance checks the database,
					// Redis, and storage from a heavyweight Python service,
					// so every probe gets an explicit generous timeout
					// instead of the 1s default.
					startupProbe: {
						httpGet: {
							path: "/health/instance"
							port: PORT
						}
						periodSeconds:    10
						failureThreshold: 60
						timeoutSeconds:   5
					}
					readinessProbe: {
						httpGet: {
							path: "/health/instance"
							port: PORT
						}
						timeoutSeconds: 5
					}
					livenessProbe: {
						httpGet: {
							path: "/health/instance"
							port: PORT
						}
						timeoutSeconds: 5
					}
					// Laptop sizing: Quay is heavy — a Python monolith whose
					// supervisord runs ~20 worker processes — so the memory
					// limit is generous relative to the other platform
					// services.  2Gi was not enough: with the default
					// CPU-scaled worker pools the first start was OOMKilled
					// before serving (observed on the live cluster), hence
					// the pinned pools above.  4Gi was still not enough:
					// before the WORKER_COUNT_UNSUPPORTED_MINIMUM floor fix
					// above the container OOMKilled roughly every 10
					// minutes at ~4.1Gi anonymous memory, and even with the
					// pools genuinely pinned it idles at ~3.6Gi — Quay's
					// ~20 single-purpose workers each carry the full Python
					// codebase — leaving under 500Mi of headroom for push
					// load (all observed on the live cluster during the
					// HOL-1178 verification).  6Gi gives real headroom; a
					// limit reserves nothing, so the only cost is on a box
					// that cannot spare it.
					resources: {
						requests: {
							cpu:    "250m"
							memory: "512Mi"
						}
						limits: memory: "6Gi"
					}
					// No readOnlyRootFilesystem: the Quay entrypoint writes
					// runtime state (supervisord configuration and logs)
					// throughout its filesystem.
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
					}
					volumeMounts: [
						{
							name:      "config"
							mountPath: "/conf/stack"
						},
						{
							// The local-ca root cert, mounted where the Quay
							// entrypoint's certs_install step picks up extra
							// trust anchors, so server-side OIDC TLS to
							// auth.holos.localhost validates.  A subdir mount of
							// /conf/stack — it coexists with the config volume's
							// config.yaml the initContainer renders.
							name:      "local-ca"
							mountPath: "/conf/stack/extra_ca_certs"
							readOnly:  true
						},
						{
							name:      "datastorage"
							mountPath: "/datastorage"
						},
					]
				}]
				volumes: [
					{
						name: "config-template"
						configMap: name: CONFIG_TEMPLATE
					},
					{
						name: "config"
						emptyDir: {}
					},
					{
						name: "local-ca"
						secret: {
							secretName: CA_CERT_SECRET
							items: [{
								key:  CA_CERT_KEY
								path: "local-ca.crt"
							}]
						}
					},
					{
						name: "datastorage"
						persistentVolumeClaim: claimName: PVC_NAME
					},
				]
			}
		}
	}
}

let SERVICE = {
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

// Cross-namespace attachment to the shared Gateway is allowed because its
// listeners set allowedRoutes.namespaces.from: All (istio-gateway
// component).  sectionName binds this route to the https listener only:
// the registry carries credentials, so it must never be served over the
// plaintext http listener — the companion route below redirects port 80 to
// HTTPS instead.  The backend is plain HTTP on 8080 (the Gateway
// terminates TLS — EXTERNAL_TLS_TERMINATION above), so no DestinationRule
// is needed: this is the echo pattern, not the Keycloak TLS-origination
// pattern.
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
	}
	spec: {
		parentRefs: [{
			name:        GATEWAY_NAME
			namespace:   GATEWAY_NAMESPACE
			sectionName: "https"
		}]
		hostnames: [HOSTNAME]
		rules: [{
			matches: [{path: {type: "PathPrefix", value: "/"}}]
			backendRefs: [{
				name: NAME
				port: PORT
			}]
		}]
	}
}

// Companion to HTTPROUTE above: bound to the http listener only, it
// permanently redirects every plaintext request for the Quay hostname to
// HTTPS, so no registry credentials can transit port 80.  A
// RequestRedirect filter terminates the request at the Gateway; no
// backendRefs.
let HTTPROUTE_REDIRECT = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      "\(NAME)-redirect-http"
		namespace: NAMESPACE
	}
	spec: {
		parentRefs: [{
			name:        GATEWAY_NAME
			namespace:   GATEWAY_NAMESPACE
			sectionName: "http"
		}]
		hostnames: [HOSTNAME]
		rules: [{
			filters: [{
				type: "RequestRedirect"
				requestRedirect: {
					scheme:     "https"
					statusCode: 301
				}
			}]
		}]
	}
}

// In-cluster clients must reach the registry by its public hostname:
// Quay pins its OCI token-auth realm to
// https://quay.holos.localhost/v2/auth (PREFERRED_URL_SCHEME +
// SERVER_HOSTNAME above), so a client that connects via the in-cluster
// Service DNS name is still redirected to the public hostname to fetch a
// token — every v2 client needs quay.holos.localhost to resolve and route
// inside the cluster.  Plain DNS cannot provide that: *.localhost names
// resolve to loopback both upstream of CoreDNS (the host resolver
// implements RFC 6761) and inside ztunnel's DNS proxy
// (AMBIENT_DNS_CAPTURE is enabled, and ztunnel's resolver special-cases
// *.localhost before forwarding), so a CoreDNS rewrite never sees queries
// from ambient-enrolled pods.  This ServiceEntry fixes both layers at
// once: it makes quay.holos.localhost a service the mesh knows, so ztunnel
// answers enrolled pods' queries with the auto-allocated VIP and routes
// connections to that VIP to the shared Gateway, which terminates TLS for
// *.holos.localhost and routes by SNI/Host to the HTTPRoute above —
// in-cluster clients traverse the exact host path, credentials and all.
// protocol TLS keeps ztunnel at L4 (the Gateway terminates TLS);
// resolution DNS tracks the Gateway Service by name so the entry survives
// ClusterIP changes — the "<gateway>-istio" Service name is Istio's
// gateway auto-deployment convention, coupled to GATEWAY_NAME above.
// exportTo is deliberately left at its mesh-wide default: every enrolled
// namespace should resolve the registry's public hostname exactly as the
// host does.  Verified live in HOL-1188; the consumption contract is
// documented in holos/docs/argocd-application-source.md.
let SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "quay-holos-localhost"
		namespace: NAMESPACE
	}
	spec: {
		hosts: [HOSTNAME]
		ports: [{
			number:   443
			name:     "tls"
			protocol: "TLS"
		}]
		resolution: "DNS"
		endpoints: [{
			address: "\(GATEWAY_NAME)-istio.\(GATEWAY_NAMESPACE).svc.cluster.local"
		}]
	}
}

// AUTH_SERVICE_ENTRY makes the Keycloak issuer hostname auth.holos.localhost a
// service the mesh resolves so Quay's server-side OIDC calls (discovery, JWKS,
// token exchange against OIDC_SERVER) reach Keycloak in-cluster — the same
// pattern as SERVICE_ENTRY above and the argocd component's identically-named
// entry (components/argocd/controller).  The quay namespace is ambient-enrolled
// (holos/namespaces.cue), and *.localhost names resolve to loopback both
// upstream of CoreDNS (RFC 6761 host resolver) and inside ztunnel's DNS proxy
// (AMBIENT_DNS_CAPTURE special-cases *.localhost before forwarding), so a plain
// DNS override never reaches enrolled pods.  This entry makes the hostname a
// mesh service: ztunnel answers the Quay pod's query with the auto-allocated
// VIP and routes to the shared Gateway, which terminates TLS for
// *.holos.localhost and routes by SNI/Host to the keycloak HTTPRoute, so the
// issuer serves https://auth.holos.localhost/realms/holos/ end-to-end and the
// iss claim matches.  protocol TLS keeps ztunnel at L4 (the Gateway terminates
// TLS); resolution DNS tracks the Gateway Service by name so the entry survives
// ClusterIP changes.  exportTo is left at its mesh-wide default — harmless, as
// only Quay resolves this issuer hostname here.
let AUTH_SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "auth-holos-localhost"
		namespace: NAMESPACE
	}
	spec: {
		hosts: [ISSUER_HOSTNAME]
		ports: [{
			number:   443
			name:     "tls"
			protocol: "TLS"
		}]
		resolution: "DNS"
		endpoints: [{
			address: "\(GATEWAY_NAME)-istio.\(GATEWAY_NAMESPACE).svc.cluster.local"
		}]
	}
}

// CA_CERTIFICATE issues a short-lived leaf certificate in the quay namespace
// from the local-ca ClusterIssuer purely as a vehicle for its ca.crt: every
// cert-manager-issued Secret carries the signing CA in ca.crt, so this puts the
// local-ca root PEM into a Secret (CA_CERT_SECRET) the Quay pod can mount from
// its own namespace.  The leaf cert itself is unused — only ca.crt is consumed
// — but a Certificate is the lightest cert-manager-native way to materialise the
// CA into an arbitrary namespace without trust-manager (not deployed here).  The
// dnsName is a stable placeholder local to this namespace; it is never served.
let CA_CERTIFICATE = {
	apiVersion: "cert-manager.io/v1"
	kind:       "Certificate"
	metadata: {
		name:      CA_CERT_SECRET
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		secretName: CA_CERT_SECRET
		// Only ca.crt is consumed; the leaf is never served, so a single
		// in-namespace placeholder dnsName suffices to satisfy the schema.
		dnsNames: ["\(NAME)-local-ca.\(NAMESPACE).svc.cluster.local"]
		issuerRef: {
			group: "cert-manager.io"
			kind:  "ClusterIssuer"
			name:  "local-ca"
		}
	}
}

userDefinedBuildPlan: {
	metadata: name: "quay"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored resources validate against the vendored
				// Kubernetes and Gateway API schemas at render time.
				resources: #Resources & {
					ConfigMap: (CONFIG_TEMPLATE_CM.metadata.name): CONFIG_TEMPLATE_CM
					ServiceAccount: {
						(BOOTSTRAP_SERVICE_ACCOUNT.metadata.name):       BOOTSTRAP_SERVICE_ACCOUNT
						(ADMIN_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): ADMIN_BOOTSTRAP_SERVICE_ACCOUNT
						(QUAY_SERVICE_ACCOUNT.metadata.name):            QUAY_SERVICE_ACCOUNT
					}
					Role: {
						(BOOTSTRAP_ROLE.metadata.name):       BOOTSTRAP_ROLE
						(ADMIN_BOOTSTRAP_ROLE.metadata.name): ADMIN_BOOTSTRAP_ROLE
					}
					RoleBinding: {
						(BOOTSTRAP_ROLE_BINDING.metadata.name):       BOOTSTRAP_ROLE_BINDING
						(ADMIN_BOOTSTRAP_ROLE_BINDING.metadata.name): ADMIN_BOOTSTRAP_ROLE_BINDING
					}
					Job: {
						(BOOTSTRAP_JOB.metadata.name):       BOOTSTRAP_JOB
						(ADMIN_BOOTSTRAP_JOB.metadata.name): ADMIN_BOOTSTRAP_JOB
					}
					Deployment: {
						(DEPLOYMENT.metadata.name):       DEPLOYMENT
						(REDIS_DEPLOYMENT.metadata.name): REDIS_DEPLOYMENT
					}
					Service: {
						(SERVICE.metadata.name):       SERVICE
						(REDIS_SERVICE.metadata.name): REDIS_SERVICE
					}
					AuthorizationPolicy: (REDIS_AUTHZ.metadata.name): REDIS_AUTHZ
					Certificate: (CA_CERTIFICATE.metadata.name):      CA_CERTIFICATE
					PersistentVolumeClaim: (PVC.metadata.name):       PVC
					ServiceEntry: {
						(SERVICE_ENTRY.metadata.name):      SERVICE_ENTRY
						(AUTH_SERVICE_ENTRY.metadata.name): AUTH_SERVICE_ENTRY
					}
					HTTPRoute: {
						(HTTPROUTE.metadata.name):          HTTPROUTE
						(HTTPROUTE_REDIRECT.metadata.name): HTTPROUTE_REDIRECT
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

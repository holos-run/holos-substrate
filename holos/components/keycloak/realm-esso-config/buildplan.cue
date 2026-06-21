// realm-esso-config reconciles the Keycloak esso realm on every scripts/apply
// with an idempotent keycloak-config-cli Job, mirroring the sibling realm-config
// component (which owns the holos realm).  It is a leaf of the keycloak
// component group and inherits KeycloakVersion and KeycloakNamespace from the
// shared ancestor ../keycloak.cue.
//
// The esso realm (HOL-1366/HOL-1368) is a SECOND realm on the same Keycloak
// instance modeling an upstream Enterprise SSO identity provider (authentication
// only).  This phase (phase 2) builds the entire esso-realm side of the
// brokering: the operator's KeycloakRealmImport (../instance/buildplan.cue
// ESSO_REALM_IMPORT) bootstraps the realm shell (realm esso, enabled: true); this
// component layers the confidential OIDC client the holos realm's IdP broker
// authenticates as (clientId https://auth.holos.internal/realms/holos) and the
// single pre-provisioned user (alice) onto it, and keeps them converged.  The
// holos realm does NOT broker to esso yet — that IdP definition is phase 3
// (HOL-1369), which reads the SAME esso-idp-oidc client-secret Secret this
// component generates (see ESSO_IDP_OIDC_SECRET below).
//
// Scope discipline: REALM_CONFIG carries only realm: "esso" — no holos realm
// fields — so it never contends with the holos realm-config Job or the holos
// KeycloakRealmImport.  keycloak-config-cli's default import.managed.* behavior
// is "no-delete" for objects it does not declare, so this never purges realm
// state it does not own.
//
// Apply ordering (the actual scripts/apply wiring is phase 4, HOL-1370): the
// esso config Job depends on the keycloak-initial-admin Secret (operator-created)
// and the generated esso-idp-oidc / esso-user-alice Secrets (the ESSO_BOOTSTRAP
// Job below).  Like the holos CONFIG_JOB, the secretKeyRefs hold the config Job's
// pod pending until the bootstrap Job has created those Secrets, so the apply
// step only needs to apply this component after the Keycloak server is Ready and
// gate on the config Job completing — no explicit ordering between the two Jobs.
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
// own metadata.name is NAME plus a content hash (JOB_NAME) so an esso realm
// import change always produces a fresh Job — see CONFIG_HASH.  It is distinct
// from the holos realm-config component's "keycloak-config" NAME so the two
// components' Jobs, ConfigMaps, and the scripts/apply gate never collide.
let NAME = "keycloak-esso-config"

// KeycloakConfigCLIImage pins the keycloak-config-cli image — the SAME pin the
// holos realm-config component uses (components/keycloak/realm-config).  adorsys
// publishes it as docker.io/adorsys/keycloak-config-cli:<cli-version>-<keycloak-version>;
// 6.5.1-26.5.5 is the newest tag built for the Keycloak 26.x line this platform
// runs (KeycloakVersion 26.6.3 in ../keycloak.cue) and publishes a multi-arch
// manifest list including linux/arm64 (required by the k3d-on-Apple-Silicon
// target, ADR-7).  Keep this in sync with the holos realm-config pin.
let KeycloakConfigCLIImage = "docker.io/adorsys/keycloak-config-cli:6.5.1-26.5.5"

// KUBECTL_IMAGE pins the image the bootstrap Job runs kubectl from — the SAME pin
// the holos realm-config and quay components use.  docker.io/alpine/kubectl:1.33.3
// is a multi-arch manifest list including linux/arm64 and is alpine-based,
// providing the /bin/sh the Job script needs.  The Job performs only core/v1
// Secret get/create, which are version-stable.
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

// The operator names the Keycloak Service "<cr-name>-service"; with CR name
// "keycloak" that is "keycloak-service".  Keycloak runs HTTP-only behind the
// shared Gateway (HOL-1362).  The Job runs in this namespace so the short Service
// name resolves and reaches the admin API over plaintext HTTP (ztunnel HBONE mTLS
// secures the in-namespace hop).
let KEYCLOAK_URL = "http://keycloak-service:8080"

// The operator generates the initial admin credentials in this Secret (keys
// username/password) on first reconcile.  No plaintext credentials are committed;
// the Job reads them at runtime via secretKeyRef.
let ADMIN_SECRET = "keycloak-initial-admin"

// REALM is the esso realm — distinct from the holos realm-config component's
// "holos".  All REALM_CONFIG below stays scoped to this realm only.
let REALM = "esso"

// ESSO_IDP_CLIENT_ID is the confidential OIDC client the holos realm's IdP broker
// (phase 3, HOL-1369) authenticates as.  The clientId is the holos realm's issuer
// URL by Keycloak's broker convention; the matching broker endpoint redirect URI
// is ESSO_IDP_REDIRECT_URI below.  Both are reserved identifiers recorded in
// ADR-20 (HOL-1367).
let ESSO_IDP_CLIENT_ID = "https://auth.holos.internal/realms/holos"
let ESSO_IDP_REDIRECT_URI = "https://auth.holos.internal/realms/holos/broker/esso/endpoint"

// ESSO_IDP_OIDC_SECRET is the Secret carrying the shared OIDC client secret for
// the esso broker client.  The ESSO_BOOTSTRAP Job below generates it once into
// the keycloak namespace (key client_secret) and never rotates it.  THIS IS THE
// SINGLE SOURCE OF THE CLIENT SECRET: phase 3's holos realm-config Job
// (HOL-1369) MUST read the SAME Secret name + key for the esso IdP's
// clientSecret, so both sides of the broker authenticate with one value.  Do not
// rename without updating phase 3 to match.  The config Job reads it via the
// ESSO_IDP_CLIENT_SECRET_ENV env var and keycloak-config-cli substitutes
// $(env:ESSO_IDP_CLIENT_SECRET) into the client at import time.
let ESSO_IDP_OIDC_SECRET = "esso-idp-oidc"
let ESSO_IDP_OIDC_SECRET_KEY = "client_secret"
let ESSO_IDP_CLIENT_SECRET_ENV = "ESSO_IDP_CLIENT_SECRET"

// ESSO_ALICE_* model the single pre-provisioned esso user.  alice's verified
// email (alice@example.com) is what the holos realm's trustEmail auto-link (the
// "first broker login" flow in components/keycloak/realm-config, HOL-1348) keys
// on when she first logs in through the esso broker (phase 3): a holos-realm user
// pre-provisioned with the same verified email is linked silently.  The username
// is the numeric subject (87654321) an upstream Enterprise SSO commonly asserts.
// alice's password is generated once at runtime (the ESSO_BOOTSTRAP Job) and
// substituted from $(env:ALICE_PASSWORD) at import time, so it is never committed
// (the Runtime Secret Handling guardrail).
let ESSO_ALICE_USERNAME = "87654321"
let ESSO_ALICE_EMAIL = "alice@example.com"
let ESSO_ALICE_PASSWORD_SECRET = "esso-user-alice"
let ESSO_ALICE_PASSWORD_SECRET_KEY = "password"
let ESSO_ALICE_PASSWORD_ENV = "ALICE_PASSWORD"

// REALM_CONFIG is the keycloak-config-cli import document, marshalled to JSON in
// the ConfigMap below (the "No raw inline YAML/JSON in CUE" guardrail — authored
// as a CUE struct, never a triple-quoted blob).  Scoped to realm: "esso" only.
let REALM_CONFIG = {
	realm: REALM

	// The confidential OIDC client the holos realm's IdP broker authenticates
	// as.  publicClient: false (it holds a secret) and standardFlowEnabled: true
	// (the browser Authorization Code flow the broker uses).  The single
	// redirectUri is the holos realm's broker endpoint for the "esso" alias.
	// keycloak-config-cli substitutes the generated secret at run time from the
	// ESSO_IDP_CLIENT_SECRET env var (CONFIG_JOB below), so no secret is
	// committed; the bootstrap Job generates it once and never rotates it, so the
	// value stays stable across reconciles.
	clients: [{
		clientId:            ESSO_IDP_CLIENT_ID
		name:                "Holos realm (OIDC broker relying party)"
		enabled:             true
		protocol:            "openid-connect"
		publicClient:        false // confidential: the holos IdP sends a client secret
		standardFlowEnabled: true
		// Confidential-only/extra flows are off: the broker uses only the browser
		// Authorization Code flow authenticated by the client secret.
		serviceAccountsEnabled:    false
		directAccessGrantsEnabled: false
		secret:                    "$(env:\(ESSO_IDP_CLIENT_SECRET_ENV))"
		redirectUris: [ESSO_IDP_REDIRECT_URI]
		webOrigins: []
	}]

	// Exactly one user: alice, the pre-provisioned esso identity.  emailVerified
	// true so the holos realm's trustEmail auto-link matches her by a verified
	// email (phase 3).  enabled true so she can authenticate.  Her password comes
	// from the generated $(env:ALICE_PASSWORD), so none is committed.
	users: [{
		username:      ESSO_ALICE_USERNAME
		email:         ESSO_ALICE_EMAIL
		firstName:     "Alice"
		lastName:      "Doe"
		enabled:       true
		emailVerified: true
		credentials: [{type: "password", value: "$(env:\(ESSO_ALICE_PASSWORD_ENV))", temporary: false}]
	}]
}

// CONFIG_MAP holds the import document as esso.json for the Job to read from
// /config.  marshalled with encoding/json so the committed deploy file is a
// stable, reviewable JSON string.
let CONFIG_MAP = {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: {
		name:      "keycloak-esso-realm-config"
		namespace: NAMESPACE
	}
	data: "esso.json": json.Marshal(REALM_CONFIG)
}

// CONFIG_HASH is a short content hash of everything that determines what the Job
// converges: the esso realm import document and the image tag.  It is suffixed
// onto the Job's metadata.name (JOB_NAME) so the rendered manifest is
// self-describing and the deploy file name changes visibly in review when the
// import document or image changes.  The actual "reconcile on every apply"
// guarantee comes from scripts/apply's pre-apply delete of the keycloak-config
// Jobs by label (phase 4), not the hash — keycloak-config-cli converges
// idempotently.  8 hex chars (32 bits) is ample for the naming role.
let CONFIG_HASH = strings.SliceRunes(hex.Encode(sha256.Sum256(
CONFIG_MAP.data["esso.json"]+"\n"+KeycloakConfigCLIImage)), 0, 8)

// JOB_NAME embeds the content hash so an import-document or image change renders
// a distinct Job.  The scripts/apply gate (phase 4) resolves the current Job by
// reading this rendered name from the committed manifest.
let JOB_NAME = "\(NAME)-\(CONFIG_HASH)"

// CONFIG_JOB runs keycloak-config-cli against the live Keycloak admin API to
// converge the esso realm.  It mirrors the holos realm-config CONFIG_JOB: same
// image, same KEYCLOAK_URL/KEYCLOAK_USER/KEYCLOAK_PASSWORD (from the
// keycloak-initial-admin Secret), IMPORT_VARSUBSTITUTION_ENABLED so the
// $(env:...) tokens resolve, and the same hardened securityContext (non-root
// 65534, read-only root filesystem with a writable /tmp emptyDir, dropped
// capabilities, no service-account token).  IMPORT_FILES_LOCATIONS points at this
// component's /config/esso.json, and the env vars read the generated
// esso-idp-oidc / esso-user-alice Secrets.
let CONFIG_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata: {
		name:      JOB_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		backoffLimit:            3
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
					runAsUser:    65534
					runAsGroup:   65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "keycloak-esso-config"
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
						// The shared esso broker-client secret.  keycloak-config-cli
						// substitutes $(env:ESSO_IDP_CLIENT_SECRET) into the
						// confidential client at run time.  The ESSO_BOOTSTRAP Job
						// generates the Secret once and never rotates it; the
						// secretKeyRef holds this pod pending until that Secret exists
						// (level-triggered convergence), so no explicit Job ordering is
						// needed.
						{
							name: ESSO_IDP_CLIENT_SECRET_ENV
							valueFrom: secretKeyRef: {
								name: ESSO_IDP_OIDC_SECRET
								key:  ESSO_IDP_OIDC_SECRET_KEY
							}
						},
						// alice's generated password.  Substituted into her
						// credential at import time; the secretKeyRef holds the pod
						// pending until the bootstrap Job has created the Secret.
						{
							name: ESSO_ALICE_PASSWORD_ENV
							valueFrom: secretKeyRef: {
								name: ESSO_ALICE_PASSWORD_SECRET
								key:  ESSO_ALICE_PASSWORD_SECRET_KEY
							}
						},
						// Tolerate the apply gate polling before the server is fully
						// serving; keycloak-config-cli retries the admin API until it
						// answers or the timeout elapses.
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
							value: "/config/esso.json"
						},
						// Enable $(env:...) substitution so the client secret and
						// alice's password placeholders resolve at import time.  The
						// CLI defaults this to false, which would import the literal
						// placeholder strings.
						{
							name:  "IMPORT_VARSUBSTITUTION_ENABLED"
							value: "true"
						},
						// keycloak-config-cli is a Spring Boot app; point its writable
						// temp directory at the /tmp emptyDir since the root filesystem
						// is read-only.
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

// ESSO_BOOTSTRAP is the generate-once bootstrap for BOTH the shared esso broker
// client secret AND alice's password (the AC permits combining the two), modeled
// on the holos realm-config QUAY_OIDC_BOOTSTRAP / PASSWORD_BOOTSTRAP Jobs.  It
// runs in the keycloak namespace and writes two Secrets there:
//
//   - ESSO_IDP_OIDC_SECRET (key client_secret): read by the CONFIG_JOB here AND
//     by phase 3's holos realm-config Job (HOL-1369) — the single source of the
//     broker client secret.
//   - ESSO_ALICE_PASSWORD_SECRET (key password): alice's generated password, read
//     by the CONFIG_JOB.  Retrievable with:
//       kubectl -n keycloak get secret esso-user-alice \
//         -o jsonpath='{.data.password}' | base64 -d
//
// Generate-once discipline: the script creates each Secret only if absent and
// never overwrites, so both values are stable across re-applies and never
// rotated.  Generated at runtime, never committed (the Runtime Secret Handling
// guardrail).  The values are alphanumeric (base64 stripped to A-Za-z0-9) so they
// are safe in the realm JSON; the length check guards against an empty-secret
// pipeline failure under set -eu; each Secret is piped on stdin so the material
// never appears in the container's argv (/proc-visible).
let ESSO_BOOTSTRAP = "esso-secret-bootstrap"

let ESSO_BOOTSTRAP_METADATA = {
	name:      ESSO_BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": ESSO_BOOTSTRAP
}

// The two Secrets the Job manages, used to scope the Role's get grant to exactly
// these names.
let ESSO_BOOTSTRAP_SECRETS = [ESSO_IDP_OIDC_SECRET, ESSO_ALICE_PASSWORD_SECRET]

let ESSO_BOOTSTRAP_SCRIPT = """
	set -eu
	random_secret() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	create_if_absent() {
	  # $1 = Secret name, $2 = data key.  Generate a value and create the Secret
	  # only if it does not already exist; never overwrite an existing one.
	  name="$1"
	  key="$2"
	  if kubectl -n \(NAMESPACE) get secret "$name" >/dev/null 2>&1; then
	    echo "Secret $name already exists in \(NAMESPACE); leaving it untouched."
	    return 0
	  fi
	  value="$(random_secret)"
	  [ "${#value}" -eq 48 ]
	  kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: $name
	  namespace: \(NAMESPACE)
	stringData:
	  $key: "${value}"
	EOF
	  echo "Secret $name created in \(NAMESPACE)."
	}
	create_if_absent \(ESSO_IDP_OIDC_SECRET) \(ESSO_IDP_OIDC_SECRET_KEY)
	create_if_absent \(ESSO_ALICE_PASSWORD_SECRET) \(ESSO_ALICE_PASSWORD_SECRET_KEY)
	"""

let ESSO_BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   ESSO_BOOTSTRAP_METADATA
}

// Role granting the Job get on exactly the two Secrets and namespace-wide create
// on secrets (the API server does not evaluate resourceNames for create).  Both
// Secrets live in the keycloak namespace, so a single Role/RoleBinding pair
// suffices (the PASSWORD_BOOTSTRAP precedent).
let ESSO_BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   ESSO_BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: ESSO_BOOTSTRAP_SECRETS
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let ESSO_BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   ESSO_BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     ESSO_BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      ESSO_BOOTSTRAP
		namespace: NAMESPACE
	}]
}

// The bootstrap Job.  Like the holos realm-config bootstrap Jobs it is deleted
// and recreated on every apply by the phase-4 scripts/apply hook (its own
// app.kubernetes.io/name label), so it always re-runs; idempotent (exits 0
// leaving existing Secrets untouched), and the Secrets survive the Job deletion,
// so the generate-once guarantee holds across re-runs.
let ESSO_BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   ESSO_BOOTSTRAP_METADATA
	spec: {
		backoffLimit:            3
		ttlSecondsAfterFinished: 86400
		template: {
			metadata: labels: ESSO_BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: ESSO_BOOTSTRAP
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
					command: ["/bin/sh", "-c", ESSO_BOOTSTRAP_SCRIPT]
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
	metadata: name: NAME
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per resource
		// so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the hand-authored
				// ConfigMap and Job validate against the vendored Kubernetes schemas
				// at render time.
				resources: #Resources & {
					ConfigMap: (CONFIG_MAP.metadata.name): CONFIG_MAP
					Job: {
						(CONFIG_JOB.metadata.name):         CONFIG_JOB
						(ESSO_BOOTSTRAP_JOB.metadata.name): ESSO_BOOTSTRAP_JOB
					}
					ServiceAccount: (ESSO_BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): ESSO_BOOTSTRAP_SERVICE_ACCOUNT
					Role: (ESSO_BOOTSTRAP_ROLE.metadata.name):               ESSO_BOOTSTRAP_ROLE
					RoleBinding: (ESSO_BOOTSTRAP_ROLE_BINDING.metadata.name): ESSO_BOOTSTRAP_ROLE_BINDING
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					// Every resource sets metadata.namespace explicitly (all in the
					// keycloak namespace); no blanket namespace: directive.
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

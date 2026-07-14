package holos

import (
	"strings"
	"encoding/yaml"
)

// argocd renders the Argo CD core install — application controller, repo
// server, API/UI server, and single-instance redis — from the upstream
// argo-cd Helm chart, with a laptop footprint (ADR-7: a single local
// instance): no dex, no notifications controller, no ApplicationSet pods,
// no HA, no autoscaling, no PodDisruptionBudgets.  The CRDs are
// deliberately NOT rendered here — they are isolated in the sibling crds
// component per the component guidelines (crds.install: false below).  The
// version pin and shared names live in ../argocd.cue.
//
// The UI is exposed at https://argocd.holos.internal through the shared
// Gateway (components/istio-gateway): the Gateway terminates TLS with its
// wildcard-holos-internal Certificate (no per-service Certificate
// needed), and the HTTPRoute pair below attaches the argocd-server Service
// to it — the quay pattern.  server.insecure: "true" makes argocd-server
// serve plain HTTP behind the Gateway; the argocd namespace is
// ambient-enrolled (holos/namespaces.cue), so the Gateway→server hop is
// secured by the mesh.
//
// SSO: Argo CD authenticates against the Keycloak holos realm with OIDC
// using the Authorization Code flow + PKCE (S256).  The public argocd
// client — no client secret — is provisioned declaratively by the
// keycloak-config component (components/keycloak/realm-config), which also
// emits the groups claim carrying both Keycloak group names and realm-role
// names.  configs.cm "oidc.config" below points Argo CD at that issuer and
// client; configs.rbac maps the realm roles to Argo CD roles
// (platform-owner → admin, platform-viewer/editor → readonly).  The
// backchannel OIDC path (discovery/JWKS/token) runs in-cluster: the
// SERVICE_ENTRY below makes the issuer hostname auth.holos.internal a
// service the mesh resolves to the shared Istio gateway, so argocd-server
// reaches Keycloak through the same Gateway→Keycloak path browsers use,
// following the quay-holos-internal ServiceEntry pattern.  The Gateway
// terminates external TLS once and forwards plaintext HTTP to the ambient
// Keycloak pod over a ztunnel HBONE mTLS hop (HOL-1362 — no re-encryption
// DestinationRule).  See the SERVICE_ENTRY comment for why the entry is
// retained even though, on the .internal TLD, CoreDNS now resolves the issuer
// hostname authoritatively.  "oidc.tls.insecure.skip.verify" accepts the
// per-machine local-CA cert the Gateway serves (the local-only MVP posture
// documented below).

let NAME = "argocd"

// argocd.holos.internal matches the shared Gateway's *.holos.internal
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "argocd.holos.internal"

// The shared Gateway's namespace (components/istio-gateway).
let GATEWAY_NAMESPACE = "istio-gateways"

// The shared Gateway's name (components/istio-gateway).  Feeds both the
// HTTPRoute parentRefs and the SERVICE_ENTRY endpoint below; nothing ties
// the literal to the istio-gateway component at render time, so a Gateway
// rename surfaces only at runtime — update both components together (the
// quay component's GATEWAY_NAME note).
let GATEWAY_NAME = "default"

// The Keycloak issuer hostname (components/keycloak/instance HOSTNAME and the
// OIDC_CONFIG issuer below).  argocd-server's OIDC backchannel must resolve
// and route this name in-cluster — see SERVICE_ENTRY.
let ISSUER_HOSTNAME = "auth.holos.internal"

// The chart names the API/UI Service argocd-server and serves HTTP on
// service port 80 (server.service.servicePortHttp, the chart default) when
// server.insecure is set — the Gateway terminates TLS, so the backend hop
// is plain HTTP, the echo/quay pattern.
let SERVER_SERVICE = "argocd-server"
let SERVER_PORT = 80

// Cross-namespace attachment to the shared Gateway is allowed because its
// listeners set allowedRoutes.namespaces.from: All (istio-gateway
// component).  sectionName binds this route to the https listener only:
// the Argo CD UI carries session credentials, so it must never be served
// over the plaintext http listener — the companion route below redirects
// port 80 to HTTPS instead.
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: ArgoCDNamespace
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
				name: SERVER_SERVICE
				port: SERVER_PORT
			}]
		}]
	}
}

// Companion to HTTPROUTE above: bound to the http listener only, it
// permanently redirects every plaintext request for the Argo CD hostname
// to HTTPS, so no credentials can transit port 80.  A RequestRedirect
// filter terminates the request at the Gateway; no backendRefs.
let HTTPROUTE_REDIRECT = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      "\(NAME)-redirect-http"
		namespace: ArgoCDNamespace
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

// OIDC_CONFIG is the argocd-cm "oidc.config" block, YAML-marshalled below
// (the reference platform's encoding/yaml.Marshal pattern).  A public PKCE
// client: Argo CD's UI and CLI cannot hold a secret, so they use the
// Authorization Code flow with PKCE (S256) and carry NO clientSecret.  The
// issuer is Keycloak's public hostname (its hostname.hostname is
// https://auth.holos.internal, so the iss claim matches), the holos realm
// discovery endpoint.  clientID "argocd" is the public client the
// keycloak-config component provisions.
//
// requestedScopes deliberately does NOT include "groups": the
// keycloak-config component (components/keycloak/realm-config) attaches the
// group-membership and realm-role protocol mappers directly to the argocd
// client's protocolMappers, not to a Keycloak client scope named "groups",
// so the groups claim is emitted unconditionally on every token regardless
// of the requested scopes.  Requesting a "groups" scope that is not a
// registered client scope assigned to the client would make Keycloak reject
// the authorization request with invalid_scope (Keycloak validates requested
// scopes against the client's assigned default/optional client scopes), so
// only the built-in scopes openid/profile/email — backed by Keycloak's
// default client scopes — are requested.  requestedIDTokenClaims still marks
// the groups claim essential so the ID token is guaranteed to carry the
// group/realm-role membership Argo CD's RBAC keys on.
let OIDC_CONFIG = {
	name:                     "Keycloak"
	issuer:                   "https://auth.holos.internal/realms/holos"
	clientID:                 "argocd"
	enablePKCEAuthentication: true
	requestedScopes: ["openid", "profile", "email"]
	requestedIDTokenClaims: groups: essential: true
}

// APP_HEALTH_LUA is the resource.customizations.health.argoproj.io_Application
// health check the platform App-of-Apps (components/app-of-apps, HOL-1376)
// depends on.  Argo CD REMOVED built-in health assessment of the Application
// kind in 1.8 and documents that an app-of-apps using sync waves MUST restore it
// (https://argo-cd.readthedocs.io/en/stable/operator-manual/health/#argocd-app):
// without an Application health customization Argo CD treats every child
// Application as Healthy the instant it is created, so the root App-of-Apps
// applies ALL sync waves at once and the ascending
// argocd.argoproj.io/sync-wave annotations on the children become cosmetic — the
// crds-before-controllers / operator-before-instance ordering the children
// promise (cnpg-crds→cnpg→cnpg-clusters, keycloak-operator→keycloak,
// kargo-crds→kargo, …) would race.  This Lua makes a child Application report
// Healthy only once its own status reports Healthy (and Progressing/Degraded/
// Missing/Suspended otherwise), so the root waits for an earlier wave to settle
// before fanning out the next — exactly the scripts/apply dependency ordering
// AC #4 requires.  This is the canonical app-of-apps Application health script
// (the chart ships no health.argoproj.io_Application default — only the
// ignoreResourceUpdates.argoproj.io_Application entry — so it must be added
// here).  Authored as a Lua string, the sanctioned non-YAML/JSON exception to
// the marshal-embedded-config guardrail (it is a script, like a shell heredoc,
// not a YAML/JSON document).
let APP_HEALTH_LUA = """
	hs = {}
	hs.status = "Progressing"
	hs.message = "Waiting for Application status"
	if obj.status ~= nil then
	  if obj.status.health ~= nil and obj.status.health.status ~= nil then
	    hs.status = obj.status.health.status
	    if obj.status.health.message ~= nil then
	      hs.message = obj.status.health.message
	    end
	  end
	end
	return hs
	"""

// KEYCLOAK_HEALTH_LUA maps the shared keycloak.holos.run status contract to
// Argo CD health.  The wildcard resource customization below applies this one
// script to Instance, Group, GroupMembership, User, and Client resources.  A
// Ready condition is authoritative only after the controller has observed the
// current generation; resources without current status remain Progressing.
// Authored as a Lua string, the sanctioned non-YAML/JSON exception to the
// marshal-embedded-config guardrail.
let KEYCLOAK_HEALTH_LUA = """
	hs = {}
	hs.status = "Progressing"
	hs.message = "Waiting for the Ready condition"

	if obj.status == nil or obj.status.conditions == nil then
	  return hs
	end

	if obj.status.observedGeneration ~= nil and
	   obj.metadata ~= nil and
	   obj.metadata.generation ~= nil and
	   obj.status.observedGeneration ~= obj.metadata.generation then
	  hs.message = "Waiting for the controller to observe the latest generation"
	  return hs
	end

	for _, condition in ipairs(obj.status.conditions) do
	  if condition.type == "Ready" then
	    if condition.message ~= nil and condition.message ~= "" then
	      hs.message = condition.message
	    elseif condition.reason ~= nil and condition.reason ~= "" then
	      hs.message = condition.reason
	    end

	    if condition.status == "True" then
	      hs.status = "Healthy"
	    elseif condition.status == "False" then
	      hs.status = "Degraded"
	    end
	    return hs
	  end
	end

	return hs
	"""

// Wildcard GVKs are supported only in Argo CD's aggregate
// resource.customizations value.  The split
// resource.customizations.health.<group>_<kind> ConfigMap keys cannot contain
// the '*' needed to cover every keycloak.holos.run kind.  Marshal the aggregate
// document so its YAML structure is type-checked and indentation-safe.
let RESOURCE_CUSTOMIZATIONS = {
	"keycloak.holos.run/*": "health.lua": KEYCLOAK_HEALTH_LUA
}

// POLICIES maps Keycloak group/realm-role membership to Argo CD roles, the
// reference platform's _POLICIES shape adapted to this platform's three
// managed roles.  Argo CD matches the g, <subject>, <role> rules against the
// groups claim (configs.rbac scopes: "[groups]" below), which the
// keycloak-config component populates with both group names and realm-role
// names.
//   - platform-owner → role:admin (AC #5): full Argo CD admin.
//   - platform-viewer → role:readonly (AC #6).
//   - platform-editor → role:readonly: Argo CD ships no native "editor"
//     role, so readonly is the safe default for this managed role until a
//     custom role is defined; documented here rather than silently dropped.
//   - authenticated → role:readonly: every realm user is bound to the
//     authenticated default group (keycloak-config), so this grants baseline
//     read access to any successfully authenticated user.
let POLICIES = [
	"g, platform-owner, role:admin",
	"g, platform-viewer, role:readonly",
	"g, platform-editor, role:readonly",
	"g, authenticated, role:readonly",
]

// SERVICE_ENTRY makes the Keycloak issuer hostname auth.holos.internal a
// service the mesh resolves, so argocd-server's server-side OIDC calls
// (discovery/JWKS/token) reach Keycloak in-cluster.  This is the
// quay-holos-internal ServiceEntry pattern (components/quay/buildplan.cue),
// applied here for the same reason: argocd-server's server-side OIDC calls
// must resolve and route the issuer hostname in-cluster, then traverse the
// shared Gateway's TLS path so the iss claim matches.  On the .internal TLD
// CoreDNS (components/coredns) now answers *.holos.internal authoritatively
// for in-cluster clients — unlike the retired .localhost names, .internal is
// an ordinary DNS name with no RFC 6761 loopback short-circuit, so neither
// the host resolver nor ztunnel's DNS proxy special-cases it.  The
// ServiceEntry is retained in this phase (HOL-1364, conservative scope)
// alongside CoreDNS: it makes the hostname a service the mesh knows, so
// ztunnel answers enrolled pods' queries with the auto-allocated VIP and
// routes connections to that VIP to the shared Gateway, which terminates TLS
// for *.holos.internal and routes by
// SNI/Host to the keycloak HTTPRoute — argocd-server traverses the exact
// host path browsers use, and the Gateway forwards plaintext HTTP to the
// ambient Keycloak pod over a ztunnel HBONE mTLS hop (HOL-1362 — no
// re-encryption DestinationRule), so the issuer serves
// https://auth.holos.internal/realms/holos end-to-end and the iss claim
// matches OIDC_CONFIG.issuer.  protocol TLS keeps ztunnel at L4 (the Gateway
// terminates external TLS once); resolution DNS tracks the Gateway
// Service by name so the entry survives ClusterIP changes — the
// "<gateway>-istio" Service name is Istio's gateway auto-deployment
// convention, coupled to GATEWAY_NAME above.  Lives in the argocd namespace
// (the consumer); exportTo is left at its mesh-wide default, harmless since
// only argocd-server resolves this issuer hostname.
let SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "auth-holos-internal"
		namespace: ArgoCDNamespace
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

userDefinedBuildPlan: {
	metadata: name: NAME
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Helm"
				output: "helm-output.yaml"
				helm: {
					namespace: ArgoCDNamespace
					// The chart's only hooks are the redis-secret-init Job
					// and its ServiceAccount/Role/RoleBinding (pre-install,
					// pre-upgrade) — verified against the vendored chart;
					// re-verify when bumping ArgoCDChartVersion.  Rendering
					// them is deliberate: the Job runs `argocd admin
					// redis-initial-password`, which generates the
					// argocd-redis auth Secret only if it does not exist —
					// create-if-absent, like the quay secret-keys bootstrap
					// Job — so it converges under declarative re-apply and
					// the redis Deployment's required secretKeyRef resolves
					// on first bring-up.  The hook annotations are inert to
					// kubectl apply.
					enableHooks: true
					chart: {
						name:       "argo-cd"
						version:    ArgoCDChartVersion
						release:    "argocd"
						repository: ArgoCDRepository
					}
					values: {
						// Helm derives version-gated template output from the
						// helm binary's compiled-in default Kubernetes version
						// unless overridden; pin it to the local cluster's
						// k3s version — v1.31.5, the k3d v5.8.3 default
						// image, per the CertManagerVersion pin comment in
						// components/cert-manager/cert-manager.cue — so
						// rendering is deterministic across helm versions on
						// contributor machines and CI.  Keep in sync with
						// that comment when the cluster's k3s version moves.
						kubeVersionOverride: "1.31.5"
						// CRDs are isolated in the argocd-crds component.
						crds: install: false
						// global.domain flows into the argocd-cm url and
						// redirect configuration; it must match the HTTPRoute
						// hostname above.
						global: domain: HOSTNAME
						// SSO is OIDC against the Keycloak holos realm
						// (configs.cm "oidc.config" below) using PKCE, not dex:
						// dex is a bundled OIDC broker for upstream IdPs that
						// cannot do OIDC directly, but Keycloak is a first-class
						// OIDC provider, so Argo CD talks to it directly and dex
						// renders no pods.
						dex: enabled: false
						// The notifications controller is out of scope per
						// HOL-1185.
						notifications: enabled: false
						// The ApplicationSet controller is out of scope per
						// HOL-1185.  Chart 9.x has no applicationSet.enabled
						// toggle (the controller is part of the core
						// install), so scale it to zero pods; its Service
						// and RBAC render but select nothing.
						applicationSet: {
							replicas: 0
							pdb: enabled: false
						}
						// Laptop footprint: single-instance redis, one
						// replica of everything, no autoscaling, no
						// PodDisruptionBudgets (all chart defaults today,
						// pinned explicitly so a chart-default change can
						// never silently grow the footprint).
						"redis-ha": enabled: false
						redis: pdb: enabled: false
						controller: {
							replicas: 1
							pdb: enabled: false
						}
						server: {
							replicas: 1
							autoscaling: enabled: false
							pdb: enabled:         false
						}
						repoServer: {
							replicas: 1
							autoscaling: enabled: false
							pdb: enabled:         false
						}
						configs: {
							// argocd-cmd-params-cm.  server.insecure: the
							// shared Gateway terminates TLS (the HTTPRoute
							// pair above), so argocd-server serves plain HTTP
							// behind it.
							params: {
								"server.insecure": "true"

								// reposerver.repo.cache.expiration realizes the
								// "Always" re-pull of the mutable :dev OCI tag
								// the platform App-of-Apps bootstrap reads
								// (components/app-of-apps, HOL-1376).  Argo CD
								// caches an OCI tag's RESOLVED manifest in the
								// repo-server repo cache
								// (ARGOCD_REPO_CACHE_EXPIRATION, default 24h); a
								// re-pushed :dev is not re-pulled until that entry
								// expires.  Argo CD 3.4.2 (chart 9.5.15) exposes
								// NO OCI-tag-specific expiration knob — the only
								// OCI cmd-params keys the vendored chart wires are
								// reposerver.oci.manifest.max.extracted.size and
								// reposerver.oci.layer.media.types (size/format
								// limits, not a TTL) — so this repo-cache TTL is
								// the applicable mechanism.  Shortened to 1m so a
								// moved :dev is re-resolved within a minute; the
								// children's syncPolicy.automated (prune +
								// selfHeal) then reconciles the new manifests to
								// the cluster — the "Always" image-tag update
								// policy (HOL-1376 AC #3).  Tradeoff: a shorter
								// TTL means more frequent re-resolution work on
								// the repo-server; 1m is comfortable for this
								// single-instance laptop cluster.  A digest-pinned
								// targetRevision would make the cache moot, but
								// the bootstrap deliberately tracks mutable :dev.
								"reposerver.repo.cache.expiration": "1m0s"
							}

							// argocd-cm: OIDC SSO against the Keycloak holos
							// realm.  oidc.config is YAML-marshalled (the
							// reference platform pattern) from OIDC_CONFIG
							// above.
							cm: {
								// Keep the chart's built-in admin account
								// enabled as deliberate local break-glass
								// access, NOT a silent chart default: this is
								// the local k3d MVP cluster, and a clean
								// bootstrap has no platform-owner realm user
								// yet, so disabling admin here would lock the
								// operator out of the UI until they create one
								// in Keycloak.  The account is constrained by
								// scope: argocd-server generates its password
								// into argocd-initial-admin-secret (in-cluster,
								// not committed) and the UI is reachable only
								// through the shared Gateway at
								// argocd.holos.internal (→ 127.0.0.1, never off
								// the host).  Keycloak SSO + the groups-claim
								// RBAC below is the authoritative path for every
								// real user; when a production cluster is
								// established (the production deployment area
								// placeholder in holos/docs/placeholders.md),
								// set this to "false" so Keycloak is the only
								// way in.
								"admin.enabled": "true"

								"oidc.config": yaml.Marshal(OIDC_CONFIG)

								// Accept the local-CA/mkcert cert the shared
								// Gateway serves on the argocd-server →
								// Keycloak backchannel hop (OIDC
								// discovery/JWKS/token).  Local-only
								// MVP posture: the mkcert root CA is
								// per-machine and cannot be embedded at
								// render time deterministically, so a
								// render-time rootCA trust anchor is not
								// available — the backchannel accepts the
								// local-CA cert the shared Gateway serves on
								// the auth.holos.internal hop.  Future
								// production work: replace with oidc.config
								// rootCA trust (pin the cluster CA bundle)
								// once a non-mkcert issuer is in play — see
								// the production deployment area placeholder
								// in holos/docs/placeholders.md.
								"oidc.tls.insecure.skip.verify": "true"

								// Restore Application-kind health assessment
								// (removed upstream in Argo CD 1.8) so the
								// platform App-of-Apps' (components/app-of-apps)
								// child sync waves actually gate ordering — see
								// APP_HEALTH_LUA above for the full rationale.
								// Without it the ascending sync-wave annotations
								// on the children are cosmetic and the
								// crds-before-controllers ordering races.
								"resource.customizations.health.argoproj.io_Application": APP_HEALTH_LUA

								// Assess every keycloak.holos.run kind from its Ready
								// condition and observedGeneration.  Wildcards require
								// the aggregate resource.customizations value rather
								// than a split ConfigMap key.
								"resource.customizations": yaml.Marshal(RESOURCE_CUSTOMIZATIONS)
							}

							// argocd-rbac-cm: map Keycloak group/realm-role
							// membership (the groups claim) to Argo CD roles.
							rbac: {
								// Join the g, rules with real newlines into
								// the single policy.csv string.
								"policy.csv": strings.Join(POLICIES, "\n")
								// No implicit access: the explicit g, rules
								// in policy.csv are the only grants.  An
								// unmatched subject gets nothing.
								"policy.default": ""
								// Match g, subjects against the groups claim,
								// which the keycloak-config client populates
								// with both group names and realm-role names.
								"scopes": "[groups]"
							}
						}
					}
				}
			}, {
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored resources validate against the vendored
				// Gateway API schemas at render time.
				resources: #Resources & {
					HTTPRoute: {
						(HTTPROUTE.metadata.name):          HTTPROUTE
						(HTTPROUTE_REDIRECT.metadata.name): HTTPROUTE_REDIRECT
					}
					ServiceEntry: (SERVICE_ENTRY.metadata.name): SERVICE_ENTRY
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						// Forces argocd onto every namespaced resource.  The
						// chart emits nothing destined for another namespace
						// today; re-verify that assumption when bumping
						// ArgoCDChartVersion.
						namespace: ArgoCDNamespace
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

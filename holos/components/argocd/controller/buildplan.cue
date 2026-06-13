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
// The UI is exposed at https://argocd.holos.localhost through the shared
// Gateway (components/istio-gateway): the Gateway terminates TLS with its
// wildcard-holos-localhost Certificate (no per-service Certificate
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
// SERVICE_ENTRY below makes the issuer hostname auth.holos.localhost a
// service the mesh resolves to the shared Istio gateway, so argocd-server
// reaches Keycloak through the same Gateway→Keycloak re-encrypt path
// browsers use (the quay-holos-localhost ServiceEntry pattern — a plain
// CoreDNS rewrite cannot work because ztunnel's DNS proxy special-cases
// *.localhost before CoreDNS sees the query, see the SERVICE_ENTRY comment),
// and "oidc.tls.insecure.skip.verify" accepts the per-machine local-CA cert
// on that hop (the local-only MVP posture documented below).

let NAME = "argocd"

// argocd.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "argocd.holos.localhost"

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
let ISSUER_HOSTNAME = "auth.holos.localhost"

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
// https://auth.holos.localhost, so the iss claim matches), the holos realm
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
	issuer:                   "https://auth.holos.localhost/realms/holos"
	clientID:                 "argocd"
	enablePKCEAuthentication: true
	requestedScopes: ["openid", "profile", "email"]
	requestedIDTokenClaims: groups: essential: true
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

// SERVICE_ENTRY makes the Keycloak issuer hostname auth.holos.localhost a
// service the mesh resolves, so argocd-server's server-side OIDC calls
// (discovery/JWKS/token) reach Keycloak in-cluster.  This is the
// quay-holos-localhost ServiceEntry pattern (components/quay/buildplan.cue),
// applied here for the same reason: the argocd namespace is ambient-enrolled
// (holos/namespaces.cue), and *.localhost names resolve to loopback both
// upstream of CoreDNS (the host resolver implements RFC 6761) and inside
// ztunnel's DNS proxy (AMBIENT_DNS_CAPTURE is enabled and ztunnel's resolver
// special-cases *.localhost before forwarding), so a CoreDNS rewrite never
// sees queries from enrolled pods — a plain DNS override cannot fix this.
// The ServiceEntry fixes both layers at once: it makes the hostname a
// service the mesh knows, so ztunnel answers enrolled pods' queries with the
// auto-allocated VIP and routes connections to that VIP to the shared
// Gateway, which terminates TLS for *.holos.localhost and routes by
// SNI/Host to the keycloak HTTPRoute — argocd-server traverses the exact
// host path browsers use, and the existing Gateway→Keycloak DestinationRule
// re-encrypts to the backend, so the issuer serves
// https://auth.holos.localhost/realms/holos end-to-end and the iss claim
// matches OIDC_CONFIG.issuer.  protocol TLS keeps ztunnel at L4 (the Gateway
// terminates TLS, then re-encrypts); resolution DNS tracks the Gateway
// Service by name so the entry survives ClusterIP changes — the
// "<gateway>-istio" Service name is Istio's gateway auto-deployment
// convention, coupled to GATEWAY_NAME above.  Lives in the argocd namespace
// (the consumer); exportTo is left at its mesh-wide default, harmless since
// only argocd-server resolves this issuer hostname.
let SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "auth-holos-localhost"
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
						redis: pdb: enabled:  false
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
							// The shared Gateway terminates TLS (the
							// HTTPRoute pair above); argocd-server serves
							// plain HTTP behind it.
							params: "server.insecure": "true"

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
								// argocd.holos.localhost (→ 127.0.0.1, never off
								// the host).  Keycloak SSO + the groups-claim
								// RBAC below is the authoritative path for every
								// real user; when a production cluster is
								// established (the production deployment area
								// placeholder in holos/docs/placeholders.md),
								// set this to "false" so Keycloak is the only
								// way in.
								"admin.enabled": "true"

								"oidc.config": yaml.Marshal(OIDC_CONFIG)

								// Accept the local-CA/mkcert backend cert on
								// the argocd-server → Keycloak backchannel
								// hop (OIDC discovery/JWKS/token).  Local-only
								// MVP posture: the mkcert root CA is
								// per-machine and cannot be embedded at
								// render time deterministically, so a
								// render-time rootCA trust anchor is not
								// available — this mirrors the reference
								// platform's insecureSkipVerify on the
								// Keycloak DestinationRule.  Future
								// production work: replace with oidc.config
								// rootCA trust (pin the cluster CA bundle)
								// once a non-mkcert issuer is in play — see
								// the production deployment area placeholder
								// in holos/docs/placeholders.md.
								"oidc.tls.insecure.skip.verify": "true"
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

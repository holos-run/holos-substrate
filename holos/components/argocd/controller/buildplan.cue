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
// SSO: Argo CD authenticates users against the Keycloak `holos` realm over
// OIDC with PKCE (HOL-1211).  It uses the public `argocd` PKCE client
// provisioned by components/keycloak/realm-config (HOL-1210) — no client
// secret — and maps the realm's group/role membership (carried in the
// `groups` token claim) to Argo CD roles via the RBAC ConfigMap below.  The
// in-cluster backchannel to the issuer is solved by the ServiceEntry below.

let NAME = "argocd"

// argocd.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "argocd.holos.localhost"

// The shared Gateway's namespace (components/istio-gateway).
let GATEWAY_NAMESPACE = "istio-gateways"

// The shared Gateway's name; its Istio auto-deployment generates the
// "<name>-istio" Service the ServiceEntry below targets (the quay pattern).
let GATEWAY_NAME = "default"

// AUTH_HOSTNAME is Keycloak's public hostname (components/keycloak/instance
// sets hostname.hostname to https://auth.holos.localhost).  The OIDC issuer
// below is the holos realm under it; the ServiceEntry makes the name
// resolve and route inside the cluster for the backchannel hops.
let AUTH_HOSTNAME = "auth.holos.localhost"

// ISSUER is the Keycloak holos-realm OIDC issuer.  It MUST equal the realm
// discovery document's `issuer` (and the `iss` claim of issued tokens),
// which derives from Keycloak's configured hostname.hostname — so the
// scheme+host here must match AUTH_HOSTNAME above.
let ISSUER = "https://\(AUTH_HOSTNAME)/realms/holos"

// OIDC_CONFIG is Argo CD's oidc.config (argocd-cm).  Public PKCE client:
// references the `argocd` client provisioned by
// components/keycloak/realm-config (HOL-1210) with enablePKCEAuthentication
// and NO clientSecret.  groups is requested as an essential ID-token claim
// so Argo CD always receives the membership it matches RBAC subjects
// against.  Ported from the reference platform's OIDC_CONFIG shape,
// adapted to this realm/client.  AC #1.
let OIDC_CONFIG = {
	name:                     "Keycloak"
	issuer:                   ISSUER
	clientID:                 "argocd"
	enablePKCEAuthentication: true
	requestedScopes: ["openid", "profile", "email", "groups"]
	requestedIDTokenClaims: groups: essential: true
}

// POLICIES maps Keycloak realm groups/roles (carried in the `groups` claim,
// see the realm-config protocol mappers in HOL-1210) to Argo CD roles.
//   - platform-owner  -> admin    (AC #5)
//   - platform-viewer -> readonly (AC #6)
//   - platform-editor -> readonly: Argo CD has no built-in "editor" role;
//     readonly is the safe default for the third managed role until a custom
//     policy is defined.
//   - authenticated   -> readonly: every realm user is in the `authenticated`
//     default group, granting baseline read access.
let POLICIES = [
	"g, platform-owner, role:admin",
	"g, platform-editor, role:readonly",
	"g, platform-viewer, role:readonly",
	"g, authenticated, role:readonly",
]

// SERVICE_ENTRY makes auth.holos.localhost resolve and route inside the
// cluster so argocd-server/repo-server can complete the OIDC backchannel
// (discovery/JWKS/token).  A CoreDNS coredns-custom rewrite does NOT work
// here: the argocd namespace is ambient-enrolled (holos/namespaces.cue), so
// ztunnel's DNS proxy intercepts *.localhost queries before CoreDNS ever
// sees them (AMBIENT_DNS_CAPTURE; ztunnel special-cases *.localhost) — the
// same constraint the quay component documents and solved with a
// ServiceEntry (HOL-1188, live-verified).  This entry makes
// auth.holos.localhost a service the mesh knows: ztunnel answers enrolled
// pods with the auto-allocated VIP and routes to the shared Gateway, which
// terminates TLS for *.holos.localhost and re-encrypts to Keycloak's HTTPS
// backend via the existing keycloak DestinationRule
// (components/keycloak/instance).  The path then serves
// https://auth.holos.localhost/realms/holos end-to-end, so the `iss` claim
// matches ISSUER above.  resolution DNS tracks the Gateway Service by name
// so the entry survives ClusterIP changes; the "<gateway>-istio" Service
// name is Istio's gateway auto-deployment convention, coupled to
// GATEWAY_NAME above.
let SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "auth-holos-localhost"
		namespace: ArgoCDNamespace
	}
	spec: {
		hosts: [AUTH_HOSTNAME]
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
			name:        "default"
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
			name:        "default"
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
						// SSO is OIDC against Keycloak (configs.cm below), not
						// dex.  Argo CD talks to Keycloak directly with PKCE, so
						// the dex bundled proxy stays disabled.
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
						// The shared Gateway terminates TLS (the HTTPRoute
						// pair above); argocd-server serves plain HTTP
						// behind it.
						configs: params: "server.insecure": "true"
						configs: cm: {
							// OIDC/PKCE SSO against the Keycloak holos realm
							// (AC #1).  yaml.Marshal is the reference pattern:
							// oidc.config is a single YAML string value in
							// argocd-cm.
							"oidc.config": yaml.Marshal(OIDC_CONFIG)
							// Accept the local-CA/mkcert cert on the OIDC
							// backchannel hop.  Local-only MVP posture: the
							// mkcert root is per-machine and cannot be embedded
							// at render time deterministically, so trust is
							// skipped here — mirroring the reference platform's
							// insecureSkipVerify on the Keycloak
							// DestinationRule.  TODO(prod): replace with rootCA
							// trust once a deterministic CA bundle is available.
							"oidc.tls.insecure.skip.verify": "true"
						}
						// Group/role -> Argo CD role mapping (AC #5, AC #6).
						configs: rbac: {
							// Real newlines join the policy lines into the
							// single policy.csv string argocd-rbac-cm expects.
							"policy.csv": strings.Join(POLICIES, "\n")
							// No implicit access: the explicit g, rules above
							// are the only grants.
							"policy.default": ""
							// Match g, subjects against the `groups` claim,
							// which the realm-config client (HOL-1210)
							// populates with both group and realm-role names.
							"scopes": "[groups]"
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

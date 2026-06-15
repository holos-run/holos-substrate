package holos

// kargo renders the Kargo control plane — the controller, API/UI server, the
// management controller, the garbage collector, and the internal and external
// webhooks servers — from the upstream Kargo Helm chart, with a laptop
// footprint (ADR-7: a single local instance) and a simplified local-auth
// posture appropriate for the single-user k3d cluster.  The CRDs are
// deliberately NOT rendered here — they are isolated in the sibling kargo-crds
// component per the component guidelines (crds.install: false below).
//
// This adapts the reference platform's kargo component
// (holos-reference/holos/components/kargo) to this repo's conventions.  The
// reference targets AWS / Thomson-Reuters clusters: it wires Kargo to the
// Thomson-Reuters Keycloak OIDC issuer, patches the validating/mutating
// webhook configurations to route to an external public CA, attaches the API
// to a private-service Gateway under *.thomsonreuters.com, and runs two
// replicas of each server.  None of that fits a local single-user cluster, so
// this component instead:
//
//   - Authenticates the API against the Keycloak holos realm with OIDC using
//     the Authorization Code flow + PKCE (S256).  The public kargo client — no
//     client secret — is provisioned declaratively by the keycloak-config
//     component (components/keycloak/realm-config, HOL-1250), which also emits
//     the groups claim carrying both Keycloak group names and realm-role names.
//     api.oidc below points the API at that issuer and client and maps the
//     realm roles to Kargo access levels (platform-owner → system-wide admin,
//     platform-viewer → read-only, platform-editor/authenticated → baseline
//     read).  The backchannel OIDC path (discovery/JWKS/token) runs in-cluster:
//     the SERVICE_ENTRY below makes the issuer hostname auth.holos.localhost a
//     service the mesh resolves to the shared Istio Gateway (the argocd
//     pattern), and the CA_CERTIFICATE below materialises the local-CA root
//     into the kargo namespace so the API trusts the issuer cert without any
//     skip-verify knob (Kargo has none — the quay CA_CERTIFICATE pattern).  The
//     chart's admin account stays disabled (adminAccount.enabled: false), so
//     there is no UI break-glass account and no admin Secret to commit or
//     rotate — Keycloak SSO is the only way in.  The chart still renders an
//     EMPTY Secret/kargo-api (no token signing key is required for OIDC); the
//     kubectl-slice below keeps it because the API Deployment references it.
//   - Exposes the API/UI at https://kargo.holos.localhost through the shared
//     Istio Gateway (components/istio-gateway), the argocd pattern: the Gateway
//     terminates TLS with its wildcard-holos-localhost Certificate (no
//     per-service Certificate needed) and the HTTPRoute pair below attaches the
//     kargo-api Service to it.  api.tls.enabled: false makes the API serve
//     plain HTTP behind the Gateway; the kargo namespace is ambient-enrolled
//     (holos/namespaces.cue), so the Gateway→api hop is secured by the mesh.
//   - Keeps the chart's cert-manager-issued self-signed certificates for the
//     internal webhooks server (webhooksServer.tls.selfSignedCert: true, the
//     chart default): cert-manager IS installed on this cluster
//     (components/cert-manager), so the chart's Issuer/Certificate render and
//     the Kubernetes API server trusts the webhook over TLS without the
//     reference's external-CA webhook patches.
//   - Runs one replica of each server (laptop footprint).
//
// The Project / Warehouse / Stage configuration is out of scope this phase
// (the next phase, HOL-1240); this component only stands up Kargo itself.

// KargoChartVersion pins the upstream Kargo Helm chart
// (oci://ghcr.io/akuity/kargo-charts/kargo).  Chart 1.10.3 installs Kargo app
// version v1.10.3 (the chart's appVersion — chart and app versions track
// together in this chart), matching the reference platform's pin
// (holos-reference/holos/config/kargo/version/*.yaml).  The chart is vendored
// at vendor/1.10.3/kargo.  This MUST match KargoChartVersion in
// components/kargo-crds/buildplan.cue so the CRDs applied ahead of these
// workloads are exactly the versions they expect; the two sibling components
// have no shared CUE ancestor (the issue's flat layout), so a bump touches
// both files plus both vendored charts and both deploy trees.  Before bumping,
// re-check the chart's appVersion and the multi-arch (linux/arm64) image
// availability — the cluster is k3d on OrbStack/Apple silicon.
let KargoChartVersion = "1.10.3"

// KargoChartName is the full OCI reference for the chart; the holos Helm
// generator treats an oci:// chart name as a direct pull, so no separate
// repository field is set.
let KargoChartName = "oci://ghcr.io/akuity/kargo-charts/kargo"

let NAME = "kargo"

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure: if
// "kargo" is ever removed or renamed in holos/namespaces.cue, rendering fails
// here instead of at apply time with a NotFound namespace error.
let NAMESPACE = "kargo" & #RegisteredNamespace

// ArgoCDNamespace is this platform's Argo CD namespace (components/argocd:
// ArgoCDNamespace = "argocd").  Kargo's controller needs it for the
// argocd-update promotion step that patches Argo CD Application targetRevision
// (ADR-16).  The literal must match the argocd entry in the central namespaces
// registry; unifying with #RegisteredNamespace makes a rename or removal of
// that namespace a render failure here rather than a silent runtime failure of
// the argocd-update step.
let ArgoCDNamespace = "argocd" & #RegisteredNamespace

// kargo.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "kargo.holos.localhost"

// The shared Gateway's namespace and name (components/istio-gateway).  Nothing
// ties these literals to the istio-gateway component at render time, so a
// Gateway rename surfaces only at runtime — update both components together
// (the argocd/quay component note).
let GATEWAY_NAMESPACE = "istio-gateways" & #RegisteredNamespace
let GATEWAY_NAME = "default"

// The Keycloak issuer hostname (components/keycloak/instance HOSTNAME and the
// api.oidc.issuerURL below).  The API's OIDC backchannel must resolve and route
// this name in-cluster — see SERVICE_ENTRY.
let ISSUER_HOSTNAME = "auth.holos.localhost"

// CA_CERT_SECRET carries the local-ca root certificate in its ca.crt key.  The
// Kargo API performs its OIDC discovery/JWKS/token calls to the issuer
// (https://auth.holos.localhost) server-side with TLS verification ON and has
// no per-OIDC "insecure skip verify" knob (unlike Argo CD), so it must trust
// the local CA that signed the shared Gateway's *.holos.localhost certificate.
// A cert-manager Certificate issued by the local-ca ClusterIssuer
// (components/local-ca) writes this Secret into the kargo namespace with the
// signing CA in ca.crt; api.cabundle.secretName below points the chart at it,
// and the chart's parse-cabundle initContainer installs every cert in the
// Secret into the API's system trust store on start.  Mounting a per-namespace
// cert-manager Secret (rather than the Gateway's wildcard-holos-localhost
// Secret, which lives in the istio-gateways namespace) keeps the trust anchor
// local to this pod's namespace — a pod can only mount Secrets from its own
// namespace.
let CA_CERT_SECRET = "kargo-local-ca"

// The chart names the API/UI Service kargo-api.  With api.tls.enabled: false
// it serves plain HTTP on service port 80 (the chart default), so the Gateway
// terminates TLS and the backend hop is plain HTTP — the argocd/echo/quay
// pattern.
let API_SERVICE = "kargo-api"
let API_PORT = 80

// HTTPROUTE attaches the Kargo API/UI to the shared Gateway's https listener
// only: the UI carries session credentials, so it must never be served over
// the plaintext http listener — the companion redirect route below sends port
// 80 to HTTPS instead.  Cross-namespace attachment is allowed because the
// shared listener sets allowedRoutes.namespaces.from: All (istio-gateway
// component), so no ReferenceGrant is needed (unlike the reference, whose
// route lived in the gateway namespace and granted cross-namespace Service
// access).
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
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
				name: API_SERVICE
				port: API_PORT
			}]
		}]
	}
}

// Companion to HTTPROUTE above: bound to the http listener only, it
// permanently redirects every plaintext request for the Kargo hostname to
// HTTPS, so no credentials can transit port 80 (the argocd pattern).  A
// RequestRedirect filter terminates the request at the Gateway; no
// backendRefs.
let HTTPROUTE_REDIRECT = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      "\(NAME)-redirect-http"
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
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

// CA_CERTIFICATE issues a short-lived leaf certificate in the kargo namespace
// from the local-ca ClusterIssuer purely as a vehicle for its ca.crt: every
// cert-manager-issued Secret carries the signing CA in ca.crt, so this puts the
// local-ca root PEM into a Secret (CA_CERT_SECRET) the Kargo API pod can mount
// from its own namespace.  The leaf cert itself is unused — only ca.crt is
// consumed by the chart's parse-cabundle initContainer — but a Certificate is
// the lightest cert-manager-native way to materialise the CA into an arbitrary
// namespace without trust-manager (not deployed here).  The dnsName is a stable
// placeholder local to this namespace; it is never served.  The quay
// CA_CERTIFICATE pattern.
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
		dnsNames: ["\(CA_CERT_SECRET).\(NAMESPACE).svc.cluster.local"]
		issuerRef: {
			group: "cert-manager.io"
			kind:  "ClusterIssuer"
			name:  "local-ca"
		}
	}
}

// SERVICE_ENTRY makes the Keycloak issuer hostname auth.holos.localhost a
// service the mesh resolves, so the Kargo API's server-side OIDC calls
// (discovery/JWKS/token) reach Keycloak in-cluster.  This is the argocd
// SERVICE_ENTRY pattern (components/argocd/controller/buildplan.cue), applied
// here for the same reason: the kargo namespace is ambient-enrolled
// (holos/namespaces.cue), and *.localhost names resolve to loopback both
// upstream of CoreDNS (the host resolver implements RFC 6761) and inside
// ztunnel's DNS proxy (which special-cases *.localhost before forwarding), so a
// CoreDNS rewrite never sees queries from enrolled pods — a plain DNS override
// cannot fix this.  The ServiceEntry fixes both layers at once: it makes the
// hostname a service the mesh knows, so ztunnel answers enrolled pods' queries
// with the auto-allocated VIP and routes connections to that VIP to the shared
// Gateway, which terminates TLS for *.holos.localhost and routes by SNI/Host to
// the keycloak HTTPRoute — the API traverses the exact host path browsers use,
// and the existing Gateway→Keycloak DestinationRule re-encrypts to the backend,
// so the issuer serves https://auth.holos.localhost/realms/holos end-to-end and
// the iss claim matches api.oidc.issuerURL.  protocol TLS keeps ztunnel at L4
// (the Gateway terminates TLS, then re-encrypts); resolution DNS tracks the
// Gateway Service by name so the entry survives ClusterIP changes — the
// "<gateway>-istio" Service name is Istio's gateway auto-deployment convention,
// coupled to GATEWAY_NAME above.  Lives in the kargo namespace (the consumer);
// exportTo is left at its mesh-wide default, harmless since only the Kargo API
// resolves this issuer hostname.
let SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "auth-holos-localhost"
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
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
					namespace: NAMESPACE
					chart: {
						name:    KargoChartName
						version: KargoChartVersion
						release: NAME
					}
					values: {
						// Helm derives version-gated template output from the
						// helm binary's compiled-in default Kubernetes version
						// unless overridden; pin it to the local cluster's k3s
						// version — v1.31.5, the k3d v5.8.3 default image, per
						// the CertManagerVersion pin comment in
						// components/cert-manager/cert-manager.cue — so
						// rendering is deterministic across helm versions on
						// contributor machines and CI.
						kubeVersionOverride: "1.31.5"

						// CRDs are isolated in the kargo-crds component.
						crds: install: false

						global: {
							// The cluster-secrets namespace is a deprecated
							// chart concern (global.clusterSecretsNamespace) tied
							// to cluster-scoped webhook receivers, which this
							// phase does not use; do not have the chart create
							// it.
							createClusterSecretsNamespace: false
							// Components MUST NOT emit Namespace resources — all
							// platform namespaces are registered centrally
							// (holos/namespaces.cue) and rendered by the
							// namespaces component (the component guidelines).
							// The chart would otherwise emit Namespace resources
							// for its system- and shared-resources namespaces, so
							// disable its namespace creation; those two
							// namespaces (kargo-system-resources,
							// kargo-shared-resources) are registered in the
							// central registry instead, matching the names the
							// chart's RoleBindings reference below.
							systemResources: createNamespace: false
							sharedResources: createNamespace: false

							// Non-root posture for every Kargo container.  The
							// chart applies global.securityContext at the
							// CONTAINER level on each Deployment/CronJob (each
							// component's per-container securityContext defaults
							// to this), NOT as a pod-level securityContext, and
							// the upstream akuity/kargo image runs as a non-root
							// user, so requiring runAsNonRoot is safe without
							// pinning a uid that could drift from the image.  The
							// api.cabundle parse-cabundle initContainer (now
							// rendered — see api.cabundle below) hardcodes its own
							// runAsUser: 0 to write into the system trust store;
							// that is admissible precisely because
							// global.securityContext is applied per-container, so
							// there is no pod-level runAsNonRoot to conflict with
							// the root initContainer.  seccompProfile:
							// RuntimeDefault matches the platform's bootstrap-Job
							// hardening (the quay precedent).
							securityContext: {
								runAsNonRoot: true
								seccompProfile: type: "RuntimeDefault"
							}
						}

						api: {
							// Laptop footprint: a single API replica.
							replicas:  1
							logFormat: "JSON"
							// The host the API derives its issuer/callback URLs
							// from; it MUST match the HTTPRoute hostname.  The
							// protocol (https) is inferred from
							// tls.terminatedUpstream below.
							host: HOSTNAME

							// SSO is OIDC against the Keycloak holos realm
							// (HOL-1251) using the Authorization Code flow +
							// PKCE (S256), not dex:
							//   - enabled: turn on OIDC authentication.  Kargo is
							//     a public client and uses PKCE automatically (no
							//     client secret), against the public kargo client
							//     the keycloak-config component provisions
							//     (HOL-1250).
							//   - issuerURL/clientID: the holos realm issuer and
							//     the public kargo client ID.  The issuer is
							//     Keycloak's public hostname so the iss claim
							//     matches; the SERVICE_ENTRY above resolves and
							//     routes the backchannel in-cluster, and the
							//     CA_CERTIFICATE above + api.cabundle below make
							//     the API trust the issuer cert.
							//   - dex disabled: Keycloak is a first-class OIDC
							//     provider, so the API talks to it directly and
							//     the bundled dex broker renders no pods.
							//   - additionalScopes []: override the chart default
							//     [groups].  openid/profile/email are always
							//     requested; the groups claim arrives
							//     unconditionally from the client-side protocol
							//     mappers (HOL-1250), so requesting a "groups"
							//     scope — which is not a registered client scope
							//     — would make Keycloak reject the auth request
							//     with invalid_scope.
							//   - admins/viewers/users: map realm-role membership
							//     (surfaced in the groups claim) to Kargo access
							//     levels.  platform-owner → system-wide admin (AC
							//     #1); platform-viewer → read-only all resources
							//     (parity with argocd); platform-editor and the
							//     authenticated default group → baseline read
							//     (platform-editor has no system-level edit role
							//     until project-scoped roles exist).
							oidc: {
								enabled:   true
								issuerURL: "https://\(ISSUER_HOSTNAME)/realms/holos"
								clientID:  "kargo"
								dex: enabled: false
								additionalScopes: []
								admins: claims: groups: ["platform-owner"]
								viewers: claims: groups: ["platform-viewer"]
								users: claims: groups: ["platform-editor", "authenticated"]
							}

							// The local-CA root the API must trust to verify the
							// Keycloak issuer cert on the OIDC backchannel.  Kargo
							// has no per-OIDC skip-verify knob; the only trust
							// mechanism is api.cabundle, which the chart mounts
							// and whose certs its parse-cabundle initContainer
							// installs into the system trust store.  CA_CERTIFICATE
							// above materialises CA_CERT_SECRET (ca.crt) into this
							// namespace; only ca.crt is needed.
							cabundle: secretName: CA_CERT_SECRET

							// The chart's admin account stays disabled: enabling
							// it makes the chart `fail` unless a bcrypt
							// passwordHash and a token signing key are supplied,
							// which would mean generating and committing a Secret.
							// OIDC does NOT require the kargo-api token signing key
							// (the chart only fails for it when adminAccount is
							// enabled), so leaving the account off keeps Keycloak
							// SSO as the only way in — there is no UI break-glass
							// account — with no Secret to manage.  The chart still
							// renders an EMPTY Secret/kargo-api, which the
							// kubectl-slice below keeps because the API Deployment
							// references it.
							adminAccount: enabled: false

							// GitOps manages Kargo Projects declaratively (the
							// next phase), so the API's Secret-management surface
							// is not needed; disabling it reduces the API's
							// attackable surface (the reference does the same).
							secretManagementEnabled: false

							// Argo Rollouts is not installed on this cluster, so
							// disable the integration explicitly: the chart would
							// otherwise grant the API extra AnalysisTemplate
							// permissions and only fall back to disabled after a
							// startup CRD probe.
							rollouts: integrationEnabled: false

							// The shared Gateway terminates TLS (the HTTPRoute
							// pair above); the API serves plain HTTP behind it.
							// terminatedUpstream still forces the API's derived
							// URLs to https so links and callbacks use the
							// browser-facing scheme.  With tls.enabled: false the
							// chart emits no API cert Secret or Issuer.
							tls: {
								enabled:            false
								terminatedUpstream: true
							}

							// Laptop footprint: modest guaranteed-QoS resources.
							resources: {
								requests: {
									cpu:    "50m"
									memory: "128Mi"
								}
								limits: memory: "256Mi"
							}

							// Single-replica: nothing to disrupt, so the chart's
							// PodDisruptionBudget is noise (the argocd
							// precedent of disabling PDBs on single-replica
							// workloads).
							pdb: enabled: false
						}

						controller: {
							logFormat: "JSON"
							// The Argo CD namespace Kargo's argocd-update
							// promotion step targets (ADR-16).  Keep in sync with
							// components/argocd ArgoCDNamespace.
							argocd: namespace: ArgoCDNamespace
							// Argo Rollouts is not installed; disable the
							// integration so the controller is not granted
							// AnalysisRun permissions it cannot use.
							rollouts: integrationEnabled: false
							resources: {
								requests: {
									cpu:    "50m"
									memory: "128Mi"
								}
								limits: memory: "256Mi"
							}
							pdb: enabled: false
						}

						// The remaining servers run one replica each (laptop
						// footprint) with modest resources.  The internal and
						// external webhooks servers keep the chart's
						// cert-manager-issued self-signed certs
						// (tls.selfSignedCert: true, the chart default):
						// cert-manager is installed on this cluster, so the
						// Issuer/Certificate render and the Kubernetes API server
						// trusts the internal webhook over TLS.
						webhooksServer: {
							replicas:  1
							logFormat: "JSON"
							resources: {
								requests: {
									cpu:    "20m"
									memory: "64Mi"
								}
								limits: memory: "128Mi"
							}
							pdb: enabled: false
						}
						externalWebhooksServer: {
							replicas:  1
							logFormat: "JSON"
							resources: {
								requests: {
									cpu:    "20m"
									memory: "64Mi"
								}
								limits: memory: "128Mi"
							}
							pdb: enabled: false
						}
						managementController: {
							logFormat: "JSON"
							resources: {
								requests: {
									cpu:    "20m"
									memory: "64Mi"
								}
								limits: memory: "128Mi"
							}
						}
						garbageCollector: {
							logFormat: "JSON"
							resources: {
								requests: {
									cpu:    "20m"
									memory: "64Mi"
								}
								limits: memory: "128Mi"
							}
						}
					}
				}
			}, {
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored HTTPRoutes validate against the vendored Gateway
				// API schemas at render time.
				resources: #Resources & {
					HTTPRoute: {
						(HTTPROUTE.metadata.name):          HTTPROUTE
						(HTTPROUTE_REDIRECT.metadata.name): HTTPROUTE_REDIRECT
					}
					Certificate: (CA_CERTIFICATE.metadata.name): CA_CERTIFICATE
					ServiceEntry: (SERVICE_ENTRY.metadata.name): SERVICE_ENTRY
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						resources: inputs
						// Coerce OIDC_ADDITIONAL_SCOPES to an explicit empty
						// string.  The chart template renders
						// `OIDC_ADDITIONAL_SCOPES: {{ join "," .additionalScopes }}`
						// unquoted, so our additionalScopes: [] produces a bare
						// `OIDC_ADDITIONAL_SCOPES:` line that YAML parses as null —
						// an invalid ConfigMap data value (data must be
						// map[string]string), which the API server would reject on
						// apply.  This strategic-merge patch overwrites that null
						// with the quoted empty string the API expects (no extra
						// scopes beyond the always-requested openid/profile/email),
						// preserving the AC-required additionalScopes: [] intent
						// while keeping the manifest applyable.
						patches: [{
							target: {
								kind: "ConfigMap"
								name: "kargo-api"
							}
							patch: """
								- op: replace
								  path: /data/OIDC_ADDITIONAL_SCOPES
								  value: ""
								"""
						}]
					}
				},
				{
					kind: "Command"
					inputs: [transformers[0].output]
					// this output is the artifact holos writes to the deploy
					// directory, one file per resource.  CustomResourceDefinition
					// is excluded — the kargo-crds component owns the CRDs.
					//
					// Secret is deliberately NOT excluded (unlike the reference,
					// which supplied credentials out-of-band via ExternalSecrets
					// and stripped the chart's Secret): with the admin account
					// disabled the chart still renders an EMPTY Secret/kargo-api
					// (stringData: {}, no credentials), and the API Deployment's
					// envFrom references it with a non-optional secretRef
					// (api/secret.yaml + api/deployment.yaml in the vendored
					// chart).  Excluding it would leave the api pod stuck in
					// CreateContainerConfigError.  Because the Secret carries no
					// data, committing it leaks nothing — the no-auth posture is
					// what makes it empty (codex round 1).
					output: artifact
					command: args: [
						"holos",
						"kubectl-slice",
						"--exclude-kind=CustomResourceDefinition",
						"-f", "\(BuildContext.tempDir)/\(inputs[0])",
						"-o", "\(BuildContext.tempDir)/\(artifact)",
					]
				},
			]
		}
	}
}

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
//   - Disables OIDC and the chart's admin account (no auth): the API runs
//     unauthenticated on the local cluster, the MVP posture the nats and
//     webhook-receiver components also take.  This is recorded as a deferral in
//     holos/docs/placeholders.md.  No admin Secret is generated, so there is
//     nothing to commit or rotate, and the kubectl-slice below strips any
//     Secret the chart would emit.
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

							// Non-root pod posture for every Kargo pod.  The
							// chart applies global.securityContext as the pod
							// securityContext on each Deployment/CronJob (each
							// component's per-pod securityContext defaults to
							// this), and the upstream akuity/kargo image runs as
							// a non-root user, so requiring runAsNonRoot is safe
							// without pinning a uid that could drift from the
							// image.  The cabundle initContainer that hardcodes
							// runAsUser: 0 only renders when api.cabundle is set,
							// which it is not here.  seccompProfile:
							// RuntimeDefault matches the platform's bootstrap-Job
							// hardening (the nats/quay precedent).
							securityContext: {
								runAsNonRoot: true
								seccompProfile: type: "RuntimeDefault"
							}
						}

						api: {
							// Laptop footprint: a single API replica.
							replicas: 1
							logFormat: "JSON"
							// The host the API derives its issuer/callback URLs
							// from; it MUST match the HTTPRoute hostname.  The
							// protocol (https) is inferred from
							// tls.terminatedUpstream below.
							host: HOSTNAME

							// No auth on the local single-user cluster (MVP
							// posture, deferred — see
							// holos/docs/placeholders.md):
							//   - OIDC disabled: no external identity provider is
							//     wired in for the local cluster (the reference's
							//     Thomson-Reuters issuer is deliberately not
							//     copied).
							//   - dex disabled: the bundled OIDC broker is not
							//     needed without OIDC.
							//   - adminAccount disabled: enabling it makes the
							//     chart `fail` unless a bcrypt passwordHash and a
							//     token signing key are supplied, which would mean
							//     generating and committing a Secret; leaving it
							//     off keeps the API unauthenticated with no Secret
							//     to manage.
							oidc: {
								enabled: false
								dex: enabled: false
							}
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
							// PodDisruptionBudget is noise (the argocd/nats
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
							replicas: 1
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
							replicas: 1
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
					// directory, one file per resource.  CustomResourceDefinition
					// is excluded — the kargo-crds component owns the CRDs — and
					// Secret is excluded so no generated credential lands in the
					// committed deploy tree (the reference platform's kargo
					// kubectl-slice pattern; here it is belt-and-suspenders since
					// the no-auth values emit no admin Secret).
					output: artifact
					command: args: [
						"holos",
						"kubectl-slice",
						"--exclude-kind=CustomResourceDefinition",
						"--exclude-kind=Secret",
						"-f", "\(BuildContext.tempDir)/\(inputs[0])",
						"-o", "\(BuildContext.tempDir)/\(artifact)",
					]
				},
			]
		}
	}
}

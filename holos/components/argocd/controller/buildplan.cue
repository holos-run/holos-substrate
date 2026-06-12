package holos

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

let NAME = "argocd"

// argocd.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "argocd.holos.localhost"

// The shared Gateway's namespace (components/istio-gateway).
let GATEWAY_NAMESPACE = "istio-gateways"

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
						// No SSO in this phase: the local admin user (chart
						// default) signs in to the UI.
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
						// The shared Gateway terminates TLS (the HTTPRoute
						// pair above); argocd-server serves plain HTTP
						// behind it.
						configs: params: "server.insecure": "true"
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

package holos

// webhook-receiver is the thin HTTP ingress that accepts inbound webhooks at
// the shared Gateway (https://hooks.holos.localhost) and publishes each raw,
// unmodified request body to the NATS WEBHOOKS JetStream WorkQueue stream
// (ADR-9, ADR-13).  It emits a Deployment running the holos-paas image with the
// webhook-receiver subcommand, a Service, and an HTTPRoute attached to the
// shared Gateway (istio-gateway component).  It is modeled on the echo
// component — the closest analog for a Deployment + Service + HTTPRoute on the
// shared Gateway — and shares echo's hardened pod posture.
//
// The webhook-receiver Namespace — including its ambient mesh enrollment label
// — is registered in the central namespaces registry (holos/namespaces.cue)
// and rendered by the namespaces component; this component emits no Namespace.
// Enrollment is load-bearing: the receiver publishes cross-namespace into
// nats, and the nats AuthorizationPolicy (components/nats/buildplan.cue) ALLOWs
// this namespace as a source — a policy that only takes effect when both peers
// are captured by ztunnel.

// IMAGE pins the holos-paas image published to the in-cluster k3d registry by
// HOL-1197 (registry.holos.localhost:5100/holos-paas).  The cluster is created
// with --registry-use k3d-registry.holos.localhost:5100, so images pushed to
// registry.holos.localhost:5100 are pullable in-cluster (docs/local-cluster.md).
// The :dev tag matches the Makefile's IMAGE_TAG default (docker-build /
// docker-push); bump it here in lockstep when publishing a new tag.  The image
// is built for linux/arm64 (the cluster is k3d on OrbStack/Apple silicon).
let IMAGE = "registry.holos.localhost:5100/holos-paas:dev"

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure: if
// "webhook-receiver" is ever removed or renamed in holos/namespaces.cue,
// rendering fails here instead of at apply time with a NotFound namespace
// error.
let NAMESPACE = "webhook-receiver" & #RegisteredNamespace
let NAME = "webhook-receiver"
let PORT = 8080

let METADATA = {
	name:      NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": NAME
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
				// hand-authored resources validate against the vendored
				// Kubernetes and Gateway API schemas at render time.
				resources: #Resources & {
					Deployment: (NAME): {
						apiVersion: "apps/v1"
						kind:       "Deployment"
						metadata:   METADATA
						spec: {
							replicas: 1
							selector: matchLabels: METADATA.labels
							template: {
								metadata: labels: METADATA.labels
								spec: {
									// The receiver talks only to NATS over the
									// network, never to the Kubernetes API; don't
									// mount a ServiceAccount token it has no use
									// for.
									automountServiceAccountToken: false
									securityContext: {
										runAsNonRoot: true
										// The image's distroless:nonroot base
										// declares USER 65532; pin it here too so
										// the pod's non-root guarantee does not
										// depend on the image default.
										runAsUser:  65532
										runAsGroup: 65532
										seccompProfile: type: "RuntimeDefault"
									}
									containers: [{
										name:  NAME
										image: IMAGE
										// The :dev tag is mutable: `make docker-push`
										// overwrites it in the registry without
										// changing this spec, so a re-apply is a
										// no-op for the Deployment.  Always pull so a
										// pod (re)start picks up the freshly pushed
										// image rather than a stale layer cached on
										// the node under IfNotPresent (the k3s
										// default for a non-:latest tag) — without
										// this, scripts/apply's rollout gate can pass
										// against an old image.  Pin to a content
										// digest instead of a mutable tag if this is
										// ever promoted past the local k3d cluster.
										imagePullPolicy: "Always"
										// The image ENTRYPOINT is the holos-paas
										// binary; the service is selected by the
										// subcommand arg.  The NATS URL defaults to
										// the in-cluster client endpoint
										// (nats://nats.nats.svc.cluster.local:4222)
										// in the receiver's flag defaults, so it is
										// not repeated here — keep the arg surface
										// minimal and let the binary own its
										// defaults.
										args: ["webhook-receiver"]
										ports: [{
											name:          "http"
											containerPort: PORT
											protocol:      "TCP"
										}]
										// /readyz reports 200 only once the
										// receiver is connected to NATS, so a pod
										// that cannot reach the broker is held out
										// of the Service (and the rollout gate in
										// scripts/apply) until it can publish.
										// /healthz is unconditional liveness — it
										// stays 200 through a NATS outage so the
										// kubelet does not restart a pod that is
										// correctly returning 503 and waiting to
										// reconnect.
										readinessProbe: httpGet: {
											path: "/readyz"
											port: PORT
										}
										livenessProbe: httpGet: {
											path: "/healthz"
											port: PORT
										}
										// A modest QoS floor for a thin,
										// I/O-bound HTTP→NATS forwarder; it idles
										// far below these (the echo precedent).
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
									}]
								}
							}
						}
					}

					Service: (NAME): {
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

					// Cross-namespace attachment to the shared Gateway is
					// allowed because its listener sets
					// allowedRoutes.namespaces.from: All (istio-gateway
					// component).  hooks.holos.localhost matches the listener
					// hostname *.holos.localhost and resolves to 127.0.0.1 on
					// the host per docs/local-cluster.md.
					HTTPRoute: (NAME): {
						apiVersion: "gateway.networking.k8s.io/v1"
						kind:       "HTTPRoute"
						metadata:   METADATA
						spec: {
							parentRefs: [{
								name:      "default"
								namespace: "istio-gateways"
							}]
							hostnames: ["hooks.holos.localhost"]
							rules: [{
								// The webhook ingest surface is POST
								// /webhooks/{source}; a PathPrefix match on
								// /webhooks/ admits every source without
								// enumerating them, which is acceptable here
								// because the body is opaque and the receiver
								// authenticates/authorizes nothing it forwards —
								// keeping the Gateway surface to exactly the
								// ingest prefix is the minimal route.  The match
								// pins method POST too, so the Gateway forwards
								// only the verb the receiver contract serves
								// (the handler 405s every other method anyway —
								// this keeps the route aligned with the API and
								// drops other verbs at the edge).  /healthz and
								// /readyz are deliberately NOT routed through the
								// Gateway: they are kubelet-facing probes (exempt
								// from ambient capture), so exposing them
								// externally would only widen the surface.
								matches: [
									{
										path: {type: "PathPrefix", value: "/webhooks/"}
										method: "POST"
									},
								]
								backendRefs: [{
									name: NAME
									port: PORT
								}]
							}]
						}
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

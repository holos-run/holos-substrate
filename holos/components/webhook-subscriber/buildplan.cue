package holos

// webhook-subscriber is the durable JetStream consumer that drains the NATS
// WEBHOOKS WorkQueue stream: for each raw webhook body it parses the payload,
// publishes one DeployTask per tag to the TASKS stream (tasks.deploy), and
// acks the raw message only once every publish is acked (ADR-9, ADR-13).  It
// emits a Deployment running the holos-paas image with the webhook-subscriber
// subcommand and nothing else — unlike the webhook-receiver, it serves no
// inbound business HTTP, so it has NO Service and NO HTTPRoute.  It is modeled
// on the webhook-receiver component and shares its hardened pod posture.
//
// The webhook-subscriber Namespace — including its ambient mesh enrollment
// label — is registered in the central namespaces registry
// (holos/namespaces.cue) and rendered by the namespaces component; this
// component emits no Namespace.  Enrollment is load-bearing: the subscriber
// is a NATS client in its own namespace, and the nats AuthorizationPolicy
// (components/nats/buildplan.cue) ALLOWs this namespace as a source on the
// client port (4222) — a policy that only takes effect when both peers are
// captured by ztunnel.

// IMAGE pins the holos-paas image published to the in-cluster k3d registry by
// HOL-1197 (k3d-k3d-registry.holos.localhost:5100/holos-paas).  The cluster is created
// with --registry-usek3d-registry.holos.localhost:5100, so images pushed to
//k3d-registry.holos.localhost:5100 are pullable in-cluster (docs/local-cluster.md).
// The :dev tag matches the Makefile's IMAGE_TAG default (docker-build /
// docker-push); bump it here in lockstep when publishing a new tag.  The image
// is built for linux/arm64 (the cluster is k3d on OrbStack/Apple silicon).
let IMAGE = "k3d-registry.holos.localhost:5100/holos-paas:dev"

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure: if
// "webhook-subscriber" is ever removed or renamed in holos/namespaces.cue,
// rendering fails here instead of at apply time with a NotFound namespace
// error.
let NAMESPACE = "webhook-subscriber" & #RegisteredNamespace
let NAME = "webhook-subscriber"

// PORT is the subscriber's health/readiness HTTP port (the subcommand's
// default :8080 listen address, HOL-1203).  It is exposed ONLY as a
// containerPort for the kubelet probes below — there is no Service and no
// HTTPRoute, because the subscriber serves no inbound business traffic.  The
// health endpoints are kubelet-facing (exempt from ambient capture); the
// webhook-receiver component's comment explains why they must not be widened
// through a Service or the Gateway, and that reasoning applies here verbatim.
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
				// Kubernetes schemas at render time.
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
									// The subscriber talks only to NATS over the
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
										// in the subscriber's flag defaults, so it is
										// not repeated here — keep the arg surface
										// minimal and let the binary own its
										// defaults.
										args: ["webhook-subscriber"]
										// The health port is a containerPort only:
										// the kubelet probes below address it
										// directly.  It is deliberately NOT fronted
										// by a Service or HTTPRoute — /healthz and
										// /readyz are kubelet-facing, and the
										// subscriber serves no inbound business
										// traffic to widen the surface for.
										ports: [{
											name:          "health"
											containerPort: PORT
											protocol:      "TCP"
										}]
										// /readyz reports 200 only once the
										// subscriber is connected to NATS, so a pod
										// that cannot reach the broker is held out
										// of the rollout gate in scripts/apply until
										// it can consume.  /healthz is unconditional
										// liveness — it stays 200 through a NATS
										// outage so the kubelet does not restart a
										// pod that is correctly waiting to
										// reconnect.
										readinessProbe: httpGet: {
											path: "/readyz"
											port: PORT
										}
										livenessProbe: httpGet: {
											path: "/healthz"
											port: PORT
										}
										// A modest QoS floor for a thin, I/O-bound
										// NATS consumer; it idles far below these
										// (the webhook-receiver precedent).
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

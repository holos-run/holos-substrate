package holos

// nats renders the NATS JetStream server from the official upstream NATS Helm
// chart, vendored unmodified, with every choice expressed through injected
// Helm values.  It is a single-replica StatefulSet with filesystem-backed
// JetStream on a local-path PVC, a headless Service (peer discovery) and a
// client Service (in-cluster clients), a laptop footprint (ADR-7: one local
// instance) with clustering disabled.  No authentication is configured this
// phase (MVP posture); NATS listens for in-cluster clients only on port 4222.
//
// The server was brought up render-only in HOL-1192 (registered in the
// platform, rendered into the committed deploy tree).  This phase (HOL-1193)
// makes the backbone live and self-bootstrapping: it adds the stream
// bootstrap Job below — which creates the two file-backed WorkQueue streams
// (WEBHOOKS, TASKS) idempotently — and integrates the component into
// scripts/apply with a wait_nats() gate (the bootstrap Job completion plus
// the StatefulSet rollout).  This mirrors the Argo CD bring-up split
// (render-only in HOL-1186, apply integration in HOL-1187).
//
// The nats Namespace — including its ambient mesh enrollment label — is
// registered in the central namespaces registry (holos/namespaces.cue) and
// rendered by the namespaces component; this component emits no Namespace.

// NATSChartVersion pins the upstream nats Helm chart.  Chart 2.14.2 installs
// NATS server app version 2.14.2 (the chart's appVersion — chart and app
// versions track together in this chart) and is the latest release from the
// official repository https://nats-io.github.io/k8s/helm/charts/ (verified
// 2026-06-13 via `helm search repo nats/nats --versions`).  The
// nats:2.14.2-alpine container image (the chart default) is a multi-arch
// manifest list including linux/arm64 — required because the cluster is k3d
// on OrbStack/Apple silicon.  Before bumping, re-check the chart's appVersion
// and that the pinned image tag still publishes linux/arm64.
let NATSChartVersion = "2.14.2"

// NATSRepository is the official upstream NATS Helm chart repository.
let NATSRepository = {
	name: "nats"
	url:  "https://nats-io.github.io/k8s/helm/charts/"
}

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure: if
// "nats" is ever removed or renamed in holos/namespaces.cue, rendering fails
// here instead of at apply time with a NotFound namespace error.
let NAMESPACE = "nats" & #RegisteredNamespace

let NAME = "nats"

// WS_PORT is the NATS WebSocket listener port.  The shared Istio Gateway
// terminates TLS for wss://nats.holos.localhost and forwards plain ws to this
// port on the nats Service (HOL-1228).  8080 is the chart's default websocket
// port (vendor/2.14.2/nats/values.yaml); pinning it here keeps the Helm
// config, the HTTPRoute backendRef, and the AuthorizationPolicy rule in sync.
let WS_PORT = 8080

// GATEWAY_NAMESPACE is the namespace hosting the auto-provisioned shared
// Gateway pods (istio-gateway component).  The HTTPRoute attaches to the
// "default" Gateway there and the AuthorizationPolicy ALLOWs this namespace as
// a source on the WebSocket port.  Unifying with #RegisteredNamespace
// (holos/namespaces.cue) makes a rename or removal of that namespace a render
// failure here rather than a silent cross-namespace deny at the WebSocket port.
let GATEWAY_NAMESPACE = "istio-gateways" & #RegisteredNamespace

// The NATS server runs without authentication this phase (MVP posture), so
// "in-cluster clients only" cannot rely on the broker itself — Istio ambient
// enrollment (holos/namespaces.cue) provides mTLS transport and L4 identity,
// but not default-deny access control.  This AuthorizationPolicy makes the
// in-cluster-only claim true by construction the same way the quay component's
// REDIS_AUTHZ does for unauthenticated Redis: it selects the NATS pods and
// ALLOWs only same-namespace sources, so arbitrary cross-namespace pods cannot
// reach the unauthenticated client port (4222) or the monitoring endpoint
// (8222) — Codex flagged both as cluster-wide-reachable without a policy.
// Kubelet health probes are exempt from ambient capture, so the StatefulSet's
// /healthz probes on the monitor port keep working.  The webhook-receiver
// (HOL-1198) and webhook-subscriber (HOL-1204) namespaces are now ALLOWed as
// clients on the client port (rules 2 and 3 below); tightening those from
// namespace- to per-ServiceAccount granularity, once NATS gains in-cluster
// authentication, is the remaining future work.
//
// RECEIVER_NAMESPACE is the webhook-receiver component's namespace
// (holos/components/webhook-receiver/buildplan.cue), added to the ALLOW rule's
// source namespaces so the receiver may publish raw webhook bodies to the
// WEBHOOKS stream from its own namespace (HOL-1198).  Namespace-granularity is
// the right scope this MVP phase — NATS is unauthenticated, so there is no
// principal to bind to yet; the per-ServiceAccount tightening is the future
// work noted above.  Unifying with #RegisteredNamespace
// makes a rename or removal of that namespace a render failure here rather than
// a silent cross-namespace deny at the client port.
let RECEIVER_NAMESPACE = "webhook-receiver" & #RegisteredNamespace

// SUBSCRIBER_NAMESPACE is the webhook-subscriber component's namespace
// (holos/components/webhook-subscriber/buildplan.cue), added to the ALLOW
// rules so the durable JetStream consumer may reach the WEBHOOKS and TASKS
// streams from its own namespace (HOL-1204) — it pulls raw webhook bodies off
// WEBHOOKS and publishes DeployTasks to TASKS, both on the client port (4222).
// Like the receiver this is namespace-granularity, the right scope this MVP
// phase — NATS is unauthenticated, so there is no principal to bind to yet;
// the per-ServiceAccount tightening is the future work noted
// above.  Unifying with #RegisteredNamespace makes a rename or removal of that
// namespace a render failure here rather than a silent cross-namespace deny at
// the client port.
let SUBSCRIBER_NAMESPACE = "webhook-subscriber" & #RegisteredNamespace
let AUTHZ = {
	apiVersion: "security.istio.io/v1"
	kind:       "AuthorizationPolicy"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		// The chart's StatefulSet pod labels (verified against the rendered
		// statefulset-nats.yaml; re-verify when bumping NATSChartVersion).
		selector: matchLabels: {
			"app.kubernetes.io/name":      NAME
			"app.kubernetes.io/instance":  NAME
			"app.kubernetes.io/component": NAME
		}
		action: "ALLOW"
		// Four rules, each least-privilege.  An ALLOW policy denies everything
		// no rule matches, so every other cross-namespace pod is rejected.  The
		// receiver and subscriber namespaces are both admitted explicitly below;
		// tightening them to per-ServiceAccount principals is future work, once
		// NATS gains in-cluster authentication.
		//
		//   1. Same-namespace sources reach every port: the bootstrap Job below
		//      runs in this namespace and needs the client port (4222) to create
		//      the streams, and in-namespace operators may scrape the monitoring
		//      endpoint (8222).
		//   2. The webhook-receiver namespace (HOL-1198) reaches ONLY the client
		//      port (4222) so it can publish to the WEBHOOKS stream — it has no
		//      business on the unauthenticated monitoring endpoint (8222), so the
		//      to.operation.ports restriction keeps that surface same-namespace
		//      only.  The port is a string because Istio matches operation ports
		//      as strings.
		//   3. The webhook-subscriber namespace (HOL-1204) reaches ONLY the
		//      client port (4222), mirroring rule 2: the durable consumer pulls
		//      raw bodies off WEBHOOKS and publishes DeployTasks to TASKS, both
		//      over the client port, and has no business on the monitoring
		//      endpoint (8222).
		//   4. The shared Gateway namespace (istio-gateways) reaches ONLY the
		//      WebSocket port (8080), mirroring rules 2 and 3.  The Gateway
		//      terminates wss://nats.holos.localhost and forwards plain ws to the
		//      unauthenticated WebSocket port for host-facing debugging
		//      (HOL-1228), consistent with the MVP no-auth posture; it has no
		//      business on the client (4222) or monitoring (8222) ports, so the
		//      to.operation.ports restriction keeps those surfaces off the
		//      Gateway.  istio-gateways is deliberately NOT ambient-enrolled, but
		//      the Gateway proxy presents its own SPIFFE identity
		//      (ns/istio-gateways) to the ambient NATS backend, so this namespace
		//      rule matches at L4.
		rules: [
			{
				from: [{source: namespaces: [NAMESPACE]}]
			},
			{
				from: [{source: namespaces: [RECEIVER_NAMESPACE]}]
				to: [{operation: ports: ["4222"]}]
			},
			{
				from: [{source: namespaces: [SUBSCRIBER_NAMESPACE]}]
				to: [{operation: ports: ["4222"]}]
			},
			{
				from: [{source: namespaces: [GATEWAY_NAMESPACE]}]
				to: [{operation: ports: ["\(WS_PORT)"]}]
			},
		]
	}
}

// HTTPROUTE attaches NATS to the shared Istio Gateway so the in-cluster
// WebSocket listener is reachable from the host at wss://nats.holos.localhost
// (HOL-1228).  Modeled on the webhook-receiver HTTPRoute
// (components/webhook-receiver/buildplan.cue): cross-namespace attachment to
// the "default" Gateway is allowed because the shared listener sets
// allowedRoutes.namespaces.from: All and terminates TLS for *.holos.localhost
// (istio-gateway component), so no ReferenceGrant is needed.
// nats.holos.localhost matches that wildcard and resolves to 127.0.0.1 on the
// host per docs/local-cluster.md.  The single rule carries no matches — a bare
// rule matches all paths, correct here because the NATS WebSocket endpoint
// serves the upgrade handshake at /; Istio performs the WebSocket upgrade
// transparently over the HTTP route, forwarding to the Service's WebSocket
// port (8080).
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		parentRefs: [{name: "default", namespace: GATEWAY_NAMESPACE}]
		hostnames: ["nats.holos.localhost"]
		rules: [{
			backendRefs: [{name: NAME, port: WS_PORT}]
		}]
	}
}

// NATS_URL is the in-cluster client endpoint the bootstrap Job connects to:
// the chart's client Service (name "nats", port 4222 — verified against the
// rendered service-nats.yaml) in this component's namespace.
let NATS_URL = "nats://\(NAME).\(NAMESPACE).svc.cluster.local:4222"

// NATS_BOX_IMAGE pins the nats-box image the bootstrap Job runs the `nats`
// CLI from.  natsio/nats-box:0.19.7 is the tag the vendored chart pins for
// its own nats-box (vendor/2.14.2/nats/values.yaml) — reuse it so the CLI
// version tracks the chart.  The image is a multi-arch manifest list
// including linux/arm64 (required because the cluster is k3d on
// OrbStack/Apple silicon).  The image declares no USER and defaults to /root,
// so the Job's pod securityContext below enforces non-root (uid 65534) and a
// /tmp working directory.  Keep this in sync with the chart's
// natsBox.container.image.tag when bumping NATSChartVersion.
let NATS_BOX_IMAGE = "natsio/nats-box:0.19.7"

// BOOTSTRAP is the name of the stream bootstrap Job and its pod label.  It
// carries its own app.kubernetes.io/name — NOT the StatefulSet's "nats" —
// because the chart's client Service selects on that label: a probe-less
// bootstrap pod labeled like the server would become a dead Service endpoint
// for the seconds it runs whenever the Job re-runs after TTL garbage
// collection (the quay BOOTSTRAP_METADATA precedent).
let BOOTSTRAP = "nats-stream-bootstrap"

let BOOTSTRAP_METADATA = {
	name:      BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": BOOTSTRAP
}

// The bootstrap script: create the two file-backed WorkQueue streams the
// platform's NATS backbone needs (HOL-1120) idempotently against the
// in-cluster NATS client endpoint.  WEBHOOKS ingests raw inbound webhooks
// (subjects webhooks.>) and TASKS carries internal work (subjects tasks.>).
// The webhooks.> wildcard captures the documented producer subject
// webhooks.quay (ADR-13's "Raw event published to NATS: webhooks.quay on
// WEBHOOKS"), so a publish from the receiver matches this stream rather than
// being rejected; the narrower webhooks.raw.> the issue quoted from HOL-1120
// predates ADR-13 and would not match webhooks.quay.  tasks.> likewise
// captures tasks.render and tasks.deploy (ADR-13).  WorkQueue retention holds
// each message until a consumer acks it and then removes it; file storage
// persists the queue across a NATS pod restart.  Keep the stream
// names/subjects in sync with ADR-13 and the subject hierarchy documented in
// HOL-1194.
//
// Idempotency has two layers so the Job is safe to re-run under
// `kubectl apply` (and after TTL garbage collection):
//
//   - `nats stream info <NAME>` gates `nats stream add`, so an existing
//     stream is never re-created (add errors on a duplicate name).
//   - `nats stream edit --force` then converges an existing stream's
//     mutable config (the declared subjects and retention) without
//     prompting, so a change to a stream definition reconciles on the next
//     apply rather than drifting silently.  Storage is immutable after
//     creation and is set only by `stream add` (see converge_stream).
//
// `nats stream add --defaults` accepts the CLI defaults for every option not
// given explicitly (so the command never blocks on an interactive prompt),
// while the explicit flags pin the three properties the acceptance criteria
// name: --subjects, --retention=work (WorkQueue), --storage=file.
//
// The leading reachability loop tolerates the gate ordering in scripts/apply
// polling the Job before the StatefulSet is necessarily serving: it retries
// `nats server check connection` until NATS answers (or ~2 min elapses, after
// which the script exits non-zero and the Job's backoffLimit re-runs the
// pod).  `set -eu` makes any unexpected CLI error fail the pod rather than
// leaving a stream half-created.  converge_stream centralizes the
// info-gated create-or-edit so each stream is one line below.
let BOOTSTRAP_SCRIPT = """
	set -eu
	SERVER="\(NATS_URL)"
	echo "Waiting for NATS at ${SERVER} ..."
	i=0
	until nats --server "${SERVER}" server check connection >/dev/null 2>&1; do
	  i=$((i + 1))
	  if [ "$i" -ge 60 ]; then
	    echo "NATS did not become reachable in time" >&2
	    exit 1
	  fi
	  sleep 2
	done
	echo "NATS is reachable; converging streams."
	converge_stream() {
	  name="$1"
	  subjects="$2"
	  if nats --server "${SERVER}" stream info "${name}" >/dev/null 2>&1; then
	    echo "Stream ${name} exists; converging config."
	    # `stream edit` only accepts mutable fields: --storage is rejected
	    # ("unknown long flag '--storage'") because a stream's storage backend
	    # is immutable after creation, so converge only the subjects (and
	    # re-assert the retention policy, which edit does accept).  The
	    # storage=file guarantee is established by `stream add` below and never
	    # changes.
	    nats --server "${SERVER}" stream edit --force \\
	      --subjects="${subjects}" --retention=work "${name}"
	  else
	    echo "Creating stream ${name}."
	    nats --server "${SERVER}" stream add --defaults \\
	      --subjects="${subjects}" --retention=work --storage=file "${name}"
	  fi
	}
	converge_stream WEBHOOKS 'webhooks.>'
	converge_stream TASKS 'tasks.>'
	echo "Stream bootstrap complete."
	"""

// CAVEAT: a completed Job's pod template is immutable.  Server-side re-apply
// of this unchanged spec is a no-op while the Job exists, and
// ttlSecondsAfterFinished garbage-collects it a day after completion — after
// that a re-apply recreates the Job, which converges the already-created
// streams and exits 0.  Only a pod-template change within the TTL window
// (e.g. editing BOOTSTRAP_SCRIPT) requires deleting the old Job
// first (kubectl -n nats delete job nats-stream-bootstrap) — the streams it
// created survive in JetStream's file store, and the new Job converges them.
let BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   BOOTSTRAP_METADATA
	spec: {
		// The reachability loop inside the script already tolerates a NATS
		// that is not yet serving; backoffLimit covers the rarer case of the
		// pod itself failing (e.g. an image pull blip) before the loop runs.
		backoffLimit: 3
		// A day keeps the Job's logs around for debugging a fresh bootstrap
		// while still dissolving the immutable-pod-template caveat above for
		// routine re-applies (the quay BOOTSTRAP_JOB precedent).
		ttlSecondsAfterFinished: 86400
		template: {
			// The distinct label matters most here: the chart's client
			// Service must never select this pod (see BOOTSTRAP_METADATA).
			metadata: labels: BOOTSTRAP_METADATA.labels
			spec: {
				restartPolicy: "Never"
				// The Job talks to NATS over the network, never to the
				// Kubernetes API, so it needs no ServiceAccount token.
				automountServiceAccountToken: false
				securityContext: {
					runAsNonRoot: true
					// The nats-box image declares no non-root USER; pick the
					// conventional "nobody" uid (the quay bootstrap precedent).
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "bootstrap"
					image: NATS_BOX_IMAGE
					command: ["/bin/sh", "-c", BOOTSTRAP_SCRIPT]
					// The nats-box image's working directory is /root (mode
					// 700, owned by root), which uid 65534 cannot even stat —
					// and the nats CLI stats the working directory while
					// validating a stream config, so `stream add` fails with
					// "stat .: permission denied" from /root.  Run from /tmp
					// (the writable emptyDir) instead so the stat — and the
					// CLI's context/state writes under $HOME, also pointed at
					// /tmp — succeed under the read-only root filesystem.
					workingDir: "/tmp"
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
						name:       "nats"
						version:    NATSChartVersion
						release:    NAME
						repository: NATSRepository
					}
					values: {
						// Helm derives version-gated template output from the
						// helm binary's compiled-in default Kubernetes version
						// unless overridden; pin it to the local cluster's k3s
						// version — v1.31.5, the k3d v5.8.3 default image, per
						// the CertManagerVersion pin comment in
						// components/cert-manager/cert-manager.cue — so
						// rendering is deterministic across helm versions on
						// contributor machines and CI.  Keep in sync with that
						// comment when the cluster's k3s version moves.
						kubeVersionOverride: "1.31.5"
						config: {
							// JetStream with filesystem persistence on a PVC.
							jetstream: {
								enabled: true
								fileStore: {
									enabled: true
									// storageClassName is deliberately omitted
									// (left null, the chart default): the claim
									// binds to the k3s default local-path
									// StorageClass on the local cluster — the
									// quay and cnpg-clusters PVC precedent.  2Gi
									// is ample for the WorkQueue streams the next
									// phase creates on a laptop (ADR-7).
									pvc: size: "2Gi"
								}
							}
							// Single server — no clustering (out of scope this
							// phase).  With cluster disabled the StatefulSet
							// runs a single replica (the chart default).
							cluster: enabled: false
							// WebSocket listener for host-facing wss debugging
							// access (HOL-1228): the shared Istio Gateway
							// terminates TLS for wss://nats.holos.localhost and
							// forwards plain ws to this port.  tls.enabled stays
							// false so NATS serves plain ws (the chart emits
							// `no_tls: true` in files/config/websocket.yaml) and
							// the Gateway owns TLS termination.  The chart's
							// service.ports.websocket.enabled default is already
							// true, so enabling this config block exposes port
							// 8080 on the nats Service (verified against
							// vendor/2.14.2/nats/files/service.yaml, which emits
							// a port only when both config.<proto>.enabled and
							// service.ports.<proto>.enabled are true).  No auth
							// is configured (MVP no-auth posture) — see the
							// container block below.
							websocket: {
								enabled: true
								port:    WS_PORT
								tls: enabled: false
							}
						}
						// Laptop footprint (ADR-7): modest requests with a
						// memory limit; a single-instance in-cluster message
						// broker idles far below these.  No CPU limit — a limit
						// reserves nothing and only throttles, and the broker
						// is bursty on stream operations.
						//
						// No authentication (MVP posture — deferred): NATS
						// listens for in-cluster clients only.  The nats
						// namespace is ambient-enrolled (holos/namespaces.cue),
						// so the client hop is secured by the mesh at L4.  The
						// chart leaves auth disabled by default, so nothing is
						// set here to enable it.
						container: resources: {
							requests: {
								cpu:    "50m"
								memory: "64Mi"
							}
							limits: memory: "256Mi"
						}
						// Laptop footprint (ADR-7): a single-replica server has
						// nothing to disrupt, so the chart's default
						// PodDisruptionBudget is noise — disable it, the argocd
						// precedent (every workload's pdb.enabled: false).
						podDisruptionBudget: enabled: false
						// nats-box is a debugging utility Deployment (a shell
						// with the nats CLI).  Not part of the server bring-up
						// and not needed to render the StatefulSet + Services;
						// disable it to keep the footprint minimal.  Stream
						// creation runs as its own bootstrap Job (below), not
						// from this pod.
						natsBox: enabled: false
					}
				}
			}, {
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored AuthorizationPolicy, HTTPRoute, and stream
				// bootstrap Job validate against the vendored Istio,
				// Gateway API, and Kubernetes schemas at render time.
				resources: #Resources & {
					AuthorizationPolicy: (AUTHZ.metadata.name): AUTHZ
					HTTPRoute: (HTTPROUTE.metadata.name):       HTTPROUTE
					Job: (BOOTSTRAP_JOB.metadata.name):         BOOTSTRAP_JOB
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: {
						// Forces nats onto every namespaced resource.  The
						// chart emits nothing destined for another namespace
						// today; re-verify that assumption when bumping
						// NATSChartVersion.
						namespace: NAMESPACE
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

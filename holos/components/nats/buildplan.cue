package holos

// nats renders the NATS JetStream server from the official upstream NATS Helm
// chart, vendored unmodified, with every choice expressed through injected
// Helm values.  It is a single-replica StatefulSet with filesystem-backed
// JetStream on a local-path PVC, a headless Service (peer discovery) and a
// client Service (in-cluster clients), a laptop footprint (ADR-7: one local
// instance) with clustering disabled.  No authentication is configured this
// phase (MVP posture); NATS listens for in-cluster clients only on port 4222.
//
// Render-only in this phase (HOL-1192): the component is registered in the
// platform and renders into the committed deploy tree, but is NOT yet added
// to scripts/apply and no streams are created — those land in the next phase
// (HOL-1193).  This mirrors the Argo CD bring-up split (render-only in
// HOL-1186, apply integration in HOL-1187).
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
// /healthz probes on the monitor port keep working.  The next phase (HOL-1193)
// extends this to ALLOW the specific producer/consumer ServiceAccounts in
// other namespaces as clients are introduced.
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
		// Allow only sources in the nats namespace.  An ALLOW policy with a
		// rule denies everything the rule does not match, so cross-namespace
		// pods are rejected until HOL-1193 adds their principals explicitly.
		rules: [{
			from: [{source: namespaces: [NAMESPACE]}]
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
						// and not needed to render the StatefulSet + Services
						// this phase requires; disable it to keep the footprint
						// minimal.  Stream creation in the next phase (HOL-1193)
						// runs as its own Job, not from this pod.
						natsBox: enabled: false
					}
				}
			}, {
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored AuthorizationPolicy validates against the
				// vendored Istio schema at render time.
				resources: #Resources & {
					AuthorizationPolicy: (AUTHZ.metadata.name): AUTHZ
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

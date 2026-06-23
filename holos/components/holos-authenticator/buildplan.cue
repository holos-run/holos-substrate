package holos

// holos-authenticator deploys the Holos Authenticator (ADR-23): a
// controller-runtime manager that runs an Envoy ext_authz gRPC server and
// reconciles authenticator.holos.run Backend custom resources.  Each Backend
// fronts one Kubernetes API server with OIDC token validation and Kubernetes
// impersonation, so Envoy forwards an authenticated request straight to the API
// server with Impersonate-User/Impersonate-Group headers set.
//
// This component is platform-native: it renders the runtime manifests the
// platform applies (the manager Deployment + RBAC + Service + the generated
// Backend CRD + an AuthorizationPolicy + one example Backend CR), unlike the
// holos-controller which is deployed via the config/ kustomize tree
// (make controller-deploy).  Use components/echo/buildplan.cue as the
// CUE-native workload template (Resources generator → Kustomize → kubectl-slice)
// and components/quay for the AuthorizationPolicy shape.
//
// The holos-authenticator Namespace — including its ambient mesh enrollment
// label — is registered in the central namespaces registry
// (holos/namespaces.cue) and rendered by the namespaces component; this
// component emits NO Namespace (the no-inline-Namespace guardrail).

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent drift
// between this literal and the registry entry into a render failure: if
// "holos-authenticator" is ever removed or renamed in holos/namespaces.cue,
// rendering fails here instead of at apply time with a NotFound namespace error.
let NAMESPACE = "holos-authenticator" & #RegisteredNamespace
let NAME = "holos-authenticator"

// IMAGE is the manager container image.  The platform's in-cluster Quay registry
// holds the multi-arch holos-authenticator image (make controller targets, the
// Images CI workflow); the :dev tag is the convention the local cluster pulls,
// matching the holos-controller image tag convention.
let IMAGE = "quay.holos.internal/holos/holos-authenticator:dev"

// The manager flag/port contract is the single source of truth in
// cmd/holos-authenticator/main.go: the Prometheus metrics endpoint binds :8080,
// the health/readiness probe endpoint binds :8081, and the Envoy ext_authz gRPC
// server binds :9000.  Keep these in lock-step with main.go's flag defaults.
let GRPC_PORT = 9000
let METRICS_PORT = 8080
let HEALTH_PORT = 8081

let METADATA = {
	name:      NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": NAME
}

// SELECTOR is the pod selector shared by the Deployment, Service, and the
// AuthorizationPolicy's selector — a stable subset of labels (NOT the full
// METADATA.labels, which a Service selector should keep minimal).
let SELECTOR = {"app.kubernetes.io/name": NAME}

userDefinedBuildPlan: {
	metadata: name: "holos-authenticator"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [
				{
					kind:   "Resources"
					output: "resources.gen.yaml"
					// Unify with #Resources (holos/resources.cue) so the
					// hand-authored resources validate against the vendored
					// Kubernetes / Istio / authenticator schemas at render time.
					resources: #Resources & {
						// The manager Deployment: one replica (leader election makes
						// scaling safe but the local cluster runs one), the ext_authz
						// gRPC server plus metrics and health endpoints, POD_NAMESPACE
						// via the downward API so the credential resolver reads
						// per-Backend Secrets from the namespace the manager runs in
						// (mirroring the holos-controller manager).
						Deployment: (NAME): {
							apiVersion: "apps/v1"
							kind:       "Deployment"
							metadata:   METADATA
							spec: {
								replicas: 1
								selector: matchLabels: SELECTOR
								template: {
									metadata: labels: METADATA.labels
									spec: {
										serviceAccountName: NAME
										securityContext: {
											runAsNonRoot: true
											seccompProfile: type: "RuntimeDefault"
										}
										containers: [{
											name:            "manager"
											image:           IMAGE
											imagePullPolicy: "Always"
											args: [
												"--leader-elect",
												"--health-probe-bind-address=:\(HEALTH_PORT)",
												"--metrics-bind-address=:\(METRICS_PORT)",
												"--grpc-bind-address=:\(GRPC_PORT)",
											]
											env: [{
												// Expose the manager's own namespace via the
												// downward API so the per-Backend
												// credentialsSecretRef resolver reads from the
												// namespace the manager actually runs in.
												name: "POD_NAMESPACE"
												valueFrom: fieldRef: fieldPath: "metadata.namespace"
											}]
											ports: [
												{
													name:          "grpc"
													containerPort: GRPC_PORT
													protocol:      "TCP"
												},
												{
													name:          "metrics"
													containerPort: METRICS_PORT
													protocol:      "TCP"
												},
												{
													name:          "health"
													containerPort: HEALTH_PORT
													protocol:      "TCP"
												},
											]
											livenessProbe: httpGet: {
												path: "/healthz"
												port: HEALTH_PORT
											}
											readinessProbe: httpGet: {
												path: "/readyz"
												port: HEALTH_PORT
											}
											securityContext: {
												allowPrivilegeEscalation: false
												readOnlyRootFilesystem:    true
												capabilities: drop: ["ALL"]
											}
											resources: {
												requests: {
													cpu:    "10m"
													memory: "64Mi"
												}
												limits: {
													cpu:    "500m"
													memory: "128Mi"
												}
											}
										}]
										terminationGracePeriodSeconds: 10
									}
								}
							}
						}

						// The manager's identity.  The ClusterRoleBinding below binds
						// the generated holos-authenticator ClusterRole to it.
						ServiceAccount: (NAME): {
							apiVersion: "v1"
							kind:       "ServiceAccount"
							metadata:   METADATA
						}

						// The generated ClusterRole (config/authenticator/rbac/role.yaml,
						// holos-authenticator-role): watch Backends, update their
						// status/finalizers, manage leader-election Leases, and emit
						// Events.  Authored here from the generated rules so the role
						// ships with the component (the kubebuilder source of truth is
						// config/authenticator/rbac/role.yaml).
						ClusterRole: (NAME): {
							apiVersion: "rbac.authorization.k8s.io/v1"
							kind:       "ClusterRole"
							metadata: {
								name: "holos-authenticator-role"
								labels: METADATA.labels
							}
							rules: [
								{
									apiGroups: [""]
									resources: ["events"]
									verbs: ["create", "patch"]
								},
								{
									apiGroups: ["authenticator.holos.run"]
									resources: ["backends"]
									verbs: ["get", "list", "watch"]
								},
								{
									apiGroups: ["authenticator.holos.run"]
									resources: ["backends/finalizers"]
									verbs: ["update"]
								},
								{
									apiGroups: ["authenticator.holos.run"]
									resources: ["backends/status"]
									verbs: ["get", "patch", "update"]
								},
								{
									apiGroups: ["coordination.k8s.io"]
									resources: ["leases"]
									verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
								},
							]
						}

						ClusterRoleBinding: (NAME): {
							apiVersion: "rbac.authorization.k8s.io/v1"
							kind:       "ClusterRoleBinding"
							metadata: {
								name: "holos-authenticator-rolebinding"
								labels: METADATA.labels
							}
							roleRef: {
								apiGroup: "rbac.authorization.k8s.io"
								kind:     "ClusterRole"
								name:     "holos-authenticator-role"
							}
							subjects: [{
								kind:      "ServiceAccount"
								name:      NAME
								namespace: NAMESPACE
							}]
						}

						// Namespaced Role granting the manager read access to Secrets in
						// its own namespace — the per-Backend impersonator credential
						// (credentialsSecretRef) the authorizer resolves at request time.
						// From the generated config/authenticator/rbac/role.yaml.
						Role: (NAME): {
							apiVersion: "rbac.authorization.k8s.io/v1"
							kind:       "Role"
							metadata: {
								name:      "holos-authenticator-role"
								namespace: NAMESPACE
								labels:    METADATA.labels
							}
							rules: [{
								apiGroups: [""]
								resources: ["secrets"]
								verbs: ["get"]
							}]
						}

						RoleBinding: (NAME): {
							apiVersion: "rbac.authorization.k8s.io/v1"
							kind:       "RoleBinding"
							metadata: {
								name:      "holos-authenticator-rolebinding"
								namespace: NAMESPACE
								labels:    METADATA.labels
							}
							roleRef: {
								apiGroup: "rbac.authorization.k8s.io"
								kind:     "Role"
								name:     "holos-authenticator-role"
							}
							subjects: [{
								kind:      "ServiceAccount"
								name:      NAME
								namespace: NAMESPACE
							}]
						}

						// The Service exposing the ext_authz gRPC port the Istio
						// extensionProvider (components/istio/istio.cue) points at:
						// holos-authenticator.holos-authenticator.svc.cluster.local:9000.
						// The metrics port is exposed too for Prometheus scrape.
						Service: (NAME): {
							apiVersion: "v1"
							kind:       "Service"
							metadata:   METADATA
							spec: {
								selector: SELECTOR
								ports: [
									{
										name:       "grpc"
										port:       GRPC_PORT
										targetPort: GRPC_PORT
										protocol:   "TCP"
									},
									{
										name:       "metrics"
										port:       METRICS_PORT
										targetPort: METRICS_PORT
										protocol:   "TCP"
									},
								]
							}
						}

						// The CUSTOM AuthorizationPolicy: it delegates the authorization
						// decision for the selected workloads to the named extension
						// provider (the holos-authenticator ext_authz gRPC provider
						// declared in istiod's MeshConfig, components/istio/istio.cue).
						// provider.name MUST match the meshConfig.extensionProviders[].name
						// there.  This example selects the authenticator's own pods as a
						// harmless, self-contained default; a real deployment retargets the
						// selector at the protected workload behind a waypoint (L7
						// ext_authz in ambient mode requires a waypoint — ztunnel is
						// L4-only; the full waypoint/ServiceEntry topology is a deferred
						// follow-up recorded in holos/docs/placeholders.md).
						AuthorizationPolicy: (NAME): {
							apiVersion: "security.istio.io/v1"
							kind:       "AuthorizationPolicy"
							metadata:   METADATA
							spec: {
								selector: matchLabels: SELECTOR
								action:   "CUSTOM"
								provider: name: "holos-authenticator"
								// An empty rules entry matches all requests to the selected
								// workloads, sending every one to the ext_authz provider.
								rules: [{}]
							}
						}

						// L4 caller guard on the ext_authz gRPC Service — the decisive
						// fix for the "anyone who can reach :9000 reads back the
						// impersonator credential" hazard.  The Check response carries the
						// backend's privileged Authorization: Bearer <impersonator-token>;
						// the gRPC Service is a normal ClusterIP, so WITHOUT this policy
						// any meshed pod that can dial
						// holos-authenticator.holos-authenticator.svc:9000, present a
						// valid OIDC token for a configured Backend, and read the response
						// could exfiltrate that credential — independent of the deferred
						// waypoint topology.  This ALLOW policy (mTLS source-principal
						// allowlist, the quay-redis pattern) restricts callers to the
						// platform-owned namespaces where the ext_authz client (the Istio
						// waypoint that fronts a protected workload) runs: the
						// authenticator's own namespace and the istio control-plane
						// namespaces.  It is fail-closed against tenants: a CUSTOM-action
						// ALLOW policy denies any source not listed, so until the waypoint
						// is deployed (deferred) NO tenant-namespace pod can reach the
						// gRPC server.  Tighten this to the exact waypoint ServiceAccount
						// principal when the waypoint topology lands (placeholders.md).
						AuthorizationPolicy: "\(NAME)-grpc-callers": {
							apiVersion: "security.istio.io/v1"
							kind:       "AuthorizationPolicy"
							metadata: {
								name:      "\(NAME)-grpc-callers"
								namespace: NAMESPACE
								labels:    METADATA.labels
							}
							spec: {
								selector: matchLabels: SELECTOR
								action: "ALLOW"
								rules: [{
									from: [{
										source: namespaces: [
											NAMESPACE,
											"istio-system",
											"istio-gateways",
										]
									}]
									// Restrict to the gRPC ext_authz port; the metrics port
									// is governed separately (scrape auth on :8080).
									to: [{
										operation: ports: ["\(GRPC_PORT)"]
									}]
								}]
							}
						}

						// One example Backend CR documenting the shape an operator fills
						// in.  It points at the in-cluster API server, validates tokens
						// against the platform Keycloak holos realm, and names the
						// impersonator credential Secret resolved from this namespace.  Its
						// credentialsSecretRef material is created at runtime and NEVER
						// committed (the Runtime Secret Handling guardrail); see README.md.
						// The CABundle fields are intentionally omitted (empty) here so the
						// committed manifest carries no per-cluster trust material — an
						// operator injects the local-ca PEM out of band, mirroring the
						// caBundle convention used by the project/application components.
						Backend: example: {
							apiVersion: "authenticator.holos.run/v1alpha1"
							kind:       "Backend"
							metadata: {
								name:      "example"
								namespace: NAMESPACE
								labels:    METADATA.labels
							}
							spec: {
								// Host is the request :authority the authorizer routes by.
								host: "api.example.holos.internal"
								oidc: {
									issuerURL: "https://keycloak.holos.internal/realms/holos"
									clientID:  "holos-authenticator"
								}
								server: {
									// The in-cluster API server.  An external target would
									// set an external URL and a ServiceEntry+waypoint would
									// front it (deferred, holos/docs/placeholders.md).
									url: "https://kubernetes.default.svc"
								}
								credentialsSecretRef: name: "holos-authenticator-backend-creds"
							}
						}
					}
				},
				{
					// The generated Backend CRD
					// (config/crd/bases/authenticator.holos.run_backends.yaml, vendored
					// here as vendor/customresourcedefinition-backends.yaml) ships with
					// the component so the authenticator.holos.run types are installed
					// alongside the controller — the cert-manager-crds / cnpg-crds /
					// kargo-crds pattern, co-located here because the example Backend CR
					// is in the same component.  cat it into the Kustomize input set so
					// it flows through the same kubectl-slice one-file-per-resource
					// pipeline as the authored resources above.
					kind: "Command"
					command: {
						args: ["cat", "\(BuildContext.rootDir)/\(BuildContext.leafDir)/vendor/customresourcedefinition-backends.yaml"]
						isStdoutOutput: true
					}
					output: "crd.gen.yaml"
				},
			]
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

package holos

// clusters represents the clusters the platform manages, keyed by name.  Each
// registered cluster gets every component registered in the platform:
// components: struct below, parameterized by the clusterName tag.
clusters: [NAME=string]: name: NAME

// k3d-holos is the local development cluster.  See docs/local-cluster.md and
// the k3d/ directory for how the cluster is created.
clusters: "k3d-holos": _

// Register production clusters here when a production deployment area is
// established.  For example:
//
//  clusters: "prod-us-east-1": _

// All components managed on all clusters get merged into one big platform
// structure.  Use holos show platform to inspect the structure holos render
// platform iterates over, rendering each component concurrently.
//
// See https://holos.run/docs/api/author/v1alpha6/#Platform
platform: {
	name: "holos-paas"

	for CLUSTER in clusters {
		components: {
			// namespaces renders the central namespaces registry
			// (holos/namespaces.cue), one file per Namespace resource.  It
			// must apply before every other component: namespaced resources
			// cannot be created until their Namespace exists.  The
			// namespaces: "true" label selects it for apply tooling,
			// analogous to the crds: "true" convention (e.g. holos show
			// buildplans --selector namespaces==true).
			(#ComponentTemplate & {inputs: {
				component: "namespaces"
				cluster:   CLUSTER.name
				labels: namespaces: "true"
			}}).output

			// gateway-api renders the Gateway API standard channel CRDs.  CRDs
			// are isolated components labeled crds: "true" so they apply before
			// the controllers that depend on them.
			(#ComponentTemplate & {inputs: {
				component: "gateway-api"
				cluster:   CLUSTER.name
				labels: {
					app:  "istio"
					crds: "true"
				}
			}}).output

			// cert-manager-crds renders the cert-manager CRDs from the
			// upstream release manifest.  CRDs are isolated components
			// labeled crds: "true" so they apply before the controllers that
			// depend on them.  The controller is the cert-manager component
			// below; both share the version pin in
			// components/cert-manager/cert-manager.cue.
			(#ComponentTemplate & {inputs: {
				name:      "cert-manager-crds"
				component: "crds"
				prefix:    "components/cert-manager"
				cluster:   CLUSTER.name
				labels: {
					app:  "cert-manager"
					crds: "true"
				}
			}}).output

			// Istio ambient-mode control plane, rendered from the upstream
			// Helm charts.  Manifests are applied manually during Layer 0
			// bootstrap in the order documented in holos/README.md, the
			// single authoritative apply order.  istio-base ships the Istio
			// CRDs, so it is labeled crds: "true" and applies before the
			// controllers that depend on them.  Note the webhook
			// failurePolicy caveat in holos/README.md before re-applying
			// istio-base or istiod: istiod manages that field at runtime.
			(#ComponentTemplate & {inputs: {
				name:      "istio-base"
				component: "base"
				prefix:    "components/istio"
				cluster:   CLUSTER.name
				labels: {
					app:  "istio"
					crds: "true"
				}
			}}).output

			(#ComponentTemplate & {inputs: {
				name:      "istiod"
				component: "istiod"
				prefix:    "components/istio"
				cluster:   CLUSTER.name
				labels: app: "istio"
			}}).output

			(#ComponentTemplate & {inputs: {
				name:      "istio-cni"
				component: "cni"
				prefix:    "components/istio"
				cluster:   CLUSTER.name
				labels: app: "istio"
			}}).output

			(#ComponentTemplate & {inputs: {
				name:      "istio-ztunnel"
				component: "ztunnel"
				prefix:    "components/istio"
				cluster:   CLUSTER.name
				labels: app: "istio"
			}}).output

			// cert-manager renders the controller, webhook, and cainjector
			// from the upstream Helm chart.  Applies after istio-ztunnel:
			// the cert-manager namespace is ambient-enrolled, so the mesh
			// dataplane must be capturing traffic when its workloads start.
			(#ComponentTemplate & {inputs: {
				name:      "cert-manager"
				component: "controller"
				prefix:    "components/cert-manager"
				cluster:   CLUSTER.name
				labels: app: "cert-manager"
			}}).output

			// local-ca emits the CA ClusterIssuer that signs every platform
			// certificate, referencing the mkcert root CA Secret staged by
			// scripts/local-ca.  A separate component so the ordered apply
			// can wait for the cert-manager webhook before the ClusterIssuer
			// applies.
			(#ComponentTemplate & {inputs: {
				component: "local-ca"
				cluster:   CLUSTER.name
				labels: app: "cert-manager"
			}}).output

			// istio-gateway emits the shared Gateway all platform services
			// attach HTTPRoutes to, the wildcard TLS Certificate for its
			// HTTPS listener, and its Namespace.  Applies after the Istio
			// control plane components above: the istio GatewayClass must
			// exist and istiod must be running to program the Gateway.
			(#ComponentTemplate & {inputs: {
				component: "istio-gateway"
				cluster:   CLUSTER.name
				labels: app: "istio"
			}}).output

			// echo is the permanent Layer 0 smoke test: an ambient-enrolled
			// echo workload reachable through the shared Gateway.  Its
			// HTTPRoute attaches to the istio-gateway component's Gateway;
			// attachment is level-triggered, so apply order does not matter —
			// a route applied first simply reports unattached until the
			// Gateway exists.
			(#ComponentTemplate & {inputs: {
				component: "echo"
				cluster:   CLUSTER.name
				labels: app: "echo"
			}}).output

			// cnpg-crds renders the CloudNativePG CRDs, filtered out of the
			// single upstream release manifest (upstream ships no CRDs-only
			// asset).  CRDs are isolated components labeled crds: "true" so
			// they apply before the controllers that depend on them.  The
			// operator is the cnpg component below; both share the version
			// pin in components/cnpg/cnpg.cue.
			(#ComponentTemplate & {inputs: {
				name:      "cnpg-crds"
				component: "crds"
				prefix:    "components/cnpg"
				cluster:   CLUSTER.name
				labels: {
					app:  "cnpg"
					crds: "true"
				}
			}}).output

			// cnpg renders the CloudNativePG operator from the same release
			// manifest, CRDs and Namespace stripped.  Applies after
			// istio-ztunnel: the cnpg-system namespace is ambient-enrolled,
			// so the mesh dataplane must be capturing traffic when its
			// workloads start.  Its admission webhooks for postgresql.cnpg.io
			// resources fail closed, so scripts/apply waits for the
			// controller-manager rollout before later components apply
			// Cluster resources.
			(#ComponentTemplate & {inputs: {
				name:      "cnpg"
				component: "operator"
				prefix:    "components/cnpg"
				cluster:   CLUSTER.name
				labels: app: "cnpg"
			}}).output

			// cnpg-clusters emits the per-service Postgres Cluster resources
			// (keycloak-db, quay-db), each in its consuming service's
			// namespace.  Applies after the cnpg operator: its admission
			// webhooks for postgresql.cnpg.io resources fail closed, so
			// scripts/apply waits for the controller-manager rollout and
			// retries this component through the brief window before the
			// webhook serves, then gates on Cluster readiness because the
			// Keycloak phase depends on a reachable database.
			(#ComponentTemplate & {inputs: {
				component: "cnpg-clusters"
				cluster:   CLUSTER.name
				labels: app: "cnpg"
			}}).output

			// keycloak-operator-crds renders the Keycloak operator CRDs
			// (keycloaks.k8s.keycloak.org, keycloakrealmimports.k8s.keycloak.org)
			// from the upstream keycloak-k8s-resources manifests.  CRDs are
			// isolated components labeled crds: "true" so they apply before
			// the controllers that depend on them.  The operator is the
			// keycloak-operator component below; both share the version pin
			// in components/keycloak/keycloak.cue.
			(#ComponentTemplate & {inputs: {
				name:      "keycloak-operator-crds"
				component: "operator-crds"
				prefix:    "components/keycloak"
				cluster:   CLUSTER.name
				labels: {
					app:  "keycloak"
					crds: "true"
				}
			}}).output

			// keycloak-operator renders the Keycloak operator from the
			// upstream kubernetes.yml manifest, placed in the keycloak
			// namespace (deliberately NOT ambient-enrolled, see
			// holos/namespaces.cue).  Applies after cnpg-clusters in
			// scripts/apply, which gates on the operator Deployment rollout:
			// the next phase applies Keycloak/KeycloakRealmImport CRs that
			// need the operator reconciling and the keycloak-db Cluster
			// reachable.
			(#ComponentTemplate & {inputs: {
				name:      "keycloak-operator"
				component: "operator"
				prefix:    "components/keycloak"
				cluster:   CLUSTER.name
				labels: app: "keycloak"
			}}).output

			// keycloak emits the Keycloak server instance: the Keycloak CR,
			// its TLS Certificate, the declarative holos realm import, the
			// HTTPRoute attaching it to the shared Gateway, and the
			// DestinationRule for the Gateway→Keycloak TLS hop.  Applies
			// last in scripts/apply: its CRs need the operator reconciling
			// and the keycloak-db Cluster reachable, and its Certificate
			// needs the cert-manager webhook admitting — hence the retried
			// apply — with gates on the Keycloak CR Ready and realm import
			// Done conditions as the Layer 1 smoke check.
			(#ComponentTemplate & {inputs: {
				name:      "keycloak"
				component: "instance"
				prefix:    "components/keycloak"
				cluster:   CLUSTER.name
				labels: app: "keycloak"
			}}).output

			// keycloak-config reconciles the holos realm with an idempotent
			// keycloak-config-cli Job: the three platform roles, the
			// authenticated default group, and the Argo CD OIDC public PKCE
			// client with the group/realm-role protocol mappers.  Registered
			// immediately after keycloak (instance): the realm shell must be
			// bootstrapped by the instance component's KeycloakRealmImport,
			// and the Keycloak server Ready, before this Job can converge the
			// realm against the live admin API — scripts/apply gates on the
			// Job completing.  Unlike the bootstrap-only KeycloakRealmImport,
			// this Job reconciles on every apply, closing the "Keycloak realm
			// reconciliation" placeholder.
			(#ComponentTemplate & {inputs: {
				name:      "keycloak-config"
				component: "realm-config"
				prefix:    "components/keycloak"
				cluster:   CLUSTER.name
				labels: app: "keycloak"
			}}).output

			// quay emits the Quay registry: the Quay Deployment (config
			// rendered by an initContainer from the committed template plus
			// the CNPG-generated quay-db credentials), its Service, the
			// quay-redis Deployment, Service, and AuthorizationPolicy, the
			// registry blob-storage PVC, the secret-keys bootstrap Job with
			// its scoped RBAC, and the HTTPRoute pair attaching it to the
			// shared Gateway at quay.holos.localhost.  Applies after
			// keycloak in
			// scripts/apply: it needs the quay-db Cluster reachable (gated
			// Ready in the cnpg-clusters step), and appending it keeps the
			// established order stable — its gate waits on the quay
			// Deployment rollout as the smoke check.
			(#ComponentTemplate & {inputs: {
				component: "quay"
				cluster:   CLUSTER.name
				labels: app: "quay"
			}}).output

			// argocd-crds renders the Argo CD CRDs (applications,
			// applicationsets, appprojects in group argoproj.io) from the
			// upstream source tree at the pinned app version.  CRDs are
			// isolated components labeled crds: "true" so they apply before
			// the controllers that depend on them.  The core install is the
			// argocd component below; both share the version pin in
			// components/argocd/argocd.cue.
			(#ComponentTemplate & {inputs: {
				name:      "argocd-crds"
				component: "crds"
				prefix:    "components/argocd"
				cluster:   CLUSTER.name
				labels: {
					app:  "argocd"
					crds: "true"
				}
			}}).output

			// argocd renders the Argo CD core install — application
			// controller, repo server, API/UI server, and single-instance
			// redis — from the upstream argo-cd Helm chart with a laptop
			// footprint, plus the HTTPRoute pair attaching the UI to the
			// shared Gateway at argocd.holos.localhost.  Render-only in this
			// phase (HOL-1186): integration into scripts/apply with health
			// gates lands in the next phase (HOL-1187).
			(#ComponentTemplate & {inputs: {
				name:      "argocd"
				component: "controller"
				prefix:    "components/argocd"
				cluster:   CLUSTER.name
				labels: app: "argocd"
			}}).output

			// nats renders the NATS JetStream server — a single-replica
			// StatefulSet with filesystem-backed JetStream on a local-path
			// PVC, a headless Service and a client Service — from the
			// official upstream NATS Helm chart with a laptop footprint.
			// Render-only in this phase (HOL-1192): integration into
			// scripts/apply and the WorkQueue stream creation land in the
			// next phase (HOL-1193).
			(#ComponentTemplate & {inputs: {
				component: "nats"
				cluster:   CLUSTER.name
				labels: app: "nats"
			}}).output

			// webhook-receiver is the thin HTTP ingress that publishes raw
			// inbound webhook bodies to the NATS WEBHOOKS stream: a Deployment
			// running the holos-paas image (webhook-receiver subcommand), a
			// Service, and an HTTPRoute attaching it to the shared Gateway at
			// hooks.holos.localhost.  Registered after nats: it publishes into
			// the NATS backbone, so the stream must exist for an end-to-end
			// publish to land, and the nats AuthorizationPolicy ALLOWs this
			// component's namespace as a client source.  Its HTTPRoute attaches
			// to the istio-gateway component's Gateway; attachment is
			// level-triggered, so apply order does not matter for the route
			// itself.
			(#ComponentTemplate & {inputs: {
				component: "webhook-receiver"
				cluster:   CLUSTER.name
				labels: app: "webhook-receiver"
			}}).output

			// webhook-subscriber is the durable JetStream consumer that
			// drains the NATS WEBHOOKS stream and publishes DeployTasks to
			// the TASKS stream: a Deployment running the holos-paas image
			// (webhook-subscriber subcommand) and nothing else — unlike the
			// receiver it serves no inbound business HTTP, so it has no
			// Service and no HTTPRoute.  Registered after nats and the
			// receiver: it is a NATS client, so the streams must exist for a
			// consume/publish to land, and the nats AuthorizationPolicy
			// ALLOWs this component's namespace as a client source on port
			// 4222.  Nothing during bootstrap depends on it, so appending it
			// keeps the established order stable.
			(#ComponentTemplate & {inputs: {
				component: "webhook-subscriber"
				cluster:   CLUSTER.name
				labels: app: "webhook-subscriber"
			}}).output

			// kargo-crds renders the Kargo CustomResourceDefinitions
			// (kargo.akuity.io group) sliced from the upstream Kargo Helm
			// chart.  CRDs are isolated components labeled crds: "true" so
			// they apply before the controllers that depend on them.  The
			// controller is the kargo component below; both pin the same chart
			// version in their buildplan.cue (the two are sibling components
			// with no shared CUE ancestor, so the pin is duplicated and a
			// mismatch is visible in the diff).
			(#ComponentTemplate & {inputs: {
				component: "kargo-crds"
				cluster:   CLUSTER.name
				labels: {
					app:  "kargo"
					crds: "true"
				}
			}}).output

			// kargo renders the Kargo control plane — controller, API/UI,
			// management controller, garbage collector, and the internal and
			// external webhooks servers — from the upstream Kargo Helm chart
			// with a laptop footprint and a simplified no-auth posture for the
			// local single-user cluster (HOL-1238).  The API/UI is exposed at
			// kargo.holos.localhost through the shared Gateway.  Registered
			// after kargo-crds: its workloads need the kargo.akuity.io types
			// established first (the crds-before-controllers guardrail).  The
			// controller drives Argo CD via the argocd-update promotion step,
			// so its argocd.namespace Helm value points at this platform's
			// argocd namespace; nothing during bootstrap depends on Kargo, so
			// appending the pair keeps the established order stable.
			(#ComponentTemplate & {inputs: {
				component: "kargo"
				cluster:   CLUSTER.name
				labels: app: "kargo"
			}}).output

			// kargo-project-echo defines the Kargo Project for the echo sample
			// app's delivery pipeline (HOL-1240): a minimal Project resource
			// (mirroring the reference platform's kargo-project-braintrust) that
			// reconciles to the dedicated kargo-echo namespace and carries the
			// auto-promotion policy for the test Stage.  Registered after kargo:
			// the Project is a kargo.akuity.io custom resource, so the CRDs
			// (kargo-crds) and the controller that reconciles it (kargo) must be
			// established first.  scripts/apply gates this on the Project
			// reconciling its namespace before the kargo-echo Warehouse/Stage
			// apply into it.
			(#ComponentTemplate & {inputs: {
				component: "kargo-project-echo"
				cluster:   CLUSTER.name
				labels: app: "kargo"
			}}).output

			// kargo-echo wires the echo delivery pipeline: a Warehouse watching
			// the rendered-manifests OCI artifact scripts/publish pushes, a Stage
			// whose promotion runs argocd-update to repoint the echo Argo CD
			// Application's OCI targetRevision at the new artifact digest, and the
			// target Application itself (HOL-1240).  Registered after
			// kargo-project-echo: the Warehouse and Stage are namespaced into the
			// Project's kargo-echo namespace, which the Project must reconcile
			// (adopt) first, and the Application's authorized-stage annotation
			// references the Project's Stage.
			(#ComponentTemplate & {inputs: {
				component: "kargo-echo"
				cluster:   CLUSTER.name
				labels: app: "kargo"
			}}).output
		}
	}
}

// #ComponentTemplate registers one component for one cluster.  The output
// field unifies into the platform: components: struct keyed so the same
// component may be registered for multiple clusters without collisions.
//
// holos render platform injects each entry's name and path as the
// holos_component_name and holos_component_path tags, parameters as additional
// tags (clusterName), and copies labels and annotations to the BuildPlan.
#ComponentTemplate: {
	inputs: {
		// component represents the directory name of the component under prefix.
		component: string
		// name represents the BuildPlan metadata.name, defaults to component.
		name: string | *component
		// cluster represents the name of the cluster the component renders
		// for, constrained to the names of registered clusters.  Always set
		// this field explicitly at the registration site: with a single
		// registered cluster the disjunction collapses to a concrete value,
		// so an omitted field silently binds to that cluster and breaks with
		// an incomplete-value error once a second cluster is registered.
		cluster: or([for NAME, _ in clusters {NAME}])
		// prefix represents the directory containing the component directory.
		prefix: string | *"components"
		// parameters are injected into the component as CUE @tag variables.
		parameters: {[string]: string}
		labels: {[string]: string}
		annotations: {[string]: string}
	}
	key: "cluster:\(inputs.cluster):component:\(inputs.name)"
	output: (key): {
		name: inputs.name
		path: "\(inputs.prefix)/\(inputs.component)"
		parameters: inputs.parameters & {
			clusterName: inputs.cluster
		}
		// labels are useful for inspecting BuildPlans and rendering a subset of
		// the platform.  For example:
		//  holos show buildplans --selector cluster==k3d-holos
		//  holos render platform --selector cluster==k3d-holos
		labels: {
			inputs.labels
			"path":    path
			cluster:   inputs.cluster
			component: inputs.name
		}
		annotations: {
			inputs.annotations
			"app.holos.run/description": "\(inputs.name) for \(inputs.cluster)"
		}
	}
}

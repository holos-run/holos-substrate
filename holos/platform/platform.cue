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

			// coredns emits the coredns-custom ConfigMap (kube-system) that
			// makes *.holos.internal resolve to the shared Istio gateway
			// Service from inside the cluster, so in-cluster relying parties
			// reach a service by the same public hostname a browser uses.
			// This is the in-cluster half of the .internal migration
			// (HOL-1364): .internal carries no RFC 6761 loopback short-circuit
			// (unlike the retired .localhost names), so CoreDNS can answer it
			// authoritatively.  No CRD or controller dependency — it edits the
			// k3s CoreDNS custom-config ConfigMap, which already exists.
			(#ComponentTemplate & {inputs: {
				component: "coredns"
				cluster:   CLUSTER.name
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
			// namespace (ambient-enrolled like the rest of the namespace,
			// see holos/namespaces.cue).  Applies after cnpg-clusters in
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
			// the declarative holos realm import, and the HTTPRoute attaching
			// it to the shared Gateway.  Keycloak runs HTTP-only behind the
			// Gateway, which terminates external TLS once and forwards
			// plaintext HTTP to keycloak-service:8080 over a ztunnel HBONE
			// mTLS hop (HOL-1362) — no per-pod TLS Certificate and no
			// Gateway→Keycloak re-encryption DestinationRule.  Applies last
			// in scripts/apply: its CRs need the operator reconciling and the
			// keycloak-db Cluster reachable, with gates on the Keycloak CR
			// Ready and realm import Done conditions as the Layer 1 smoke
			// check.
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

			// keycloak-esso-config reconciles the esso realm (HOL-1368) with its
			// own idempotent keycloak-config-cli Job: the confidential broker
			// client (https://auth.holos.internal/realms/holos) and the single
			// pre-provisioned alice user, plus the generate-once bootstrap Job for
			// the shared esso-idp-oidc client secret and alice's password.
			// Registered alongside keycloak-config: the esso realm shell is
			// bootstrapped by the instance component's KeycloakRealmImport, and the
			// Keycloak server Ready, before this Job converges the esso realm.  This
			// phase only adds the rendered component; wiring its apply ordering and
			// gate into scripts/apply is phase 4 (HOL-1370).  It takes NO dependency
			// on the holos-controller API groups (AC #5).
			(#ComponentTemplate & {inputs: {
				name:      "keycloak-esso-config"
				component: "realm-esso-config"
				prefix:    "components/keycloak"
				cluster:   CLUSTER.name
				labels: app: "keycloak"
			}}).output

			// keycloak-instance emits the central keycloak.holos.run
			// KeycloakInstance the shipped Holos Controller reconciles the rest of
			// the keycloak.holos.run Kinds against, plus the security.holos.run
			// ReferenceGrant authorizing project namespaces (my-project) to
			// reference it cross-namespace (HOL-1348, ADR-18/ADR-20).  Registered
			// after keycloak-config: the realm must exist and the controller's
			// admin credential Secret must be provisioned (the realm-config
			// CONTROLLER_CREDS_BOOTSTRAP Job) before the KeycloakInstance is
			// reconcilable.  Its caBundle is injected at apply time via the
			// _CABundlePEM tag (never committed), so a plain `holos render
			// platform` and scripts/render stay diff-clean.
			(#ComponentTemplate & {inputs: {
				name:      "keycloak-instance"
				component: "keycloak-instance"
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
			// shared Gateway at quay.holos.internal.  Applies after
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

				// holos-authenticator deploys the Holos Authenticator (ADR-23): the
				// controller-runtime manager running the Envoy ext_authz gRPC server
				// plus its Backend CRD, RBAC, Service, an AuthorizationPolicy
				// (action: CUSTOM, provider.name holos-authenticator) referencing the
				// Istio extensionProvider declared in components/istio/istio.cue, and
				// one example Backend CR.  Registered after quay: the manager
				// Deployment pulls its image from the in-cluster Quay registry
				// (quay.holos.internal/holos/holos-authenticator:dev), so Quay must be
				// up before the holos-authenticator rollout the apply step waits on can
				// pull and start (the same image-from-Quay dependency that keeps the
				// holos-controller out of the bootstrap apply).  Its ext_authz provider
				// is part of istiod's MeshConfig and its namespace is ambient-enrolled,
				// both established far earlier in the istio data-plane phase.  The
				// component bundles its Backend CRD with the controller so the
				// authenticator.holos.run types ship with the platform (the
				// cert-manager-crds / cnpg-crds / kargo-crds pattern, here co-located
				// because the example Backend CR ships in the same component).
				(#ComponentTemplate & {inputs: {
					component: "holos-authenticator"
					cluster:   CLUSTER.name
					labels: app: "holos-authenticator"
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
			// shared Gateway at argocd.holos.internal.  Render-only in this
			// phase (HOL-1186): integration into scripts/apply with health
			// gates lands in the next phase (HOL-1187).
			(#ComponentTemplate & {inputs: {
				name:      "argocd"
				component: "controller"
				prefix:    "components/argocd"
				cluster:   CLUSTER.name
				labels: app: "argocd"
			}}).output

			// argocd-projects establishes the Argo CD project separation
			// between the platform "system" and the tenant "projects" and
			// registers the holos-paas-config OCI repository with Argo CD
			// (HOL-1375, the App-of-Apps bootstrap parent HOL-1373).  It emits
			// two AppProjects — platform (owns all system components, may
			// create cluster-scoped resources) and projects (owns the
			// top-level tenant App-of-Apps, namespaced-only) — plus the
			// create-if-absent bootstrap Job that assembles the repository
			// credential Secret at runtime.  Registered after the argocd
			// controller and before kargo: the AppProject kind is an
			// argoproj.io custom resource, so the Argo CD CRDs (argocd-crds)
			// and controller (argocd) must be established first.  It introduces
			// NO Applications, so it is a safe, additive change consumed by
			// later phases (HOL-1376/HOL-1377).
			(#ComponentTemplate & {inputs: {
				component: "argocd-projects"
				cluster:   CLUSTER.name
				labels: app: "argocd"
			}}).output

			// The NATS event-driven deployment pipeline (the nats backbone,
			// webhook-receiver, and webhook-subscriber components) was retired
			// in HOL-1241: Kargo plus the client-side ORAS publish workflow
			// (ADR-16) now own deployment, superseding the deprecated
			// receiver/subscriber/deployer path (ADR-9/10/11/14).

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
			// kargo.holos.internal through the shared Gateway.  Registered
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

			// app-of-apps bootstraps the whole platform through Argo CD
			// (HOL-1376, parent HOL-1373): a root Argo CD Application (the
			// App-of-Apps) assigned to the platform AppProject (argocd-projects,
			// HOL-1375) that fans out one child Application per SYSTEM component,
			// every child pulling the holos-paas-config OCI bundle at the mutable
			// :dev tag (HOL-1374) and carrying an ascending
			// argocd.argoproj.io/sync-wave mirroring the scripts/apply dependency
			// order.  It is the LAST system component and caps the "system" set —
			// every component registered ABOVE it (down to namespaces) is a child
			// of this App-of-Apps; the project/application collection components
			// BELOW it are tenant scaffolding applied separately
			// (scripts/apply-projects), not by this bootstrap.  Registered after
			// argocd-projects (which it depends on for the platform AppProject and
			// the repo credential) and after kargo-echo so it trails the full
			// system set it enumerates; the root Application is itself applied last
			// by scripts/apply during bring-up, once Argo CD and the AppProjects
			// exist, then it owns continuous reconciliation of the children.
			(#ComponentTemplate & {inputs: {
				component: "app-of-apps"
				cluster:   CLUSTER.name
				labels: app: "argocd"
			}}).output

			// project is the collection-driven generalization of the (now-deleted)
			// bespoke my-project scaffold (HOL-1355): it iterates the projects
			// collection (holos/collections.cue) and emits the full per-project
			// control-plane resource set into components/project/<name>/ for EVERY
			// projects.<name>.  As of HOL-1357 the bespoke holos/components/my-project
			// component was removed and my-project is produced wholly by this
			// generalized component (plus the application component for its app), so
			// the reference instance renders under components/project/my-project/.
			// Registered after argocd and kargo for the upstream-CRD/controller
			// reasons: the AppProject/Application are argoproj.io kinds, the Kargo
			// Project/Stage are kargo.akuity.io kinds, and the Organization/Keycloak
			// CRs are reconciled by the Holos Controller.  It is render-here /
			// apply-separately — its Organization carries a per-cluster caBundle
			// injected at apply time (scripts/apply-projects), so it is EXCLUDED from
			// the master scripts/apply.
			(#ComponentTemplate & {inputs: {
				component: "project"
				cluster:   CLUSTER.name
				labels: app: "project"
			}}).output

			// application is the collection-driven Application component (HOL-1356):
			// it iterates the apps collection (holos/collections.cue) and emits, for
			// every apps.<name> entry, the full application-level resource set
			// (Deployment/Service/HTTPRoute, the app KeycloakClient defining the
			// owner/editor/viewer roles the project role groups confer, the Quay
			// Repository, and the Kargo Warehouse/Stage + Argo CD Application) into
			// components/application/<name>/.  Registered after project for the
			// containment ordering: an app's KeycloakClient is referenced by the
			// project's role groups (clientRef), its Repository's organizationRef
			// names the project Organization, and its Argo CD Application runs in the
			// project AppProject — all emitted by the project component.  Like
			// project it is render-here / apply-separately (its Repository carries a
			// per-cluster caBundle injected at apply time, scripts/apply-projects),
			// so it is EXCLUDED from the master scripts/apply.  As of HOL-1357 the
			// reference app my-app (holos/apps/my-app.cue, project my-project) is
			// registered, so it renders under components/application/my-app/.
			(#ComponentTemplate & {inputs: {
				component: "application"
				cluster:   CLUSTER.name
				labels: app: "application"
			}}).output

			// holos-quay-organization emits the platform's OWN Quay org — the
			// `holos` Organization and the public `holos-controller` Repository the
			// Holos Controller reconciles (HOL-1380).  It is the bootstrap home of
			// the controller image and the App-of-Apps config bundle (in the
			// holos-paas-config repo).  Like project/application/keycloak-instance it
			// is render-here / apply-separately — its Organization and Repository
			// carry a per-cluster caBundle injected at apply time
			// (scripts/apply-holos-quay-organization), so it is EXCLUDED from the
			// master scripts/apply COMPONENTS.  Not a tenant project (no Kargo/Argo
			// CD/Keycloak control plane), so it is registered here next to the
			// collection components rather than in the system App-of-Apps set above.
			(#ComponentTemplate & {inputs: {
				component: "holos-quay-organization"
				cluster:   CLUSTER.name
				labels: app: "quay"
			}}).output

			// project-app-of-apps is the PER-PROJECT, control-plane/workload-split
			// tenant App-of-Apps (HOL-1382) that SUPERSEDES the single global
			// `projects` root (HOL-1377): it iterates the projects collection and
			// emits, FOR EACH project, a PAIR of root Argo CD Applications assigned to
			// the projects AppProject (argocd-projects) — <project>-control-plane
			// (the platform-applied control plane: AppProject, Applications,
			// Kargo/Quay/Keycloak CRs) and <project>-workload (the service-owner-
			// applied app workload) — each pulling that project's OWN OCI config
			// bundle (oci://quay.holos.internal/holos/<project>-config:dev) rather
			// than the single shared holos-paas-config bundle.  This is the "clean
			// cut line" HOL-1382 asks for: the platform App-of-Apps (app-of-apps)
			// bootstraps the system, and each project is bootstrapped separately by
			// its own per-project roots, built/pushed/applied by
			// scripts/apply-project-app-of-apps (control plane) and
			// scripts/apply-project-workload-app-of-apps (workload).  Registered LAST,
			// after the project and application collection components it delivers, and
			// after argocd-projects (the projects AppProject it is assigned to,
			// widened in HOL-1382 to permit the oci://quay.holos.internal/holos/*
			// per-project bundle sourceRepos).  Like app-of-apps it pins
			// targetRevision: dev on its OWN root Applications (no Kargo in the
			// bootstrap path) while delivering the per-project/app Applications
			// verbatim (their targetRevision stays Kargo-owned).  The root manifests
			// are applied imperatively by the per-project scripts and are NOT part of
			// any per-project bundle, so a root never reconciles itself.
			(#ComponentTemplate & {inputs: {
				component: "project-app-of-apps"
				cluster:   CLUSTER.name
				labels: app: "argocd"
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

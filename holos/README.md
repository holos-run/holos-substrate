# holos/ — Deployment Configuration and Policy

The Holos CUE configuration that renders this platform's Kubernetes
manifests using the [Holos](https://holos.run/) rendered-manifests pattern.
This directory is isolated from the Go code per
[ADR-12](../docs/adr/ADR-12.md).

To add or change a component, read
[docs/component-guidelines.md](docs/component-guidelines.md). Components
whose namespaces carry workloads must follow the ambient mesh enrollment
convention in [docs/mesh-enrollment.md](docs/mesh-enrollment.md).
Out-of-scope concerns with a planned home are stubbed in
[docs/placeholders.md](docs/placeholders.md).

## Directory layout

```text
holos/
├── cue.mod/         # CUE module: schemas vendored from holos and k8s APIs
├── platform/        # the Platform spec: registered clusters and components
├── components/      # one directory per component (BuildPlan definitions)
├── deploy/          # rendered manifests, committed: clusters/<cluster>/components/<name>/
└── docs/            # operational guidelines for this directory
```

- **`platform/platform.cue`** registers clusters and components. Every
  cluster in the `clusters` struct gets every registered component,
  parameterized by the `clusterName` tag.
- **`components/<name>/`** holds each component's `buildplan.cue` and
  boilerplate. See the
  [component guidelines](docs/component-guidelines.md#component-directory-anatomy)
  for the anatomy.
- **`deploy/`** is generated output — never edit it by hand. Render with
  `holos render platform` from this directory and commit the result; the
  tree must be diff-clean on re-render. `scripts/render` (from the repo
  root) checks exactly that: it removes `deploy/`, re-renders, and fails
  if anything under `holos/` is modified, deleted, or untracked — catching
  stale edits and orphaned manifests alike.

## Clusters: local development now, production later

The only registered cluster is **`k3d-holos`**, the local development
cluster — [docs/local-cluster.md](../docs/local-cluster.md) is the
quick-start guide for creating it and applying the platform to it. The MVP
demo target is a single Apple Silicon Mac ([ADR-7](../docs/adr/ADR-7.md)).

A production deployment area is planned but not yet established: production
clusters will be registered alongside `k3d-holos` in
`platform/platform.cue`, and each registered cluster renders its own
`deploy/clusters/<cluster>/` tree. See
[docs/placeholders.md](docs/placeholders.md#production-deployment-area).

## How rendered manifests reach the cluster

During bootstrap there is no gitops controller in the cluster yet, so
rendered manifests are applied directly with server-side apply.
`scripts/apply` (from the repo root) applies every platform component — the
Layer 0 foundation and the Layer 1 services — to the
current kubectl context in the correct order:

```bash
scripts/apply
```

This section is the canonical explanation of *why* the apply order is what
it is and the caveats that come with force-applying. For the step-by-step path
from nothing to a running platform — DNS setup, cluster creation, trusted
TLS, then this apply step — follow the quick-start guide,
[docs/local-cluster.md](../docs/local-cluster.md).

The script is idempotent: server-side apply and `kubectl wait` both
converge, so re-running it against a fresh, partially applied, or fully
applied cluster is safe. As a guard against force-applying to the wrong
cluster, it refuses to run when the current context is not `k3d-holos`
unless `KUBE_CONTEXT` is set explicitly, and pins every kubectl call to
the resolved context. Per component it runs

```bash
kubectl apply --server-side --force-conflicts -f holos/deploy/clusters/k3d-holos/components/<name>/
```

and waits only on the critical dependencies between components — CRD
establishment, the istiod rollout, the ambient data-plane DaemonSets, the
cert-manager webhook rollout, the CNPG operator rollout, the Postgres
`Cluster` Ready conditions, and the Keycloak operator rollout — plus waits
on the `echo` Deployment, the `Keycloak` CR Ready and realm import Done
conditions, the `quay` Deployment rollout, and the Argo CD workload
rollouts as smoke checks; nothing else.

Apply order matters beyond "CRD components first". The script applies the
platform components in this order — everything through `echo` is the
Layer 0 cluster foundation; everything from `cnpg-crds` on is a Layer 1
platform service:

1. `namespaces` — every platform Namespace, from the central registry
   ([namespaces.cue](namespaces.cue)); labeled `namespaces: "true"` so apply
   tooling can select it
2. `gateway-api` — Gateway API standard channel CRDs (`crds: "true"`)
3. `cert-manager-crds` — cert-manager CRDs (`crds: "true"`)
4. `istio-base` — Istio CRDs and validation webhook (`crds: "true"`)
5. `istiod` — the Istio control plane
6. `istio-cni` — the node agent that redirects ambient pod traffic to ztunnel
7. `istio-ztunnel` — the ambient node proxy
8. `cert-manager` — the certificate controller, webhook, and cainjector
9. `local-ca` — the CA `ClusterIssuer` that signs all platform certificates
10. `istio-gateway` — the shared Gateway all platform services attach
    `HTTPRoute`s to, and its wildcard TLS certificate
11. `echo` — the permanent smoke-test workload and its `HTTPRoute`
12. `cnpg-crds` — CloudNativePG CRDs (`crds: "true"`), filtered out of the
    single upstream release manifest
13. `cnpg` — the CloudNativePG operator, the platform's single Postgres
    operator
14. `cnpg-clusters` — the per-service Postgres `Cluster` resources
    (`keycloak-db`, `quay-db`), each in its consuming service's namespace
15. `keycloak-operator-crds` — Keycloak operator CRDs (`crds: "true"`),
    fetched as the two separate upstream single-CRD manifests
16. `keycloak-operator` — the Keycloak operator, in the `keycloak`
    namespace (deliberately not ambient-enrolled, see
    [namespaces.cue](namespaces.cue))
17. `keycloak` — the Keycloak server instance: the `Keycloak` CR backed by
    the `keycloak-db` Postgres `Cluster`, its TLS `Certificate`, the
    declarative `holos` realm import, the `HTTPRoute` attaching it to the
    shared Gateway at `auth.holos.localhost`, and the `DestinationRule`
    that re-encrypts the Gateway→Keycloak hop
18. `quay` — the Quay registry: the Quay `Deployment` backed by the
    `quay-db` Postgres `Cluster` and a minimal `quay-redis` Deployment,
    with blob storage on a local-path PVC and the `HTTPRoute` pair
    attaching it to the shared Gateway at `quay.holos.localhost`
19. `argocd-crds` — the Argo CD CRDs (`crds: "true"`): `applications`,
    `applicationsets`, and `appprojects` in group `argoproj.io`
20. `argocd` — the Argo CD core install: the application-controller
    `StatefulSet`, the repo-server, server, and redis `Deployment`s, and
    the `HTTPRoute` pair attaching the UI to the shared Gateway at
    `argocd.holos.localhost`
21. `nats` — the NATS JetStream backbone: the single-replica `nats`
    `StatefulSet` with file-backed JetStream, plus the
    `nats-stream-bootstrap` `Job` that idempotently creates the `WEBHOOKS`
    (`webhooks.>`) and `TASKS` (`tasks.>`) file-backed WorkQueue streams

The order encodes six rules: the `namespaces` component applies first, so
every Namespace exists before any component that populates it;
CRD components (labeled `crds: "true"`) apply before the controllers that
depend on their types; `istiod` applies before
the Gateway, because the `istio` GatewayClass must exist and istiod must be
running to program the Gateway; `istio-cni` and `istio-ztunnel` apply before
ambient-enrolled workloads like `cert-manager`, `echo`, and `cnpg`, because
they must be capturing traffic when those workloads start (the Gateway
itself is deliberately not enrolled, see
[docs/mesh-enrollment.md](docs/mesh-enrollment.md)); components with
fail-closed admission webhooks apply — and their Deployments are waited on —
before the components that create the resources they admit: `cert-manager`
before the `cert-manager.io` resources (`local-ca`'s `ClusterIssuer`,
`istio-gateway`'s `Certificate`), with a retry on the transient x509
admission error while cainjector injects the webhook's CA bundle, and the
`cnpg` operator before the `postgresql.cnpg.io` `Cluster` resources the
`cnpg-clusters` component creates — with the same shape of retry, because
the operator's webhook may briefly reject admission after its rollout is
Available; and the Gateway applies before components that attach
routes to it. `cnpg-crds` and `cnpg` trail `echo` because CNPG depends only
on its own CRDs (and, being ambient-enrolled, the data plane), so appending
them keeps the established order stable. `cnpg-clusters` trails `cnpg` and
is gated on each `Cluster`'s `Ready` condition because the Keycloak phase
applies a Keycloak CR that needs a reachable database.
`keycloak-operator-crds` and `keycloak-operator` trail `cnpg-clusters`: the
Keycloak operator depends only on its own CRDs and the `keycloak`
namespace, and the `keycloak` component applies `Keycloak` and
`KeycloakRealmImport` CRs that need both the operator reconciling — hence
the gate on its Deployment rollout — and the `keycloak-db` `Cluster`
reachable, so appending the pair after the database keeps the dependency
chain linear. `keycloak` trails the operator: its CRs need everything
above, and it creates a `cert-manager.io` `Certificate`, so its apply
retries through the same transient webhook admission window as `local-ca`
and `istio-gateway`. Its gate waits on the `Keycloak` CR Ready condition
and then on the `holos` `KeycloakRealmImport` Done condition as the Layer 1
smoke check, so a bootstrap cannot report success while the realm import
Job is still running or has failed — the first start pulls the server
image and runs the database schema migrations, so each wait gets a more
generous timeout (`KEYCLOAK_TIMEOUT`, default 600s) than the rollout
gates. `quay` trails `keycloak` because it needs the `quay-db` `Cluster`
reachable — already gated Ready in the `cnpg-clusters` step — and its gate
waits on the secret-keys bootstrap Job and then on the `quay` Deployment
rollout with its own generous timeout (`QUAY_TIMEOUT`, default 900s),
since the first pull of the Quay image is large and the first start runs
Quay's database schema migrations. `argocd-crds` and `argocd` continue the
sequence: the CRDs apply (and are gated Established) before the
controllers that need the types, and Argo CD depends only on the Gateway
its `HTTPRoute`s attach to — nothing downstream depends on it during
bootstrap — so appending the pair keeps the established order stable. The
`argocd` gate waits on the rollout of exactly the workloads the chart
renders with pods — the redis, repo-server, and server `Deployment`s and
the application-controller `StatefulSet` — as the Argo CD smoke check
(the applicationset-controller `Deployment` renders with `replicas: 0`,
and dex and notifications are disabled and render no workloads). `nats`
closes the sequence: it is a Layer 2 backbone service that nothing during
bootstrap depends on (its producers and consumers are separate components
that land later), so appending it keeps the established order stable — the
same rationale as `argocd`. Its gate first polls the `nats-stream-bootstrap`
Job to completion — the `wait_quay` Job-poll pattern, so a failure names the
Job rather than a generic rollout timeout — and then waits the `nats`
`StatefulSet` rollout as the NATS backbone smoke check.

The first rule exists because nothing orders an apply batch by kind:
kubectl submits the files sequentially in lexical order, so a single
server-side apply that carries a Namespace alongside its namespaced
resources fails with `NotFound` on the first apply whenever a namespaced
resource sorts ahead of its Namespace. The last rule is for verifiability
rather than correctness — route attachment is level-triggered, so an
`HTTPRoute` applied early simply reports unattached until the Gateway
exists — but applying `echo` after the Gateway means the smoke test
exercises a complete traffic path immediately. Certificate issuance is
level-triggered the same way: the Gateway's HTTPS listener reports an
unresolved certificate ref only until cert-manager writes the wildcard
certificate's Secret.

`--force-conflicts` is safe here because the rendered manifests in git are
the source of truth for these resources and, with the exceptions below, no
other controller manages their fields during bootstrap; do not copy it into
contexts where another field manager owns the resources.

cert-manager's cainjector manages `webhooks[].clientConfig.caBundle` on the
rendered cert-manager webhook configurations at runtime. Unlike the Istio
exception below, the field is absent from the rendered manifests, so a
re-apply with `--force-conflicts` never claims or strips it — no enforcement
gap results. The CNPG operator manages the `caBundle` on its own webhook
configurations the same way, and the field is likewise absent from the
rendered `cnpg` manifests.

The other exception is Istio's webhook reconciliation: the rendered
`ValidatingWebhookConfiguration`s (`istiod-default-validator` in
`istio-base`, `istio-validator-istio-system` in `istiod`) set
`failurePolicy: Ignore`, and istiod patches the field to `Fail` once it is
ready to serve admission requests. Re-applying either component with
`--force-conflicts` seizes the field back and downgrades it to `Ignore`
until istiod re-patches it — expect that transient enforcement gap (and the
resulting field-manager churn) on every re-apply of those two components,
including every re-run of `scripts/apply`.

Argo CD itself is now installed by `scripts/apply` (the `argocd-crds` and
`argocd` components), but ArgoCD-based delivery has not yet replaced the
direct apply: every component still renders with `argoAppDisabled: true`
and no `Application` resources are emitted, so Argo CD reconciles nothing
until the gitops Application projection is enabled. See
[docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery). The
`Application` source pattern that delivery will use — OCI artifacts in
the in-cluster Quay registry — is decided, verified, and documented in
[docs/argocd-application-source.md](docs/argocd-application-source.md).

### Keycloak admin credentials and verification

The Keycloak operator bootstraps the initial admin user itself and stores
the generated credentials in the `keycloak-initial-admin` Secret (keys
`username` and `password`) on first reconcile — no credentials are
committed to this repository. Retrieve them:

```bash
kubectl -n keycloak get secret keycloak-initial-admin -o json \
  | jq '.data | map_values(@base64d)'
```

Verify Keycloak on the live cluster after `scripts/apply`:

```bash
kubectl -n keycloak wait keycloak/keycloak --for=condition=Ready --timeout=600s
curl -fsSI https://auth.holos.localhost/        # trusted chain via the mkcert root
curl -fs https://auth.holos.localhost/realms/holos/.well-known/openid-configuration | jq .issuer
# log in to https://auth.holos.localhost/admin/ with the credentials above
```

State lives in the `keycloak-db` Postgres `Cluster`, not the pod: deleting
the Keycloak pod (`kubectl -n keycloak delete pod -l
app.kubernetes.io/managed-by=keycloak-operator`) loses nothing — after the
operator restarts it, the `holos` realm and admin login still work. Note
the realm import is bootstrap-only: the operator's import Job skips when
the realm already exists, so post-bootstrap realm changes are not
reconciled from the `KeycloakRealmImport` CR — see the caveat in
[components/keycloak/instance/buildplan.cue](components/keycloak/instance/buildplan.cue)
and the stub in
[docs/placeholders.md](docs/placeholders.md#keycloak-realm-reconciliation).

### Quay bootstrap and credentials

Quay has no operator to bootstrap users the way the Keycloak operator
does, so `scripts/quay-init` fills that role: run it once after
`scripts/apply` to create the initial `admin` user, the `holos`
organization, and the `holos+robot` robot account, with the generated
credentials stored in the `quay-initial-admin` and `quay-robot-pull`
Secrets (`quay` namespace) — never committed to this repository. The
script is idempotent. See the
[Verify Quay](../docs/local-cluster.md#verify-quay) section of the local
cluster guide for the bootstrap and the `docker push` verification flow.

### Quay verification

Two checks prove the registry behaviors the platform depends on
([ADR-13](../docs/adr/ADR-13.md) builds on the push webhook); re-run them
after any Quay change. Both assume the bootstrap above has run: the
registry is initialized and `holos/sample` exists from the
[Verify Quay](../docs/local-cluster.md#verify-quay) push.

**Push webhook.** A `repo_push` webhook notification fires on image push.
Verify it against a temporary in-cluster echo endpoint (the
`mendhak/http-https-echo` image is multi-arch and logs every request body
to stdout):

```bash
kubectl -n quay run quay-echo --image=mendhak/http-https-echo:37 --port=8080 \
  --labels=app.kubernetes.io/name=quay-echo
kubectl -n quay expose pod quay-echo --port=8080
kubectl -n quay wait pod/quay-echo --for=condition=Ready --timeout=120s

# The Quay API takes the admin OAuth token (basic auth is not accepted).
TOKEN=$(kubectl -n quay get secret quay-initial-admin -o jsonpath='{.data.token}' | base64 -d)
UUID=$(curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X POST https://quay.holos.localhost/api/v1/repository/holos/sample/notification/ \
  -d '{"event": "repo_push", "method": "webhook",
       "config": {"url": "http://quay-echo.quay.svc:8080/"},
       "eventConfig": {}, "title": "verify-webhook"}' | jq -er '.uuid // empty')
# Fire the built-in test first. The ${UUID:?} expansion aborts this command —
# even when the block is pasted into an interactive shell — if the create
# above failed, instead of POSTing to .../notification//test:
curl -fsS -o /dev/null -H "Authorization: Bearer $TOKEN" -X POST \
  "https://quay.holos.localhost/api/v1/repository/holos/sample/notification/${UUID:?notification create failed}/test"

# Then a real push (docker login per docs/local-cluster.md "Verify Quay"):
docker pull busybox && docker tag busybox quay.holos.localhost/holos/sample:test2
docker push quay.holos.localhost/holos/sample:test2

kubectl -n quay logs quay-echo
```

The echo logs must show one POST per event whose JSON body carries
`repository`, `namespace`, `name`, `docker_url`, and `updated_tags` — the
fields ADR-13's webhook receiver parses. Deliveries to cluster-internal
plain-HTTP URLs work out of the box; no allowlist configuration is
required. Failures can be silent (Quay queues deliveries through Redis),
so on trouble check the notification's failure counter
(`GET .../notification/` → `number_of_failures`) and the Quay pod logs.
Clean up when done:

```bash
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -X DELETE "https://quay.holos.localhost/api/v1/repository/holos/sample/notification/${UUID:?}"
kubectl -n quay delete pod/quay-echo svc/quay-echo
```

**Restart resilience.** Registry state lives in the `quay-db` Postgres
`Cluster` (metadata, including notification configs) and the
`quay-datastorage` PVC (blobs) — not the pods. Delete both pods and
confirm nothing is lost:

```bash
kubectl -n quay delete pod -l app.kubernetes.io/name=quay
kubectl -n quay delete pod -l cnpg.io/cluster=quay-db
kubectl -n quay rollout status deployment/quay --timeout=600s
kubectl -n quay wait cluster/quay-db --for=condition=Ready --timeout=300s
```

After recovery: `docker login` with the robot credentials still works, the
previously pushed tag is still pullable
(`docker rmi quay.holos.localhost/holos/sample:test` then
`docker pull quay.holos.localhost/holos/sample:test`), and any webhook
notification configured above is still listed via the API.

Sizing note: Quay's gunicorn pools enforce per-pool minimums that override
the `WORKER_COUNT_*` pins unless `WORKER_COUNT_UNSUPPORTED_MINIMUM` is
also set — without it the registry pool runs 8 workers and the container
OOMKills against its memory limit. See the env comment in
[components/quay/buildplan.cue](components/quay/buildplan.cue).

### Argo CD admin credentials and verification

The Argo CD UI is served at `https://argocd.holos.localhost` through the
shared Gateway, which terminates TLS with the wildcard certificate — the
server itself runs with `server.insecure: "true"` and a plain-HTTP backend,
like the other routed services. The server bootstraps the initial `admin`
user itself on first startup and stores the generated password in the
`argocd-initial-admin-secret` Secret (`argocd` namespace, key `password`)
— no credentials are committed to this repository, mirroring the Keycloak
`keycloak-initial-admin` pattern. The Secret appears only after the first
server start, so never gate on it ahead of the rollout. Retrieve the
password:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

Verify Argo CD on the live cluster after `scripts/apply` — wait on exactly
the workloads the chart renders with pods (the applicationset-controller
Deployment renders with `replicas: 0`, and dex and notifications are
disabled and render no workloads):

```bash
kubectl -n argocd wait deployment argocd-redis argocd-repo-server argocd-server \
  --for=condition=Available --timeout=300s
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
curl -fsSI https://argocd.holos.localhost/   # trusted chain via the mkcert root
# log in to https://argocd.holos.localhost/ as admin with the password above
```

The `argocd` namespace is ambient-enrolled (`_ambient: true` in
[namespaces.cue](namespaces.cue), following the reference platform);
enrolled pods report protocol `HBONE` in
`istioctl ztunnel-config workloads` — see
[docs/mesh-enrollment.md](docs/mesh-enrollment.md). Argo CD reconciles
nothing yet: no `Application` resources are emitted until the gitops
Application projection is enabled (see
[docs/placeholders.md](docs/placeholders.md#argocd-gitops-delivery)).

### Verify an OCI-source Application

The MVP delivery path syncs `Application` resources from rendered-manifests
OCI artifacts in the in-cluster Quay registry —
[docs/argocd-application-source.md](docs/argocd-application-source.md) is
the pattern's contract (artifact layout, credential Secret shape, how the
repo-server reaches Quay, tag-vs-digest guidance). The procedure below
proves the path end to end with a throwaway artifact and Application;
re-run it after any change to the argocd or quay components, or to the
`quay-holos-localhost` ServiceEntry. It assumes the
[Quay bootstrap](#quay-bootstrap-and-credentials) has run and the
[`oras`](https://oras.land/) CLI is installed. Nothing here is committed:
the artifact is pushed imperatively, so a committed Application would
leave a fresh bootstrap perpetually Degraded (see the
[pattern doc](docs/argocd-application-source.md#what-stays-imperative)).

Package a trivial manifest as the single-layer artifact Argo CD expects
and push it with the robot credentials:

```bash
WORK=$(mktemp -d)
mkdir -p "${WORK}/manifests"
cat > "${WORK}/manifests/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-smoke
  namespace: echo
data:
  purpose: OCI-source smoke test
YAML
tar -czf "${WORK}/manifests.tar.gz" -C "${WORK}/manifests" .
ROBOT_TOKEN=$(kubectl -n quay get secret quay-robot-pull -o jsonpath='{.data.\.dockerconfigjson}' \
  | base64 -d | jq -r '.auths["quay.holos.localhost"].auth' | base64 -d | cut -d: -f2-)
(cd "${WORK}" && oras push --username 'holos+robot' --password-stdin \
  quay.holos.localhost/holos/argocd-smoke:v1 \
  manifests.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip <<<"${ROBOT_TOKEN:?}")
```

Register the repository with Argo CD and create the test Application. The
`${ROBOT_TOKEN:?}` expansion aborts the paste if the extraction above
failed; `insecure: "true"` is required because the local mkcert CA is not
in the repo-server's trust store (see the
[pattern doc](docs/argocd-application-source.md#repository-credential-secret)):

```bash
kubectl apply --server-side -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: quay-argocd-smoke
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: argocd-smoke
  url: oci://quay.holos.localhost/holos/argocd-smoke
  type: oci
  username: holos+robot
  password: "${ROBOT_TOKEN:?}"
  insecure: "true"
EOF
kubectl apply --server-side -f - <<'EOF'
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: argocd-smoke
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  source:
    repoURL: oci://quay.holos.localhost/holos/argocd-smoke
    targetRevision: v1
    path: .
  destination:
    server: https://kubernetes.default.svc
    namespace: echo
  syncPolicy:
    automated:
      prune: true
EOF
```

Wait for the sync and confirm the manifest landed. `Application`s are
ordinary namespaced objects — the plain `kubectl get` is the same access
path the future deployment subscriber uses to patch `targetRevision`:

```bash
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath='{.status.sync.status}'=Synced --timeout=120s
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath='{.status.health.status}'=Healthy --timeout=120s
kubectl get applications.argoproj.io -n argocd
kubectl -n echo get configmap argocd-smoke
```

Exercise the rollout path the deployment subscriber will use — push a
changed artifact, resolve its immutable digest, and patch
`targetRevision` (prefer digests over tags for controller-driven updates;
see the
[pattern doc](docs/argocd-application-source.md#tag-vs-digest-in-targetrevision)):

```bash
cat > "${WORK}/manifests/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-smoke
  namespace: echo
data:
  purpose: OCI-source smoke test
  version: v2
YAML
tar -czf "${WORK}/manifests.tar.gz" -C "${WORK}/manifests" .
(cd "${WORK}" && oras push --username 'holos+robot' --password-stdin \
  quay.holos.localhost/holos/argocd-smoke:v2 \
  manifests.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip <<<"${ROBOT_TOKEN:?}")
DIGEST=$(oras resolve --username 'holos+robot' --password-stdin \
  quay.holos.localhost/holos/argocd-smoke:v2 <<<"${ROBOT_TOKEN:?}")
kubectl -n argocd patch application argocd-smoke --type merge \
  -p "{\"spec\":{\"source\":{\"targetRevision\":\"${DIGEST:?}\"}}}"
# Two waits: the revision wait alone races the apply — sync.revision
# updates when the controller *compares* against the new digest, before
# the automated sync has written resources, so gate on Synced too.
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath="{.status.sync.revision}"="${DIGEST:?}" --timeout=120s
kubectl -n argocd wait application/argocd-smoke \
  --for=jsonpath='{.status.sync.status}'=Synced --timeout=120s
kubectl -n echo get configmap argocd-smoke -o jsonpath='{.data.version}'  # v2
```

Clean up — the finalizer cascades the delete, so Argo CD prunes the
synced ConfigMap before the Application disappears. The
`holos/argocd-smoke` repository stays in the registry (the robot
credential cannot delete repositories), like `holos/sample` from the
[Quay verification](#quay-verification); a re-run converges on it:

```bash
kubectl -n argocd delete application argocd-smoke --timeout=120s
kubectl -n echo wait --for=delete configmap/argocd-smoke --timeout=30s   # prune confirmed
kubectl -n argocd delete secret quay-argocd-smoke
rm -rf "${WORK:?}"
```

### Postgres credentials and connection contract

The `cnpg-clusters` component provisions one Postgres `Cluster` per
consuming service, in that service's namespace. CNPG generates the
credentials and connection endpoints with conventional names — this is the
contract the Keycloak and Quay components consume:

| Cluster | Namespace | Credentials Secret | Read-write Service |
|---------|-----------|--------------------|--------------------|
| `keycloak-db` | `keycloak` | `keycloak-db-app` | `keycloak-db-rw.keycloak.svc:5432` |
| `quay-db` | `quay` | `quay-db-app` | `quay-db-rw.quay.svc:5432` |

Each `<cluster>-app` Secret carries the keys `username`, `password`,
`dbname`, `host`, `port`, `uri`, and `jdbc-uri`.

Verify the databases on the live cluster after `scripts/apply`:

```bash
kubectl get cluster -A                       # both: Cluster in healthy state
kubectl -n keycloak get secret keycloak-db-app
kubectl -n quay get secret quay-db-app
KC_POD=$(kubectl -n keycloak get pod \
  -l cnpg.io/cluster=keycloak-db,cnpg.io/instanceRole=primary -o name)
QUAY_POD=$(kubectl -n quay get pod \
  -l cnpg.io/cluster=quay-db,cnpg.io/instanceRole=primary -o name)
kubectl -n keycloak exec "${KC_POD:?no keycloak-db primary pod}" -- \
  psql -U postgres -c 'SELECT 1'
kubectl -n quay exec "${QUAY_POD:?no quay-db primary pod}" -- \
  psql -U postgres -c 'SELECT 1'
```

To exercise the same path the consuming service uses — the `-rw` Service
with the `-app` credentials — run a short-lived client pod with the `uri`
key from the Secret:

```bash
URI=$(kubectl -n keycloak get secret keycloak-db-app -o jsonpath='{.data.uri}' | base64 -d)
kubectl -n keycloak run psql-verify --rm -i --restart=Never \
  --image=ghcr.io/cloudnative-pg/postgresql:18.1 --env="URI=$URI" -- \
  psql "$URI" -c 'SELECT current_user, current_database()'
```

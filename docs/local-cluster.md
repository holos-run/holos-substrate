# Local Cluster

<!-- Originally vendored from https://github.com/holos-run/holos/blob/main/doc/md/topics/local-cluster.mdx; has since diverged — do not overwrite with a re-vendor. -->

Set up a local k3d cluster for development and testing, then apply the
platform to it. This is the canonical quick-start guide: after completing
it you'll have a running platform — the Layer 0 foundation (a Kubernetes
API server with proper DNS and TLS certificates, serving the platform's
components on ports 80 and 443) plus the Layer 1 services:
CloudNativePG-managed Postgres, Keycloak with the `holos` realm at
`https://auth.holos.localhost`, the Quay registry at
`https://quay.holos.localhost`, and Argo CD at
`https://argocd.holos.localhost`.

This is the foundation for the Holos PaaS MVP — see
[Holos PaaS MVP Milestones](planning/holos-paas-mvp-milestones.md)
for the full milestone plan.

> **Platform note:** `scripts/local-dns` is macOS-only. The MVP demo target is
> an Apple Silicon Mac (see [ADR-7](adr/ADR-7.md)). Linux users must configure
> dnsmasq or systemd-resolved themselves to resolve `*.holos.localhost` to
> `127.0.0.1`.

## Prerequisites

1. [OrbStack](https://docs.orbstack.dev/install) or [Docker](https://docs.docker.com/get-docker/) — container runtime (OrbStack recommended per ADR-7)
2. [k3d](https://k3d.io/#installation) — local Kubernetes via Docker
3. [kubectl](https://kubernetes.io/docs/tasks/tools/) — Kubernetes CLI
4. [mkcert](https://github.com/FiloSottile/mkcert) — trusted local TLS certificates
5. [jq](https://jqlang.org/) — JSON processing (used by cluster scripts)
6. [Homebrew](https://brew.sh/) — macOS only, required by `scripts/local-dns`

## One-Time DNS Setup

Configure your machine to resolve `*.holos.localhost` to your loopback
interface so requests reach the local cluster. Run this once before creating
the cluster:

```bash
scripts/local-dns
```

This installs dnsmasq via Homebrew, writes
`address=/holos.localhost/127.0.0.1` to the dnsmasq config, loads the
LaunchDaemon idempotently, and writes `/etc/resolver/holos.localhost`.
Requires `sudo` for system DNS configuration.

## Create the Cluster

Create a local k3d cluster with a container registry:

```bash
scripts/local-k3d
```

This creates:

- A local registry at `quay.holos.localhost/holos`
- A k3d cluster named `holos` with ports 80 and 443 forwarded to the load
  balancer and Traefik disabled

With Traefik disabled, nothing answers on ports 80/443 until the platform's
Layer 0 components are applied in the
[Apply the Platform](#apply-the-platform) step below. The shared Istio
Gateway (the `istio-gateway` component) then serves ports 80 and 443, and
platform services attach `HTTPRoute`s to it. The HTTPS listener terminates
TLS with a wildcard `*.holos.localhost` certificate issued by cert-manager's
`local-ca` ClusterIssuer from the mkcert root CA installed in the
[Setup Trusted TLS](#setup-trusted-tls) step below — the same root CA your
host trusts, so browsers accept the certificate without warnings. See
[mesh-enrollment.md](../holos/docs/mesh-enrollment.md) for how workload
namespaces enroll in the ambient mesh.

The static cluster shape — port mappings, k3s args — is defined in
[`k3d/config.yaml`](../k3d/config.yaml), which is the source of truth for
cluster structure. The cluster name, registry hostname, and registry port are
passed at runtime by `scripts/local-k3d` (positional name argument and
`--registry-use`), so `CLUSTER_NAME`, `REGISTRY_NAME`, and `REGISTRY_PORT`
are fully honored without editing `k3d/config.yaml`.

## Setup Trusted TLS

Install the mkcert root CA into the cluster so cert-manager can issue trusted
certificates for `*.holos.localhost`:

```bash
sudo -v
scripts/local-ca
```

**Run this each time you recreate the cluster.**

This installs the mkcert root CA into the host trust store (the one-liner
`mkcert --install`, run by the script), creates the `cert-manager` namespace,
and applies the `local-ca` Secret (`type: kubernetes.io/tls`) that
cert-manager's `local-ca` ClusterIssuer references.
The generated `namespace.yaml` and `local-ca.yaml` are saved to
`$(mkcert -CAROOT)` (mode 0600 for the key material) so you can reapply them
after a cluster reset without re-running `mkcert --install`.

## Apply the Platform

Every platform component runs an upstream image except the
`webhook-receiver`, which runs the locally built `holos-paas` image. Build
and push it to the in-cluster k3d registry before applying, or the
`webhook-receiver` rollout stalls at `ImagePullBackOff` (the gate detects this
and points back here):

```bash
make docker-push
```

Apply the rendered platform manifests to the cluster:

```bash
scripts/apply
```

The script applies every platform component in dependency order — the
Layer 0 foundation (namespaces, the Istio ambient mesh, cert-manager, the
shared Gateway) followed by the Layer 1 services (CloudNativePG Postgres,
the Keycloak operator and instance, the Quay registry, and Argo CD) —
starting with the `namespaces` component, so every namespace exists before
any namespaced resource applies. It is idempotent, so it is safe to re-run
at any time.
See
[How rendered manifests reach the cluster](../holos/README.md#how-rendered-manifests-reach-the-cluster)
for the apply-order rationale and the `--force-conflicts` and webhook
caveats. The late gates wait for the Keycloak CR to report Ready, the
`holos` realm import to complete, and the `quay` Deployment to roll out,
so the first run takes several minutes while images pull and Keycloak and
Quay run their database schema migrations (`KEYCLOAK_TIMEOUT`, default
600s; `QUAY_TIMEOUT`, default 900s).

When the script completes, the platform serves ports 80 and 443 and the
`echo` smoke-test workload answers at `https://echo.holos.localhost/`
with a browser-trusted certificate:

```bash
curl https://echo.holos.localhost/
```

## Verify Keycloak

The full bootstrap from zero — `scripts/local-dns` (one-time), then
`scripts/local-k3d`, `scripts/local-ca`, and `scripts/apply` — brings up
Keycloak with the `holos` realm and no manual steps. Verify it the same
way as the echo smoke test: the console answers at
`https://auth.holos.localhost/` with a browser-trusted certificate, and
the `holos` realm serves its OIDC discovery document:

```bash
curl -fsSI https://auth.holos.localhost/
curl -fs https://auth.holos.localhost/realms/holos/.well-known/openid-configuration | jq .issuer
```

The Keycloak operator generates the initial admin credentials on first
reconcile and stores them in the `keycloak-initial-admin` Secret — no
credentials are committed to this repository. Retrieve them and log in to
the admin console at `https://auth.holos.localhost/admin/`:

```bash
kubectl -n keycloak get secret keycloak-initial-admin -o json \
  | jq '.data | map_values(@base64d)'
```

Keycloak's state lives in the `keycloak-db` Postgres `Cluster`, not the
pod. The `KeycloakRealmImport` CR only bootstraps the realm shell — the
operator's import Job skips when the realm already exists — so the
platform's realm roles, the `authenticated` default group, and the OIDC
clients are reconciled on every `scripts/apply` by the `keycloak-config`
keycloak-config-cli `Job` instead (see
[`holos/components/keycloak/realm-config/buildplan.cue`](../holos/components/keycloak/realm-config/buildplan.cue)
and [keycloak-config: realm reconciliation](../holos/README.md#keycloak-config-realm-reconciliation)).
For the full verification steps — including the pod restart-survival
check — see
[Keycloak admin credentials and verification](../holos/README.md#keycloak-admin-credentials-and-verification);
for the database contract, see
[Postgres credentials and connection contract](../holos/README.md#postgres-credentials-and-connection-contract).

## Verify Quay

`scripts/apply` brings the Quay registry up at
`https://quay.holos.localhost`, but a fresh registry has no users.
Bootstrap it once:

```bash
scripts/quay-init
```

The script is idempotent — a second run exits 0 without changing state.
It creates the initial `admin` user, the `holos` organization, and the
`holos+robot` robot account (a member of a `creators` team, so pushes
auto-create repositories under `holos/`), and stores the generated
credentials in Secrets in the `quay` namespace — nothing secret is
committed to this repository, mirroring the Keycloak
`keycloak-initial-admin` pattern:

```bash
kubectl -n quay get secret quay-initial-admin -o json \
  | jq '.data | map_values(@base64d)'      # admin UI login + API token
kubectl -n quay get secret quay-robot-pull  # dockerconfigjson for pull consumers
```

Verify the registry with a push from the host. Log in as the robot —
the one-liner extracts the robot token from the pull Secret:

```bash
kubectl -n quay get secret quay-robot-pull -o jsonpath='{.data.\.dockerconfigjson}' \
  | base64 -d | jq -r '.auths["quay.holos.localhost"].auth' | base64 -d | cut -d: -f2- \
  | docker login quay.holos.localhost -u 'holos+robot' --password-stdin
```

Then push — the repository does not exist yet; the robot's `creators`
team membership auto-creates it:

```bash
docker pull busybox
docker tag busybox quay.holos.localhost/holos/sample:test
docker push quay.holos.localhost/holos/sample:test
```

Confirm the pushed tag in the Quay UI: log in at
`https://quay.holos.localhost` with the `quay-initial-admin` credentials
and find `holos/sample` under the `holos` organization (repositories
auto-created by push are private).

> **Docker trust note:** on the MVP target — OrbStack on Apple silicon
> ([ADR-7](adr/ADR-7.md)) — OrbStack syncs the macOS keychain trust store
> into its Docker daemon, so the mkcert root installed by
> `scripts/local-ca` is already trusted and `docker push` just works. With
> Docker Desktop instead place the CA at
> `~/.docker/certs.d/quay.holos.localhost/ca.crt`
> (`mkcert -CAROOT` prints the directory containing `rootCA.pem`).

**Verify Keycloak SSO login.** Beyond the local `admin` account, real users
sign in to Quay through the Keycloak `holos` realm with the Authorization Code
flow plus PKCE (the confidential `quay` client the `keycloak-config` Job
provisions). The design is in [ADR-15](adr/ADR-15.md); verify it end to end:

1. Create a realm user (or reuse the one from
   [Verify Argo CD](#verify-argo-cd)) in the Keycloak admin console at
   `https://auth.holos.localhost/admin/`. To exercise the roles model, assign
   the `quay` client role `platform-admin` or `project-admin` under
   **Users → (user) → Role mapping → Assign role → Filter by clients → `quay`**.
2. Open Quay and start SSO login:

   ```bash
   open https://quay.holos.localhost/
   ```

   Click **Sign in with Holos SSO** and authenticate as the realm user.
3. Confirm the SSO behavior:
   - **No username prompt.** Quay does not ask the user to choose or confirm a
     username (`FEATURE_USERNAME_CONFIRMATION: false`); login completes
     straight to the dashboard.
   - **Namespace matches the token.** The user's personal namespace equals
     their `preferred_username` claim — repositories live under
     `quay.holos.localhost/<preferred_username>/...`.
   - **Roles → teams.** A user granted a `quay` client role (or bound Keycloak
     group) gains the matching Quay team membership after the next team
     re-sync (`TEAM_RESYNC_STALE_TIME`, 30 minutes), once a Quay **superuser**
     has bound the team to that group/role name in the Quay organization UI.
     Team-sync setup is a superuser action here — this platform leaves
     `FEATURE_NONSUPERUSER_TEAM_SYNCING_SETUP` off — so use the `admin`
     superuser (or another `SUPER_USERS` member) to configure it.

`scripts/quay-init` and SSO coexist: the init script bootstraps the local
`admin` superuser, the `holos` org, and the `holos+robot` pull account
(local-database identities used by the push verification above and CI), while
realm users authenticate through SSO. The local form is hidden
(`FEATURE_DIRECT_LOGIN: false`); `admin` remains a break-glass superuser via
`SUPER_USERS`.

In-cluster pulls of `quay.holos.localhost/...` images by the k3d nodes'
containerd are out of scope here — node-level DNS and CA trust for the
registry hostname is a separate concern, tracked by
[HOL-1184](https://linear.app/holos-run/issue/HOL-1184/featquay-in-cluster-image-pulls-from-quayholoslocalhost)
and stubbed in
[placeholders.md](../holos/docs/placeholders.md#node-level-registry-trust-for-in-cluster-pulls).
`scripts/quay-init` only provisions the credentials and the
`quay-robot-pull` pull Secret.

## Verify Argo CD

`scripts/apply` brings Argo CD up at `https://argocd.holos.localhost`.
The server generates the initial `admin` password on first startup and
stores it in the `argocd-initial-admin-secret` Secret — no credentials are
committed to this repository. Verify the UI answers with a
browser-trusted certificate, then log in as the break-glass `admin`:

```bash
curl -fsSI https://argocd.holos.localhost/
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

**Verify Keycloak SSO (OIDC/PKCE).** Real users sign in through Keycloak,
not the `admin` account: Argo CD authenticates against the `holos` realm
with the Authorization Code flow plus PKCE (the public `argocd` client the
`keycloak-config` Job provisions), and maps the user's Keycloak realm role
to an Argo CD role — `platform-owner` → admin, `platform-viewer` /
`platform-editor` → read-only, and every authenticated realm user gets
baseline read-only access via the `authenticated` default group.

Create a realm user and grant it a platform role from the Keycloak admin
console at `https://auth.holos.localhost/admin/` (credentials from the
`keycloak-initial-admin` Secret, per [Verify Keycloak](#verify-keycloak)):
in the `holos` realm, **Users → Add user** (set a username, then a password
under **Credentials**, **Temporary: Off**), then **Role mapping → Assign
role → Filter by realm roles → `platform-owner`**.

Then log in to Argo CD as that user:

```bash
# Open the UI and click "LOG IN VIA Keycloak":
open https://argocd.holos.localhost/
```

After completing the Keycloak login you land back in Argo CD as an
**admin** (full access) because the user holds `platform-owner`. Repeat
with a second user granted `platform-viewer` instead and confirm that
session is **read-only** (the create/sync/delete actions are disabled).
The `admin` break-glass account above still works independently of SSO.

If the Keycloak button is missing or login fails, check the OIDC
backchannel — `argocd-server` must reach the issuer in-cluster (a
`ServiceEntry` resolves `auth.holos.localhost` to the ingress gateway):

```bash
kubectl -n argocd logs deploy/argocd-server | grep -iE 'oidc|x509|dial' || echo "no OIDC errors"
```

A clean run shows no OIDC discovery/JWKS or x509 errors.

Argo CD reconciles nothing yet — no `Application` resources are emitted
until the gitops Application projection is enabled (see
[placeholders.md](../holos/docs/placeholders.md#argocd-gitops-delivery)).
For the full verification steps, the SSO/RBAC configuration, and the
service contract, see
[Argo CD admin credentials and verification](../holos/README.md#argo-cd-admin-credentials-and-verification).

## Verify NATS over wss

For local debugging, the `nats` component exposes the NATS WebSocket port to the
host at `wss://nats.holos.localhost` through an `HTTPRoute` on the shared Gateway
(see the
[host-facing wss debug endpoint](../holos/README.md#host-facing-wss-debug-endpoint)).
The Gateway terminates browser-trusted TLS and forwards the WebSocket upgrade to
NATS, so you can reach JetStream from the host with the `nats` CLI directly — no
`kubectl port-forward`, no throwaway `nats-box` pod. **No credentials are
needed**: NATS runs unauthenticated in the MVP, and the endpoint is reachable
only from this machine because `nats.holos.localhost` resolves to `127.0.0.1`
(the local DNS set up in [Create the cluster](#create-the-cluster)). It is a
debugging affordance, not a production access path — authenticating it is
[deferred](../holos/docs/placeholders.md#nats-in-cluster-authentication).

List the streams from the host to confirm connectivity:

```bash
SERVER=wss://nats.holos.localhost
nats --server "$SERVER" stream ls          # WEBHOOKS and TASKS
nats --server "$SERVER" stream info WEBHOOKS   # Retention: WorkQueue, Storage: File
```

To read the most recent raw bodies the receiver published to the `WEBHOOKS`
stream, use the `scripts/nats-webhooks` reader (it connects over the same
`wss://` endpoint and defaults to the last 10 messages):

```bash
scripts/nats-webhooks        # last 10 retained WEBHOOKS messages, newest first
scripts/nats-webhooks 3      # last 3
```

The reader is **non-destructive**: it fetches each message by sequence with
`nats stream get`, so it never creates a consumer or acks a message — important
because `WEBHOOKS` is a WorkQueue stream the `webhook-subscriber` drains, and an
ack would destroy real work. It needs the `nats` CLI and `jq` on the host. POST
a payload through the Gateway first (see
[Verify the webhook receiver](#verify-the-webhook-receiver) below) so there is a
retained message to read; a drained stream prints `no messages on stream
WEBHOOKS`.

## Verify the webhook receiver

`scripts/apply` brings the `webhook-receiver` up at
`https://hooks.holos.localhost` (the only component running the locally built
`holos-paas` image, pushed by `make docker-push` in the
[Apply the Platform](#apply-the-platform) step). It is the thin HTTP ingress
that publishes raw inbound webhook bodies to the NATS `WEBHOOKS` WorkQueue
stream — see
[Webhook receiver and service contract](../holos/README.md#webhook-receiver-and-service-contract)
for the full contract (status codes, header framing, the durability story, and
the unauthenticated local-only posture). It performs no authentication in the
MVP: from outside the cluster it is reachable only at `hooks.holos.localhost` →
`127.0.0.1` through the shared Gateway, but its in-cluster ClusterIP `Service`
has no ingress policy, so any in-cluster workload can also enqueue a body —
consistent with the MVP's no-in-cluster-auth posture. Edge signature
verification is a future enhancement
([HOL-1200](https://linear.app/holos-run/issue/HOL-1200)); see the
[security posture](../holos/README.md#webhook-receiver-and-service-contract) for
both surfaces.

First confirm the rollout is Ready and `/readyz` reports connected to NATS:

```bash
kubectl -n webhook-receiver rollout status deployment/webhook-receiver --timeout=120s
```

**End-to-end publish.** POST a payload and confirm the **exact raw body** lands
on `webhooks.quay`. Subscribe from a throwaway `nats-box` pod (the same image the
NATS bootstrap Job uses), then POST through the Gateway:

```bash
SERVER=nats://nats.nats.svc.cluster.local:4222
# In one terminal, subscribe to the WEBHOOKS subjects:
kubectl -n nats run nats-sub --rm -it --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" sub 'webhooks.>'

# In another terminal, POST a payload and expect 202 Accepted:
echo '{"name":"sample","docker_url":"quay.holos.localhost/holos/sample"}' > /tmp/payload.json
curl -fsS -o /dev/null -w '%{http_code}\n' \
  -H 'Content-Type: application/json' \
  -X POST --data-binary @/tmp/payload.json \
  https://hooks.holos.localhost/webhooks/quay        # 202
```

The `nats sub` terminal prints one message on subject `webhooks.quay` whose body
is the exact bytes of `payload.json` and whose headers include the forwarded
`Content-Type`. You can also read it back off the stream:

```bash
kubectl -n nats run nats-get --rm -i --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" stream get WEBHOOKS --last-for=webhooks.quay
```

**Durability under a NATS outage (`503` → retry → not lost).** Scale NATS down,
confirm the receiver returns `503` (and `/readyz` goes unready), then scale NATS
back up and confirm a retried POST lands on the stream — proving an accepted
event is never dropped:

```bash
kubectl -n nats scale statefulset/nats --replicas=0
kubectl -n nats rollout status statefulset/nats --timeout=120s   # scaled to 0

# The receiver cannot publish, so it returns 503 (the signal that makes a real
# sender retry). Omit curl's -f here: a non-2xx is the expected result, and -f
# would make curl exit non-zero and abort a set -e shell:
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H 'Content-Type: application/json' \
  -X POST --data-binary @/tmp/payload.json \
  https://hooks.holos.localhost/webhooks/quay        # 503

# Bring NATS back and let the receiver reconnect (unbounded reconnect budget):
kubectl -n nats scale statefulset/nats --replicas=1
kubectl -n nats rollout status statefulset/nats --timeout=300s
kubectl -n webhook-receiver wait pod -l app.kubernetes.io/name=webhook-receiver \
  --for=condition=Ready --timeout=120s

# The retried delivery now succeeds and lands on the file-backed WorkQueue:
curl -fsS -o /dev/null -w '%{http_code}\n' \
  -H 'Content-Type: application/json' \
  -X POST --data-binary @/tmp/payload.json \
  https://hooks.holos.localhost/webhooks/quay        # 202
```

**Iterating on the receiver image.** The Deployment pulls the **mutable** `:dev`
tag with `imagePullPolicy: Always`, so after rebuilding the image the cluster
does not redeploy on its own. Push the new image and restart the Deployment to
pull it:

```bash
make docker-push
kubectl -n webhook-receiver rollout restart deployment/webhook-receiver
kubectl -n webhook-receiver rollout status deployment/webhook-receiver
```

## Verify the webhook subscriber

`scripts/apply` also brings up the `webhook-subscriber` — the durable JetStream
consumer that drains the raw events the receiver stored on the `WEBHOOKS` stream,
parses each into one or more `DeployTask`s, and publishes them to the `TASKS`
stream on `tasks.deploy`. Unlike the receiver it serves no inbound business
traffic (no `Service`, no `HTTPRoute`); it connects to NATS only. See
[Webhook subscriber and DeployTask contract](../holos/README.md#webhook-subscriber-and-deploytask-contract)
for the full DeployTask schema, the durability/retry story, and the
deferred-scope decisions.

First confirm the rollout is Ready and `/readyz` reports connected to NATS:

```bash
kubectl -n webhook-subscriber rollout status deployment/webhook-subscriber --timeout=120s
```

**End-to-end parse and dispatch.** POST a captured Quay `repo_push` payload to
the receiver and confirm a `DeployTask` appears on `tasks.deploy`. Subscribe to
the `TASKS` subjects from a throwaway `nats-box` pod (the same image the NATS
bootstrap Job uses), then POST through the Gateway:

```bash
SERVER=nats://nats.nats.svc.cluster.local:4222
# In one terminal, subscribe to the TASKS subjects:
kubectl -n nats run nats-sub-tasks --rm -it --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" sub 'tasks.>'

# In another terminal, POST a Quay repo_push payload and expect 202 Accepted:
cat > /tmp/quay-push.json <<'JSON'
{"repository":"holos/sample-app","namespace":"holos","name":"sample-app",
 "docker_url":"quay.holos.localhost/holos/sample-app","updated_tags":["v2"]}
JSON
curl -fsS -o /dev/null -w '%{http_code}\n' \
  -H 'Content-Type: application/json' \
  -X POST --data-binary @/tmp/quay-push.json \
  https://hooks.holos.localhost/webhooks/quay        # 202
```

The receiver publishes the raw body to `webhooks.quay`; the subscriber consumes
it, parses it, and publishes a `DeployTask` — so the `nats sub 'tasks.>'`
terminal prints one message on subject `tasks.deploy` whose JSON body carries
`schemaVersion`, `idempotencyKey`, `app` (`sample-app`), `repository`
(`holos/sample-app`), `tag` (`v2`), `source` (`quay`), and `receivedAt` (a
`repo_push` listing several `updated_tags` yields one message per tag). You can
also read the latest task back off the `TASKS` stream:

```bash
kubectl -n nats run nats-get-tasks --rm -i --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" stream get TASKS --last-for=tasks.deploy
```

**Idempotency header.** The dedupe that protects against redelivery lives on
each published message: the subscriber stamps a `Nats-Msg-Id` header of
`<WEBHOOKS-stream-sequence>:<idempotencyKey>`, and JetStream's per-stream
deduplication window collapses any republish carrying the same id (the effect
when a redelivered raw event is re-processed) while still admitting a later
genuine push of the same tag, which arrives at a distinct WEBHOOKS sequence and
so a distinct id. Inspect the header on the task the push above produced — it is
the observable evidence of the dedupe contract (forcing an actual raw-event
redelivery from the CLI is not a simple step; see the
[DeployTask contract](../holos/README.md#webhook-subscriber-and-deploytask-contract)
for the full durability story):

```bash
kubectl -n nats run nats-get-tasks-hdr --rm -i --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" stream get TASKS --last-for=tasks.deploy   # Headers show Nats-Msg-Id: <seq>:<key>
```

Because the subject is a WorkQueue, a single push leaves exactly one task per
pushed tag on `tasks.deploy` — no duplicates accumulate from normal processing.

**Poison messages are Term'd, not redelivered.** An unparseable or
unknown-source body cannot be turned into a task, so the subscriber `Term`s it
(after logging the raw payload base64-encoded under `raw_base64`) rather than
wedging the WorkQueue. Record the `TASKS` message count first, POST a body the
Quay parser rejects, then confirm the count is **unchanged** (no task was
published) and the subscriber logged the termination:

```bash
# Baseline count before the poison POST:
BEFORE=$(kubectl -n nats run nats-cnt --rm -i --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" stream info TASKS --json | jq '.state.messages')

echo '{"not":"a quay push"}' > /tmp/bad.json
curl -fsS -o /dev/null -w '%{http_code}\n' \
  -H 'Content-Type: application/json' \
  -X POST --data-binary @/tmp/bad.json \
  https://hooks.holos.localhost/webhooks/quay        # 202 (the receiver still stores the raw body)

# The subscriber Terms the unparseable event, so TASKS does not grow:
AFTER=$(kubectl -n nats run nats-cnt --rm -i --restart=Never --image=natsio/nats-box:0.19.7 -- \
  nats --server "$SERVER" stream info TASKS --json | jq '.state.messages')
echo "TASKS messages: before=$BEFORE after=$AFTER (expect equal — no task published)"
kubectl -n webhook-subscriber logs deployment/webhook-subscriber | grep 'terminating message'
```

**Iterating on the subscriber image.** Like the receiver, the Deployment pulls
the **mutable** `:dev` tag with `imagePullPolicy: Always`, so after rebuilding
the image push it and restart the Deployment to pull it:

```bash
make docker-push
kubectl -n webhook-subscriber rollout restart deployment/webhook-subscriber
kubectl -n webhook-subscriber rollout status deployment/webhook-subscriber
```

## Reset the Cluster

To reset to a clean state:

```bash
scripts/local-k3d    # Deletes and recreates the cluster (5-second abort window)
sudo -v
scripts/local-ca     # Re-installs the mkcert root CA
```

Alternatively, reapply the manifests saved by the previous `scripts/local-ca`
run without re-running `mkcert --install`:

```bash
scripts/local-k3d
kubectl apply --server-side=true -f "$(mkcert -CAROOT)/namespace.yaml"
kubectl apply --server-side=true -f "$(mkcert -CAROOT)/local-ca.yaml"
```

Either way, the new cluster is empty — re-run `scripts/apply` (see
[Apply the Platform](#apply-the-platform)) to bring the platform
back up.

## Clean Up

Remove the cluster entirely:

```bash
k3d cluster delete holos
k3d registry delete k3d-registry.holos.localhost
```

# Argo CD Application Source: the MVP OCI Pattern

The chosen source pattern for delivering applications in the MVP — the
sample app (Layer 3) and Kargo's `argocd-update` promotion step
([ADR-16](../../docs/adr/ADR-16.md)) both consume this contract. (The
original NATS deployment subscriber, ADR-11, was retired in HOL-1241;
Kargo now writes the `targetRevision`.) The decision itself was made in
[Research: Handling Image-Tag Updates in Argo CD with an OCI Manifest Source](../../docs/research/argocd-oci-image-tag-updates.md)
(Option 1, recommended); this document records the verified, consumable
shape on the local cluster. Verified live end to end in HOL-1188 with the
procedure in [holos/README.md](../README.md#verify-an-oci-source-application)
— re-run it after any change to the pieces below.

## The pattern in one paragraph

Rendered manifests are packaged as an **OCI artifact** with ORAS and pushed
to the in-cluster **Quay** registry. An Argo CD `Application` points at the
artifact with an `oci://` `repoURL`; `targetRevision` selects the artifact
**tag or digest**. Rolling out a new version is a single Kubernetes `PATCH`
of `Application.spec.source.targetRevision` — no Git dependency anywhere
([ADR-6](../../docs/adr/ADR-6.md)). Argo CD ≥ 3.1 is required for the
native OCI source; the argocd component pins 3.x (HOL-1186).

## Artifact layout

Argo CD expects the artifact to contain a **single layer** holding a
gzipped tarball of plain manifests, layer media type
`application/vnd.oci.image.layer.v1.tar+gzip`:

```bash
tar -czf manifests.tar.gz -C <rendered-manifests-dir> .
oras push quay.holos.internal/holos/<repo>:<tag> \
  manifests.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip
```

The `Application.spec.source.path` selects a relative path inside the
expanded tarball — `.` when the manifests sit at the tarball root.

## Tag vs digest in `targetRevision`

Prefer **immutable digests** (`sha256:…`) for anything a controller sets:
the digest is exact, auditable "what's deployed" state, and the registry is
the source of truth for versions ([ADR-8](../../docs/adr/ADR-8.md)). Tags
are acceptable for human-driven smoke tests. Resolve a tag to its digest at
publish time:

```bash
oras resolve quay.holos.internal/holos/<repo>:<tag>
```

Kargo's `argocd-update` promotion step ([ADR-16](../../docs/adr/ADR-16.md))
rolls out a new version by patching the digest (equivalent to):

```bash
kubectl -n argocd patch application <name> --type merge \
  -p '{"spec":{"source":{"targetRevision":"sha256:…"}}}'
```

**The one mutable-tag exception — the App-of-Apps bootstrap.** The platform
App-of-Apps roots and their children deliberately track the **mutable
`holos-paas-config:dev` tag** rather than a digest (HOL-1373,
[ADR-16 Rev 3](../../docs/adr/ADR-16.md)):

```yaml
spec:
  source:
    repoURL: oci://quay.holos.internal/holos/holos-paas-config
    targetRevision: dev   # the mutable bootstrap tag — re-pushing it rolls the platform
```

This is the "Always" image-update behavior: re-pushing `:dev`
(`make config-push`) makes Argo CD re-resolve and reconcile the new render. Argo
CD caches a tag's resolved manifest in the repo-server cache, so the `argocd`
controller component shortens the repo-cache TTL
(`reposerver.repo.cache.expiration`, `ARGOCD_REPO_CACHE_EXPIRATION`) to **`1m`**
so a moved `:dev` is re-pulled within a minute; each child's
`syncPolicy.automated` (`prune` + `selfHeal`) then converges it. The full
mechanism is in
[oci-publish-workflow.md](oci-publish-workflow.md#always-re-pull-of-the-mutable-dev-tag--the-exact-mechanism).
Nothing **sets** this `targetRevision` imperatively (no Kargo `argocd-update`
patch) — it is committed as `dev` and left there; the tag, not the field, moves.

## Repository credential Secret

The repo-server discovers registry credentials from a Secret in the
`argocd` namespace labeled `argocd.argoproj.io/secret-type: repository`.
The verified shape for the in-cluster Quay (credentials from the Quay
pull-robot credential Secret — its provisioning is deferred to a future Quay
Resource Controller, HOL-1293; see the
[Quay credentials and data-plane provisioning](../README.md#quay-credentials-and-data-plane-provisioning)
contract):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: <repo-name>
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: <repo-name>
  url: oci://quay.holos.internal/holos/<repo>
  type: oci
  username: holos+robot
  password: <robot token>
  insecure: "true"
```

`insecure: "true"` skips TLS verification on the repo-server's connection
to the registry. It is required because the Gateway serves
`*.holos.internal` with a certificate signed by the machine-local mkcert
CA (`scripts/local-ca`), which is generated per machine, never committed,
and not in the repo-server image's trust store — verified live: without
the field, sync fails with `x509: certificate signed by unknown
authority`. Distributing the local CA into in-cluster trust stores is the
same concern as the
[node-level registry trust placeholder](placeholders.md#node-level-registry-trust-for-in-cluster-pulls);
revisit both together. The credential is still encrypted in transit — TLS
to whichever workload answers the mesh VIP, unauthenticated — which rests
on trusting the cluster network, acceptable for the local MVP.

### Public repositories: a credential-less registration

When the repository is **public** (`visibility: public` on its
`quay.holos.run` `Repository` CR), the pull is **anonymous** — the
repository Secret drops `username`/`password` entirely and carries only the
non-sensitive registration fields:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: holos-paas-config
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: holos-paas-config
  url: oci://quay.holos.internal/holos/holos-paas-config
  type: oci
  insecure: "true"
```

`insecure: "true"` is still required (the mkcert serving cert is independent
of authentication), so the Secret still exists — but with **no secret
material** it is rendered and committed directly to the deploy tree rather
than assembled at runtime by a bootstrap Job. This is exactly how the
**App-of-Apps platform config bundle** (`holos-paas-config`) is registered:
the `holos-quay-organization` component makes that repository public
(HOL-1381), so the `argocd-projects` component renders this credential-less
registration Secret and the platform no longer depends on a
`holos-paas-config-robot` pull credential. See the *Project Delivery
Scaffold* / App-of-Apps guidance in the repo root `AGENTS.md`.

## How the repo-server reaches Quay (in-cluster reachability)

In-cluster clients use the **same URL as the host**:
`https://quay.holos.internal`, through the shared Gateway. This is not
optional — Quay pins its OCI token-auth realm to
`https://quay.holos.internal/v2/auth` (`SERVER_HOSTNAME` +
`PREFERRED_URL_SCHEME` in
[components/quay/buildplan.cue](../components/quay/buildplan.cue)), so a
client that connects to the in-cluster Service
(`quay.quay.svc.cluster.local:8080`) is still redirected to the public
hostname for every token fetch. The plain-Service-DNS option is therefore
structurally broken for the v2 API, not merely inconvenient.

In-cluster resolution of the public hostname is provided by CoreDNS. The
platform's public hostnames live on the `.internal` TLD (an ICANN-reserved
private-use TLD), **not** `.localhost`. This is deliberate: `.localhost` is
reserved for loopback (RFC 6761), so resolvers — the host's stub resolver,
musl libc inside Alpine pods, and ztunnel's DNS proxy for ambient-enrolled
namespaces (`AMBIENT_DNS_CAPTURE`) — short-circuit `*.localhost` to
`127.0.0.1`/`::1` in-process before the query ever reaches CoreDNS, making
the public hostname unresolvable from in-cluster relying parties (the root
cause behind HOL-1360). `.internal` carries no such special resolver
behavior: musl, glibc, and Go all issue an ordinary DNS query, so the
`components/coredns` rewrite answers `*.holos.internal` authoritatively
in-cluster — mapping it to the shared Istio gateway Service
(`default-istio.istio-gateways.svc.cluster.local`).

Alongside CoreDNS, the `quay-holos-internal` **ServiceEntry** in the quay
component is retained (HOL-1364, conservative scope — deletion is a
follow-up once CoreDNS resolution is verified): it makes
`quay.holos.internal` a service the mesh knows, so ztunnel answers enrolled
pods' DNS queries with an auto-allocated VIP and routes connections to that
VIP to the shared Gateway, which terminates TLS for `*.holos.internal` and
routes by SNI/Host to Quay. In-cluster clients traverse the host's HTTPS
path — same URL, same credentials, same routes (the ServiceEntry declares
only 443, so the Gateway's port-80 redirect listener stays host-only; every
v2 client speaks HTTPS anyway).

Because `.internal` is an ordinary DNS name, the old `*.localhost`
caveat — clients like curl/libcurl (git's HTTPS transport included) that
hardcode `localhost`/`*.localhost` to loopback and never issue the DNS
query — no longer applies: those clients now perform a normal lookup for
`*.holos.internal`, which CoreDNS resolves. Argo CD's static Go binary
(pure-Go resolver) queries DNS and reaches Quay via the sync itself.

## Applications are ordinary namespaced objects

`Application` resources live in the `argocd` namespace and are plain
cluster API objects:

```bash
kubectl get applications.argoproj.io -n argocd
```

A ServiceAccount granted RBAC on `applications.argoproj.io` can patch
`spec.source.targetRevision` directly — this is Kargo's write path: its
`argocd-update` promotion step ([ADR-16](../../docs/adr/ADR-16.md)) sets the
field to the promoted Freight's digest. (The original NATS deployment
subscriber, ADR-11, was retired in HOL-1241.)

## What stays imperative

The repository Secret and any test `Application` are created imperatively
(the repo's runtime-secret posture): the OCI artifact is
pushed imperatively, so a committed `Application` would leave a fresh
bootstrap perpetually Degraded until someone pushes the artifact. The
ServiceEntry is the only committed piece — it depends on nothing
imperative.

Platform self-delivery is now wired: the **OCI App-of-Apps** over the
`holos-paas-config:dev` bundle reconciles the platform's own rendered manifests
([ADR-16 Rev 4](../../docs/adr/ADR-16.md),
[oci-publish-workflow.md](oci-publish-workflow.md)). `scripts/apply` brings Argo
CD up and stops at the bootstrap floor; the separate `scripts/apply-app-of-apps`
then publishes the bundle and applies the two root `Application`s as the handoff
(split out of `scripts/apply` in HOL-1379 because the publish needs the holos
Quay organization configured first) — so the bundle push and the root apply are
the imperative bootstrap steps, after which Argo CD owns reconciliation. This
**supersedes the deferred
per-component git-source projection** (the `argoAppDisabled` flip) for the
platform; that projection stays dormant — see the
[ArgoCD delivery placeholder](placeholders.md#argocd-gitops-delivery).

# Argo CD Application Source: the MVP OCI Pattern

The chosen source pattern for delivering applications in the MVP — the
sample app (Layer 3) and the deployment subscriber's rollout mechanism
([ADR-11](../../docs/adr/ADR-11.md)) both consume this contract. The
decision itself was made in
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
oras push quay.holos.localhost/holos/<repo>:<tag> \
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
oras resolve quay.holos.localhost/holos/<repo>:<tag>
```

The deployment subscriber rolls out a new version by patching the digest:

```bash
kubectl -n argocd patch application <name> --type merge \
  -p '{"spec":{"source":{"targetRevision":"sha256:…"}}}'
```

## Repository credential Secret

The repo-server discovers registry credentials from a Secret in the
`argocd` namespace labeled `argocd.argoproj.io/secret-type: repository`.
The verified shape for the in-cluster Quay (credentials from the
`quay-robot-pull` Secret provisioned by `scripts/quay-init` — see the
[Quay bootstrap](../README.md#quay-bootstrap-and-credentials) contract):

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
  url: oci://quay.holos.localhost/holos/<repo>
  type: oci
  username: holos+robot
  password: <robot token>
  insecure: "true"
```

`insecure: "true"` skips TLS verification on the repo-server's connection
to the registry. It is required because the Gateway serves
`*.holos.localhost` with a certificate signed by the machine-local mkcert
CA (`scripts/local-ca`), which is generated per machine, never committed,
and not in the repo-server image's trust store — verified live: without
the field, sync fails with `x509: certificate signed by unknown
authority`. Distributing the local CA into in-cluster trust stores is the
same concern as the
[node-level registry trust placeholder](placeholders.md#node-level-registry-trust-for-in-cluster-pulls);
revisit both together. The credential itself is still protected: the
connection terminates at the shared Gateway inside the cluster.

## How the repo-server reaches Quay (in-cluster reachability)

In-cluster clients use the **same URL as the host**:
`https://quay.holos.localhost`, through the shared Gateway. This is not
optional — Quay pins its OCI token-auth realm to
`https://quay.holos.localhost/v2/auth` (`SERVER_HOSTNAME` +
`PREFERRED_URL_SCHEME` in
[components/quay/buildplan.cue](../components/quay/buildplan.cue)), so a
client that connects to the in-cluster Service
(`quay.quay.svc.cluster.local:8080`) is still redirected to the public
hostname for every token fetch. The plain-Service-DNS option is therefore
structurally broken for the v2 API, not merely inconvenient.

Plain DNS cannot make the public hostname resolve inside pods:
`*.localhost` names loopback at two independent layers — the upstream
resolver behind CoreDNS (RFC 6761 behavior in the host's resolver), and
ztunnel's DNS proxy for ambient-enrolled namespaces (`AMBIENT_DNS_CAPTURE`
is enabled; ztunnel's resolver special-cases `*.localhost` before
forwarding, so a CoreDNS rewrite never sees enrolled pods' queries).

The committed fix is the `quay-holos-localhost` **ServiceEntry** in the
quay component: it makes `quay.holos.localhost` a service the mesh knows,
so ztunnel answers enrolled pods' DNS queries with an auto-allocated VIP
and routes connections to that VIP to the shared Gateway
(`default-istio.istio-gateways.svc.cluster.local`), which terminates TLS
for `*.holos.localhost` and routes by SNI/Host to Quay. In-cluster clients
traverse the exact host path — same URL, same credentials, same routes.

Caveat: glibc's and musl's `getaddrinfo` special-case `*.localhost` to
loopback **in libc**, before any DNS query, so dynamically-linked clients
(curl, git) inside pods cannot use the hostname even with the ServiceEntry
in place. Argo CD is a static Go binary using the pure-Go resolver, which
does query DNS — verified live. Keep this in mind before pointing other
in-cluster consumers at `*.holos.localhost` hostnames.

## Applications are ordinary namespaced objects

`Application` resources live in the `argocd` namespace and are plain
cluster API objects:

```bash
kubectl get applications.argoproj.io -n argocd
```

A future ServiceAccount granted RBAC on `applications.argoproj.io` can
patch `spec.source.targetRevision` directly — this is the deployment
subscriber's write path ([ADR-11](../../docs/adr/ADR-11.md)). The RBAC
itself lands with the subscriber issue, not here.

## What stays imperative

The repository Secret and any test `Application` are created imperatively
(like the `scripts/quay-init` bootstrap Secrets): the OCI artifact is
pushed imperatively, so a committed `Application` would leave a fresh
bootstrap perpetually Degraded until someone pushes the artifact. The
ServiceEntry is the only committed piece — it depends on nothing
imperative. When the rendered-manifests publish pipeline exists
([Research: rendered-manifests publish pipeline](../../docs/research/rendered-manifests-publish-pipeline.md)),
the gitops Application projection takes over — see the
[ArgoCD delivery placeholder](placeholders.md#argocd-gitops-delivery).

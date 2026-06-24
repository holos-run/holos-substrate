# The Holos Authenticator: an Istio gRPC ext_authz Authorizer for OIDC → Kubernetes Impersonation

| Metadata | Value                                                      |
| -------- | ---------------------------------------------------------- |
| Date     | 2026-06-22                                                 |
| Author   | @jeffmccune                                                |
| Status   | `Implemented`                                              |
| Tags     | controller, authenticator, oidc, istio, authz, impersonation |
| Updates  | ADR-3                                                      |

| Revision | Date       | Author      | Info                                                              |
| -------- | ---------- | ----------- | ----------------------------------------------------------------- |
| 1        | 2026-06-22 | @jeffmccune | Initial design                                                    |
| 2        | 2026-06-23 | @jeffmccune | Flipped to `Implemented` — as-built summary and deviations (below) |
| 3        | 2026-06-24 | @jeffmccune | KSA / static-JWKS extension — additive `spec.oidc.jwks` for offline validation (below) |

## As-built (Revision 2)

The implementation phases (HOL-1385..HOL-1390) shipped the design as proposed,
with the deviations noted below. `Status` is now `Implemented`.

- **As built.** The `cmd/holos-authenticator` controller-runtime manager runs
  the Envoy ext_authz gRPC server as a `manager.Runnable` with
  `NeedLeaderElection() == false`; `Check` routes by `Host`, sanitizes inbound
  `Impersonate-*` headers (failure-closed — denies rather than scrubs), validates
  the OIDC token (issuer discovery + JWKS, `iss`/`aud`/`exp`), maps claims to
  groups via CEL (default `claims["<groupsClaim>"]`, i.e. `claims["groups"]`),
  and returns `Impersonate-User`/`Impersonate-Group` plus the impersonator
  credential, overwriting `Authorization`. The `authenticator.holos.run/v1alpha1`
  `Backend` CRD carries the ADR-22 status contract. The operator guide is
  [the runbook](../runbooks/holos-authenticator.md).
- **Deviation: inbound `Authorization` is accepted and overwritten, not
  stripped/rejected.** The *Design*/*Decision* above describe sanitizing inbound
  `Impersonate-*` **and `Authorization`** headers (failure-closed) before
  injecting the authorizer's own. As built, the caller's `Authorization: Bearer
  <user token>` is **required input** — it carries the OIDC token the authorizer
  validates — so only inbound **`Impersonate-*`** headers are rejected
  failure-closed; the caller's `Authorization` is then **overwritten** with the
  impersonator credential on the OK response (`HeadersToRemove` is intentionally
  empty; the overwrite replaces it in place). HOL-1388's spoofed-header tests
  cover the `Impersonate-*` rejection and the `Authorization` overwrite. The
  security property (a client cannot smuggle a privileged group or reuse its own
  token downstream) holds; the mechanism is overwrite-on-allow, not
  reject-on-inbound, for `Authorization`. The runbook documents the as-built
  model.
- **Deviation: `server.caBundle` is recorded but not yet consumed.** The
  authorizer copies `spec.server.caBundle` into its in-memory `Backend` entry but
  does **not** dial the upstream API server — Envoy (the waypoint) forwards the
  impersonated request, so upstream TLS trust is the waypoint/`ServiceEntry`
  routing layer's concern. The field exists for the deferred external-egress
  topology; `oidc.caBundle` (the issuer-discovery/JWKS dial) **is** consumed.
- **Deviation: the `Backend` reconciler also runs on every replica.** The
  *Design* above (and the original proposal) framed leader election as gating the
  reconcilers that keep the in-memory backend configuration current. As built,
  each replica's registry is **process-local**, so the `Backend` reconciler is
  configured `NeedLeaderElection=false` and runs on **every** replica — not only
  the leader — precisely so each replica's registry is populated for its own data
  path. Leader election is therefore not on the authorization path at all.
- **Deviation: deployed as a Holos component, not a kustomize tree.** The design
  pointed at `holos-controller`'s build/release machinery as the template, and
  the authenticator reuses it (isolated `Makefile.authenticator`,
  `Dockerfile.authenticator`, a discrete Images-workflow job). But unlike
  `holos-controller` (deployed via its `config/` kustomize tree, not
  platform-native), the runtime manifests are rendered by a **Holos component**
  (`holos/components/holos-authenticator/`) so the deploy tree stays render-clean
  and the namespace is ambient-enrolled via the central registry. The component
  is registered in `holos/platform/platform.cue` but **excluded** from the
  imperative `scripts/apply` floor and the App-of-Apps `SYSTEM_COMPONENTS` (its
  image does not exist on a freshly bootstrapped cluster) — it is applied out of
  band once published, the same precedent `holos-controller` set.
- **Deviation: external-egress waypoint topology deferred.** The design assumed
  ambient mesh with Envoy as the only proxy and noted external API-server targets.
  The as-built ships the **in-cluster wiring** — the `meshConfig.extensionProviders`
  `envoyExtAuthzGrpc` provider, a `CUSTOM` `AuthorizationPolicy`, and one example
  in-cluster `Backend`. The full external-API-server `ServiceEntry`/waypoint
  egress topology (and the positive tenant-policy enforcement guard) is a
  **deferred follow-up** recorded in
  [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md), mitigated
  today by construction (no waypoint deployed, the manager cache scoped to the
  `holos-authenticator` namespace, the impersonator credential resolved only from
  that namespace, and the namespace reserved against tenant projects). Token
  refresh/caching tuning and per-request CEL features beyond the default groups
  mapping are likewise deferred there.

## Static-JWKS / KSA extension (Revision 3)

Revision 3 (HOL-1392..HOL-1395) extends the `Backend` to validate tokens
**offline** against a static JSON Web Key Set, so the authenticator can front a
**remote cluster's Kubernetes API server** whose service-account (KSA) token
issuer is unreachable from this cluster. The motivating case is a remote workload
(e.g. an External Secrets Operator `SecretStore`) presenting its projected KSA ID
token to be impersonated on the management cluster.

- **Additive `spec.oidc.jwks` (no new CRD).** The existing `Backend` CR was
  **extended**, not replaced — a single optional `oidc.jwks` field (a base64
  `[]byte` holding the literal `{"keys":[…]}` document) carrying the static key
  set. The `authenticator.holos.run/v1alpha1` group and the `Backend` Kind are
  unchanged otherwise; backends that omit `jwks` are fully backward-compatible.
- **Offline validation, `iss`/`aud`/`exp` still enforced.** When `oidc.jwks` is
  set the authorizer performs **no OIDC discovery and no JWKS HTTP fetch**: it
  verifies the token signature against the static keys and still checks `iss`
  (== `oidc.issuerURL`, now the expected `iss` value only — not dialed), `aud`
  (== `oidc.clientID`), and `exp`/`nbf`. `oidc.caBundle` is unused on this path
  (there is no issuer endpoint to trust). The CEL group mapping is identical to
  the discovery path.
- **KSA impersonation via `sub` + an SA-group CEL expression.** A KSA ID token's
  `sub` is `system:serviceaccount:<ns>:<name>`, so the default
  `usernameClaim: sub` already reproduces the SA username for `Impersonate-User`.
  The SA's Kubernetes **virtual groups** are reproduced from the projected
  token's `kubernetes.io` claim with the CEL expression
  `["system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + claims["kubernetes.io"].namespace]`,
  so RBAC on the management cluster authorizes the impersonated request exactly as
  the SA would be. The impersonator `credentialsSecretRef` identity must therefore
  hold `impersonate` on the SA username and those SA virtual groups (runbook).
- **Malformed JWKS → `InvalidSpec`.** A static JWKS is pure spec data with no
  external system involved, so a malformed/unparseable JWKS (or one with zero
  usable keys) is rejected at reconcile time as an **invalid spec**
  (`Accepted=False`/`Ready=False`, reason `InvalidSpec`) — not the transient
  `DiscoveryFailed`. A valid static-JWKS backend reaches `Ready=True` via the same
  `succeed` path as a discovery backend, with no network I/O.
- **1:1 host↔Backend for remote clusters.** Each fronted remote cluster gets its
  own `Backend` with a unique `spec.host`, its own static JWKS, and its own
  management-cluster impersonator credential. The component renders
  `remote-cluster-a` as the worked KSA example (a redacted-placeholder `jwks`);
  it is excluded from the bootstrap floor and never applied automatically.
- **Key selection kept at parity with discovery (HOL-1396 hardening deferred).**
  Static-JWKS validation uses the same global supported-algorithm set as the
  discovery path, with **no per-`kid` key selection or per-key algorithm
  enforcement** — built to parity with discovery on purpose. Adding per-`kid` key
  selection and per-key alg enforcement to **both** the static and discovery paths
  together is a deferred follow-up tracked in **HOL-1396**.

The operator procedure (capturing the remote JWKS/issuer, the SA-group CEL
expression, the SA-virtual-group impersonation RBAC, and end-to-end ESO
verification) is in the [runbook's *KSA / static-JWKS
backends*](../runbooks/holos-authenticator.md#ksa--static-jwks-backends) section;
the component's static-JWKS mode is documented in its
[README](../../holos/components/holos-authenticator/README.md).

## Context and Problem Statement

The platform needs a minimal Istio **external authorizer** that lets Envoy (an
ambient-mesh waypoint, with **no other reverse proxy** in the path) front one or
more Kubernetes API servers — in-cluster **or external** — and authenticate end
users via OIDC, translating each user's identity into Kubernetes **impersonation**
so any conformant API server authorizes the request with the user's real groups.

How should the platform authenticate an end user's OIDC token at the edge, map
that user's claims to Kubernetes groups, and forward the request to a target API
server as that user — using only Envoy and one external authorizer, configured
entirely through Kubernetes custom resources, supporting multiple backends each
with their own OIDC client, without coupling the authorizer to any single
cluster's API server?

This ADR records the design of a new `holos-authenticator` service that answers
that question. It is the binding design record the implementation phases
(HOL-1385..HOL-1390) reference. This first phase (HOL-1385) ships the **service
scaffold** — the binary, build tasks, image workflow, and a trivial ext_authz
stub — and records this design; later phases flip the relevant `Status` to
`Implemented` as the OIDC, CEL, CRD, and platform-wiring work lands.

## Context / References / Prior Work

- **Envoy `ext_authz` gRPC protocol** (`envoy.service.auth.v3.Authorization`):
  the prior art and the wire contract. Envoy's external-authorization filter
  calls a gRPC `Check` on each request; the authorizer returns an `OkResponse`
  (optionally injecting/overwriting request headers the upstream then sees) or a
  `DeniedResponse` (an HTTP status and body Envoy returns directly). This is the
  exact mechanism the authenticator implements; Istio exposes it through a
  `meshConfig.extensionProviders` entry of type `envoyExtAuthzGrpc` plus an
  `AuthorizationPolicy` with `action: CUSTOM`.
- **Kubernetes user impersonation**: a request carrying `Impersonate-User` and
  `Impersonate-Group` headers, made with a credential that holds the
  `impersonate` verb, is authorized by the API server **as the impersonated
  user/groups**. This is a core, conformant Kubernetes feature — every compliant
  API server honors it — which is what lets one authorizer front *any* cluster
  without cluster-specific authentication plumbing.
- **CEL (Common Expression Language)**: the same expression language Kubernetes
  already uses for admission and CRD validation. The authenticator uses a CEL
  expression to map OIDC token claims to Kubernetes groups, so the mapping is
  declarative, sandboxed, and configurable per backend without code changes.
- [ADR-3 — Authorization via Kubernetes RBAC and group membership](ADR-3.md):
  the platform authorizes users by their group membership in Kubernetes RBAC.
  The authenticator is the bridge that turns an external OIDC identity's groups
  into the Kubernetes groups ADR-3's RBAC binds against, so this ADR **updates**
  ADR-3 by supplying the OIDC→groups translation mechanism the model assumes.
- [ADR-18 — The Holos Controller](ADR-18.md): the conventional kubebuilder
  controller-runtime manager pattern (standard-library `flag`, zap JSON logging,
  metrics/health endpoints, leader election, a stamped `version`) that the
  `holos-authenticator` binary mirrors. The authenticator reuses the **build and
  release machinery** template — isolated `Makefile.authenticator`,
  `Dockerfile.authenticator`, and a discrete job in the manual Images workflow —
  exactly as `holos-controller` established it.
- [ADR-19 — Quay API Group](ADR-19.md) / [ADR-22 — `security.holos.run`](ADR-22.md):
  the Gateway-API status-condition model (`Accepted`/`Programmed`/`Ready`,
  `observedGeneration`, `+listType=map`/`+listMapKey=type`, a `Ready` printer
  column) that ADR-22 mandates for **all** `holos.run` CRs. The
  `authenticator.holos.run` CRDs (HOL-1386) adopt this contract.
- [ADR-12 — Repository layout](ADR-12.md): the single-module monorepo with one
  binary per service under `cmd/`. `cmd/holos-authenticator` is a new service
  binary under that layout, alongside `cmd/holos-paas` and `cmd/holos-controller`.

## Design

### The service: a controller-runtime manager that also serves ext_authz

`holos-authenticator` is a controller-runtime **manager** (mirroring
`cmd/holos-controller/main.go`) that additionally runs the Envoy ext_authz gRPC
server as a `manager.Runnable` registered with `mgr.Add`. Hosting the gRPC server
on the manager's lifecycle means the server and the (future)
`authenticator.holos.run` reconcilers share **one process, one context, and one
leader-election session**: the reconcilers keep the in-memory backend
configuration current; the gRPC server reads it to answer Envoy.

The manager wiring is the conventional kubebuilder idiom: standard-library `flag`
parsing, controller-runtime's `zap` JSON logging (production encoder by default,
suitable for log ingestion), a Prometheus `--metrics-bind-address` (default
`:8080`), a `--health-probe-bind-address` (default `:8081`) serving
`healthz`/`readyz`, `--leader-elect` with
`LeaderElectionID: "holos-authenticator.holos.run"`, and a `version` var stamped
at link time. The ext_authz server binds a configurable `--grpc-bind-address`
(default `:9000`).

Crucially, the gRPC `Runnable` reports **`NeedLeaderElection() == false`**: every
replica must answer Envoy on the data path, not only the elected leader. Leader
election (when enabled) gates only the reconcilers that mutate shared state, not
the per-request authorization the gRPC server performs. The scaffold phase
(HOL-1385) registers only the core Kubernetes scheme (`clientgoscheme`) — no
`authenticator.holos.run` group yet — and the gRPC `Check` is a deterministic
**always-Denied (HTTP 403)** stub that proves the proto wiring serves end to end.

### The request path: OIDC validate → CEL map → impersonate → forward

On each request Envoy forwards to the authorizer's `Check`, the authenticator
(in the fully-implemented design):

1. **Selects the backend.** The request's target (matched by the
   `AuthorizationPolicy`/route that routed it to this ext_authz provider)
   identifies which `Backend` custom resource — and therefore which OIDC client,
   issuer, group-mapping expression, and upstream API server — applies.
2. **Sanitizes inbound impersonation/credential headers (mandatory).** Before
   anything else, the authorizer **must** strip or reject any client-supplied
   `Impersonate-User`, `Impersonate-Group`, `Impersonate-Uid`,
   `Impersonate-Extra-*`, and `Authorization` headers on the inbound request, so
   that only the authorizer's own derived values reach the upstream. This closes a
   header-smuggling / confused-deputy hole: because the authorizer injects the
   backend's privileged impersonation credential downstream, a client that smuggled
   `Impersonate-Group: system:masters` (or any other group) would otherwise be
   impersonated at that privilege. The ext_authz `OkResponse` therefore both sets
   the derived impersonation headers **and** removes any inbound impersonation/`Authorization`
   headers (using the response's header-mutation `headers_to_remove` / explicit
   overwrite); the implementation is **failure-closed** — an inbound request that
   *carries* impersonation headers is denied rather than silently scrubbed if there
   is any doubt the upstream would see the client's version. HOL-1388 implements
   this with explicit unit tests for spoofed inbound `Impersonate-*` /
   `Authorization` headers.
3. **Validates the OIDC identity token.** It extracts the bearer token, performs
   issuer discovery and JWKS signature verification, and checks `iss`, `aud`
   (the backend's OIDC client), and `exp`. A token that fails validation yields a
   `DeniedResponse` (HTTP 401/403).
4. **Maps claims to Kubernetes groups via CEL.** It evaluates the backend's CEL
   expression against the validated claims to produce the user identity and the
   Kubernetes group list. The **default** expression maps the token's `groups`
   claim directly to Kubernetes groups; a backend may override it (e.g. to derive
   groups from a different claim, prefix them, or filter them).
5. **Returns Kubernetes impersonation headers.** On success it returns an
   `OkResponse` injecting `Impersonate-User` and one `Impersonate-Group` header
   per mapped group, **compatible with any conformant cluster**, plus the
   backend's own privileged credential (an `Authorization: Bearer <token>` for a
   ServiceAccount holding the `impersonate` verb), having first removed every
   inbound impersonation/`Authorization` header per step 2 so only the derived
   values reach the upstream. Envoy then forwards the request to the upstream API
   server with those headers and **no other reverse proxy** in the path; the API
   server authorizes the request as the impersonated user with their real groups.

### Configuration as Kubernetes custom resources: `authenticator.holos.run`

All configuration is expressed as Kubernetes custom resources in a new
**`authenticator.holos.run`** API group (the CRD lands in HOL-1386). The central
Kind is a **`Backend`**: one API server backend, with exactly **one OIDC client**
(issuer URL, client ID, and the validation parameters), an **upstream API server
URL** that **may be external** to the cluster (with an optional trusted CA bundle
for a privately-signed endpoint), the backend's **impersonation credential**
reference, and a per-backend **group-mapping CEL expression** (defaulting to the
`groups`-claim mapping). **Multiple backends** are supported — one `Backend`
resource per fronted API server — so a single authenticator deployment can serve
several clusters, each with its own OIDC client and mapping.

Following [ADR-22](ADR-22.md), every `authenticator.holos.run` CR reports rich
Gateway-API status: a `status.conditions[]` of `metav1.Condition` with
`Accepted`/`Programmed`/`Ready`, a `status.observedGeneration`, and a `Ready`
printer column. Any cross-namespace reference a `Backend` makes is authorized by a
`security.holos.run` `ReferenceGrant` ([ADR-22](ADR-22.md)) in the referent
namespace, like every other `holos.run` group.

### Deployment: ambient mesh, Envoy as the only proxy

The authenticator is deployed assuming **Istio ambient mesh** (HOL-1389). Istio
is configured with a `meshConfig.extensionProviders` entry naming the
authenticator's gRPC service as an `envoyExtAuthzGrpc` provider, and an
`AuthorizationPolicy` with `action: CUSTOM` referencing that provider attaches the
authorizer to the fronted routes. Because Kubernetes impersonation lets the API
server authenticate the forwarded request, **the ambient-mesh Envoy waypoint is
the only proxy** between the client and the upstream API server — no sidecar
reverse proxy or API-server auth proxy is added to the path.

### Dependencies kept minimal

New Go dependencies are held to the minimum the protocol requires. This scaffold
phase adds only **`github.com/envoyproxy/go-control-plane`** (for the
`envoy/service/auth/v3` ext_authz types — its `/envoy` submodule under modern
versions, pinned to a version that keeps `google.golang.org/grpc` at the existing
release rather than forcing a broad transitive upgrade) and promotes
**`google.golang.org/grpc`** from an indirect to a direct dependency.
**`google.golang.org/genproto/googleapis/rpc`** is likewise promoted to direct,
because the ext_authz `CheckResponse` carries a `google.rpc.Status` the stub sets
(`server.go` imports `rpc/status`); it was already an indirect dependency, so this
adds no new module to the build graph. OIDC and CEL libraries are **deliberately
not** added until the phase that uses them (HOL-1387), keeping each phase's
dependency footprint legible.

## Decision

1. **A new `holos-authenticator` service is established**, built from this
   monorepo using `holos-controller` (ADR-18) as the template for its binary
   layout, build tasks, and image workflow: a `cmd/holos-authenticator` manager,
   an isolated `Makefile.authenticator` (`authenticator-*` targets reusing the
   shared buildx builder), a `Dockerfile.authenticator` (distroless non-root,
   cross-compiled), and a discrete `holos-authenticator` job/option in the manual
   `.github/workflows/images.yaml` Images workflow.
2. **It implements an Istio external authorizer over the Envoy gRPC ext_authz
   protocol** (`envoy.service.auth.v3.Authorization`), run as a controller-runtime
   `manager.Runnable` (via `mgr.Add`) that does **not** require leader election —
   every replica answers Envoy.
3. **It validates an OIDC identity token** (issuer discovery, JWKS signature,
   `iss`/`aud`/`exp`), **maps the token's claims to Kubernetes groups via a CEL
   expression** (default: the `groups` claim), and on success **returns Kubernetes
   impersonation headers** (`Impersonate-User`, `Impersonate-Group`) plus the
   backend's privileged credential, so Envoy forwards to the API server with **no
   other reverse proxy** in the path. The impersonation headers are compatible
   with any conformant cluster. As a **mandatory, failure-closed** precondition, it
   **strips or rejects** any client-supplied `Impersonate-*` and `Authorization`
   headers before injecting its own, so a client cannot smuggle a privileged group
   (e.g. `system:masters`) under the backend's impersonation credential; HOL-1388
   ships this with explicit spoofed-header tests.
4. **All configuration is Kubernetes custom resources** in a new
   `authenticator.holos.run` API group whose central `Backend` Kind models one
   API server backend with **one OIDC client**, an upstream URL that **may be
   external** (with an optional trusted CA bundle), and a per-backend group-mapping
   CEL expression. **Multiple backends** are supported. Every CR carries the
   ADR-22 Gateway-API status contract.
5. **It is deployed assuming ambient mesh** — an Istio `extensionProviders`
   ext_authz gRPC provider plus an `AuthorizationPolicy` with `action: CUSTOM` —
   with Envoy as the only proxy.
6. **Dependencies are limited to what is necessary**, added per phase: this
   scaffold phase adds only `github.com/envoyproxy/go-control-plane` and promotes
   `google.golang.org/grpc` to a direct dependency; OIDC/CEL libraries are added
   only when their phase needs them.
7. **This ADR is the binding design record for the implementation phases**
   (HOL-1385..HOL-1390). It starts `Proposed`; the scaffold (HOL-1385) ships the
   binary, build tasks, image workflow, and an always-Denied ext_authz stub, and
   later phases flip the `Status` toward `Implemented` as the OIDC, CEL, CRD, and
   platform-wiring work lands.

## Consequences

- **A new service to build and operate.** `holos-authenticator` is a third
  service binary alongside `holos-paas` and `holos-controller`, with its own
  image, deploy surface, and lifecycle. The isolated `authenticator-*` build
  targets and the discrete Images-workflow job keep it from colliding with the
  existing services, at the cost of one more thing to release.
- **A new `authenticator.holos.run` API group and reconcilers.** The `Backend`
  CRD (HOL-1386) and the reconcilers that keep its in-memory configuration current
  (HOL-1387+) are future work this ADR scopes but does not ship. They inherit the
  ADR-22 status contract and the `security.holos.run` `ReferenceGrant`
  cross-namespace convention.
- **A privileged impersonation credential per backend.** Each backend forwards
  with a credential holding the Kubernetes `impersonate` verb — a powerful
  permission. Compromise of the authenticator or a backend credential would let an
  attacker impersonate arbitrary users/groups on that backend's API server, so the
  credential handling, the ext_authz trust boundary, and the failure-closed
  default (deny on any validation error) are security-critical. The scaffold's
  always-Denied stub is failure-closed by construction.
- **Inbound header sanitization is non-negotiable.** Because the authorizer injects
  the backend's privileged impersonation credential downstream, it **must** strip
  or reject any client-supplied `Impersonate-User`/`Impersonate-Group`/`Impersonate-Uid`/`Impersonate-Extra-*`
  and `Authorization` headers before forwarding (failure-closed). Omitting this would
  let a client smuggle a privileged group such as `system:masters` and have the
  upstream honor it under the backend credential — a full privilege-escalation. The
  allow-path implementation (HOL-1388) carries this requirement and explicit
  spoofed-inbound-header tests; the scaffold has no allow path and so cannot leak.
- **Conformant-cluster portability, external backends included.** Relying only on
  standard Kubernetes impersonation (not cluster-specific auth plumbing) lets one
  authorizer front in-cluster and external API servers alike, but it binds the
  design to the impersonation contract: a backend's credential must hold the
  `impersonate` verb and the upstream must trust the presented CA bundle.
- **Updates ADR-3.** This ADR supplies the concrete OIDC-identity → Kubernetes-
  groups translation the ADR-3 RBAC/group-membership authorization model assumes,
  without changing that model.
- **Minimal, phase-scoped dependency growth.** Holding new dependencies to
  `go-control-plane` + `grpc` now (and adding OIDC/CEL only when used) keeps the
  module's dependency surface legible, but means the ext_authz proto types are now
  a direct, long-lived dependency to track for security updates.

# holos-authenticator component

Deploys the **Holos Authenticator** ([ADR-23](../../../docs/adr/ADR-23.md)) into
the platform and wires it as an Istio external authorizer in the ambient mesh.

The authenticator is a controller-runtime manager that runs an **Envoy
`ext_authz` gRPC server** and reconciles `authenticator.holos.run` **Backend**
custom resources. Each `Backend` fronts one Kubernetes API server with OIDC
token validation and Kubernetes impersonation: on a valid token the authorizer
returns an OK response that sets `Impersonate-User` and a single comma-joined
groups header and replaces the caller's `Authorization` with the backend's
privileged credential, so Envoy forwards the request straight to the API server.

> **Groups are a single comma-joined header that needs a paired Lua split filter.**
> The authorizer writes the mapped groups as one CSV value under the configured
> groups header (default `X-Impersonate-Groups`, `--impersonate-groups-header`) with
> the **overwrite/set** action, **not** as per-group `Impersonate-Group` append
> options — Envoy's ext_authz path drops an appended header when the request does
> not already carry it, which silently lost every group (HOL-1416). The header must
> be paired with an Envoy Lua **split** filter that unpacks the comma list into one
> `Impersonate-Group` per group, ordered after ext_authz with the version-stable
> `filterClass: AUTHZ` (an optional **reject** filter that refuses a client-supplied
> copy adds defense in depth but is **not** required — smuggling prevention is the
> authenticator's own responsibility, ADR-23 Revision 8) — see the runbook's
> [*Splitting the comma-joined groups
> header*](../../../docs/runbooks/holos-authenticator.md#splitting-the-comma-joined-groups-header)
> and [ADR-23](../../../docs/adr/ADR-23.md) Revisions 7–8. Like the `CUSTOM`
> `AuthorizationPolicy`, the filters belong to the deferred waypoint topology and
> are not yet rendered by this component.

## What this component renders

`buildplan.cue` emits, into
`holos/deploy/clusters/<cluster>/components/holos-authenticator/`:

- the manager **Deployment** — image
  `quay.holos.internal/holos/holos-authenticator:dev`, `POD_NAMESPACE` via the
  downward API, gRPC `:9000`, metrics `:8080`, health `:8081` (the flag/port
  contract in `cmd/holos-authenticator/main.go`);
- a **ServiceAccount** (`holos-authenticator`);
- the authenticator **ClusterRole** + **ClusterRoleBinding**
  (`holos-authenticator-role`/`-rolebinding`, from the generated
  `config/authenticator/rbac/role.yaml`) and the namespaced **Role** +
  **RoleBinding** granting the manager `get` on Secrets in its own namespace
  (the per-Backend `credentialsSecretRef`) **and** `create` on
  `serviceaccounts/token` scoped by `resourceNames` to
  `holos-authenticator-impersonator` (the `serviceAccountRef` TokenRequest mint);
- the default impersonator **ServiceAccount** (`holos-authenticator-impersonator`,
  distinct from the manager's own SA) plus an impersonate-only **ClusterRole** +
  **ClusterRoleBinding** (`holos-authenticator-impersonator`) — the SA a
  `Backend`'s `spec.serviceAccountRef` defaults to (see *Impersonation RBAC*
  below);
- a **Service** exposing the gRPC and metrics ports;
- the generated **Backend CRD** (vendored from
  `config/crd/bases/authenticator.holos.run_backends.yaml`);
- an **AuthorizationPolicy** with `action: CUSTOM` and
  `provider.name: holos-authenticator`, matching the Istio extension provider;
- three example **Backend** CRs — the discovery-based `example` (in-cluster
  Keycloak issuer, `credentialsSecretRef`), the static-JWKS `remote-cluster-a`
  (KSA / offline mode, `serviceAccountRef: {}`, below), and `delegated-example`
  demonstrating **delegated impersonation** (`spec.impersonation` — a `groups`
  allowlist + `extra[]` actor attribution, see *Delegated impersonation* below).

No `Namespace` is emitted: the `holos-authenticator` namespace is owned by the
central registry (`holos/namespaces.cue`) and rendered by the `namespaces`
component.

## Istio extension provider

`holos/components/istio/istio.cue` declares the matching gRPC `ext_authz`
provider in `IstioValues.meshConfig.extensionProviders`:

```cue
meshConfig: extensionProviders: [{
  name: "holos-authenticator"
  envoyExtAuthzGrpc: {
    service: "holos-authenticator.holos-authenticator.svc.cluster.local"
    port:    9000
    timeout: "2s"
  }
}]
```

The `istiod` component passes `IstioValues` verbatim as Helm values, so the
provider lands in the mesh `MeshConfig`. The component's `AuthorizationPolicy`
references it by name (`provider.name: holos-authenticator`).

> **Ambient / L7 enforcement requires a waypoint.** ztunnel is L4-only; an
> `ext_authz` decision is an L7 concern, so a **waypoint** must front the
> protected workload for the `CUSTOM` AuthorizationPolicy to take effect. The
> example policy selects the authenticator's own pods as a harmless default; a
> real deployment retargets the selector at the protected workload behind a
> waypoint. The full waypoint / `ServiceEntry` topology for an **external** API
> server target is a deferred follow-up — see `holos/docs/placeholders.md`
> (finalized in the next phase).

## Static-JWKS / KSA backends (offline validation)

A `Backend` can validate tokens **offline** against a static JSON Web Key Set
instead of doing OIDC discovery. Set `spec.oidc.jwks` to the issuer's
`{"keys":[…]}` document — **base64-encoded**, since the field is a `[]byte`
(CRD `type: string, format: byte`), the same single-base64-string convention the
`caBundle` fields use (the rendered `remote-cluster-a` example shows the encoded
form) — and the authorizer:

- performs **no OIDC discovery and no JWKS HTTP fetch** — it verifies the token
  signature against the keys in `spec.oidc.jwks` directly;
- treats `spec.oidc.issuerURL` as the **expected `iss` claim value only** (it is
  not dialed), and ignores `spec.oidc.caBundle` (there is no issuer endpoint to
  trust);
- still enforces `iss` (== `issuerURL`), `aud` (== `clientID`), and `exp`/`nbf`,
  and runs the same CEL group mapping as the discovery path.

A malformed/unparseable JWKS, or one with zero usable keys, is rejected at
reconcile time as an **invalid spec** (`Accepted=False`/`Ready=False`, reason
`InvalidSpec`) — not a transient discovery failure. An empty `spec.oidc.jwks`
is unchanged: the authorizer performs OIDC discovery as before.

> Key selection / per-key algorithm enforcement currently matches the discovery
> path (the configured supported-algorithm set, no per-`kid` binding). Tightening
> both paths together (per-`kid` key selection, per-key alg enforcement) is the
> planned hardening tracked in HOL-1396.

This is the mechanism for a token issuer that is **unreachable** from this
cluster — most importantly a **remote cluster's Kubernetes API server signing
service-account (KSA) ID tokens**. The model is **1:1 host↔Backend**: each
remote cluster gets its own `Backend` with a unique `spec.host` (e.g.
`remote-cluster-a.holos.internal`), its own static JWKS, and its own
impersonator credential for the management cluster. The component renders
`remote-cluster-a` as the worked KSA example; its `jwks` is a **redacted
placeholder** an operator replaces with the remote cluster's real JWKS document
(`kubectl get --raw /openid/v1/jwks`). The JWKS is non-secret public-key
material and may live in the CR. This **inbound** validation is independent of
the **outbound** impersonator credential: the rendered `remote-cluster-a`
example uses `serviceAccountRef: {}` (the controller mints/rotates the default
impersonator SA token — no Secret), and an external management cluster would use
`credentialsSecretRef` (a runtime Secret, never committed) instead. See
*Credential sources* below.

The full operator procedure — capturing the remote JWKS/issuer, the SA-group
CEL expression, the SA-virtual-group impersonation RBAC, and end-to-end
External Secrets Operator (`SecretStore`/`ExternalSecret`) verification — is in
the runbook's [*KSA / static-JWKS
backends*](../../../docs/runbooks/holos-authenticator.md#ksa--static-jwks-backends)
section.

## Credential sources: `serviceAccountRef` and `credentialsSecretRef`

A `Backend`'s **outbound impersonator credential** — the identity the upstream
API server authenticates Envoy as — comes from one of two **mutually exclusive**
spec fields (a CRD-level CEL validation rejects setting both):

- **`spec.serviceAccountRef`** (ADR-23 Rev 4) — reference a ServiceAccount in the
  `holos-authenticator` namespace and the controller **mints and rotates** its
  bearer token via the Kubernetes TokenRequest API. No manual `kubectl create
  token`, no Secret. `serviceAccountRef.name` defaults to the shipped
  **`holos-authenticator-impersonator`** SA; `audience` defaults to the API
  server's default audience; `expirationSeconds` defaults to `3600` (min `600`).
  Tokens are cached (keyed by name + audience + expirationSeconds) and rotated
  before expiry (margin = the smaller of 5m or 20% of lifetime), minted **without**
  a `BoundObjectRef`. The manager's namespaced `Role` grants `create` on
  `serviceaccounts/token` scoped by `resourceNames` to
  `holos-authenticator-impersonator`.
- **`spec.credentialsSecretRef`** — name a Secret (default
  `holos-authenticator-backend-creds`, key `token`) holding a raw bearer token.
  This is the right choice for an **external** API server whose impersonator
  credential is provisioned out of band (the management cluster cannot mint a
  token for a remote cluster's SA).

> **This is the *outbound* impersonator credential, not the Rev 3 *inbound*
> `oidc.jwks` validation keys.** `serviceAccountRef`/`credentialsSecretRef` say
> *whom the authorizer authenticates as* to `spec.server.url`; the static-JWKS
> `oidc.jwks` (above) says *which inbound remote-cluster SA token it validates*.
> A KSA Backend commonly uses both. Don't conflate the two SA-related features.

Whichever source supplies it, the impersonator identity **must** hold RBAC that
grants the `impersonate` verb on whatever the CEL mapping emits.

### The shipped default impersonator SA is impersonate-only and bounded

The component ships the **`holos-authenticator-impersonator`** ServiceAccount
(distinct from the manager's own `holos-authenticator` SA) bound to a
deliberately narrow ClusterRole. The shipped default grants `impersonate` on
**`groups` only**, scoped by `resourceNames` to the two namespace-independent SA
virtual groups — **nothing** on `users` or `serviceaccounts`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-authenticator-impersonator
rules:
  - apiGroups: [""]
    resources: ["groups"]
    verbs: ["impersonate"]
    resourceNames:
      - "system:authenticated"
      - "system:serviceaccounts"
```

> **Bounded by design (ADR-23 Rev 4 ratifies this).** An **unbounded**
> `impersonate` on `users`/`groups` is a cluster-wide privilege-escalation
> credential (it can impersonate any user/group, including `system:masters`). The
> parent issue's literal AC asked for `users`/`groups`/`serviceaccounts`; the
> as-shipped default was narrowed to the SA virtual groups for security and
> ratified in ADR-23 Revision 4. Add per-`Backend` impersonate scope (a specific
> SA via the `serviceaccounts` resource, a per-namespace
> `system:serviceaccounts:<ns>` group, or specific users) **per Backend** — see
> the runbook's [*Impersonation
> RBAC*](../../../docs/runbooks/holos-authenticator.md#impersonation-rbac-the-forwarded-credential)
> and the KSA worked example — never by widening this default ClusterRole.

### Runtime Secret handling

Per the **Runtime Secret Handling** guardrail, a `credentialsSecretRef`
credential's **material is never committed**. The `example` `Backend` *names*
`holos-authenticator-backend-creds` via `credentialsSecretRef`; that Secret is
created at runtime in the `holos-authenticator` namespace, out of band. (A
`serviceAccountRef` Backend needs no Secret at all — the controller mints the
token.) The example `Backend`s likewise omit the `caBundle` fields so the
committed manifests carry no per-cluster trust material; an operator injects the
local-ca PEM out of band (mirroring the caBundle convention the
project/application components use).

## Delegated impersonation (`spec.impersonation`)

By default a `Backend` runs **self impersonation**: the authorizer impersonates the
validated caller, and any inbound `Impersonate-*` header is denied fail-closed. An
optional **`spec.impersonation`** block (ADR-23 Revision 12) opts an **authorized
actor** into **delegated impersonation** — `kubectl --as <someone-else>` passthrough
— without the authorizer holding a per-user credential. It is additive: a `Backend`
omitting `spec.impersonation` is byte-for-byte the self-only behavior.

- **`spec.impersonation.groups[]`** — the actor allowlist. Delegated impersonation
  is permitted only when the actor's **mapped** Kubernetes groups (what the CEL /
  default mapping computes, not the raw claim) intersect this set. An
  omitted/empty list allowlists nothing (opt-in default).
- **`spec.impersonation.extra[]`** — actor-attribution headers stamped from the
  validated actor token as `Impersonate-Extra-<key>` (e.g. `actor-sub`,
  `actor-email`) in delegated mode only. They are **never client-settable**:
  every inbound `Impersonate-Extra-*` is denied in both modes. They may overlap
  `spec.oidc.extra` keys because `oidc.extra` is self-mode only and
  `impersonation.extra` is delegated-mode only.

The presence of an inbound non-extra `Impersonate-*` header is the
self-vs-delegated **mode switch**; an unauthorized actor (or a
nil-`spec.impersonation` Backend) is denied 403. Every inbound
`Impersonate-Extra-*` is denied before mode selection. In delegated mode the
actor's target passes through and the Backend-derived
`Impersonate-User`/groups/`Impersonate-Uid`/`spec.oidc.extra` are **not** emitted —
only `spec.impersonation.extra` is emitted from the actor token (the AC6 rule).
Impersonation-target
authorization is delegated to the **impersonator SA's `impersonate` RBAC on the
upstream API server**; the shipped default ClusterRole (above) is impersonate-only
on the two SA virtual groups and is **not** broadened — grant the impersonator SA
`impersonate` for the intended targets **per `Backend`**.

The rendered **`delegated-example`** `Backend` demonstrates the shape (a `groups`
allowlist plus `actor-*` `extra` mappings). The full operator procedure — the
`kubectl --as` flow, the audit-log distinction between impersonator SA / actor /
impersonated principal, and the required per-`Backend` impersonator RBAC including
`userextras/<key>` grants for `spec.impersonation.extra[]` — is in the
runbook's [*Delegated
impersonation*](../../../docs/runbooks/holos-authenticator.md#delegated-impersonation-kubectl---as-passthrough)
section.

## Tenant isolation (Backend is a platform-owned object)

A `Backend` is **platform-owned**, never tenant-self-service:

- The manager's cache (and the Backend reconciler's watch) is **scoped to the
  `holos-authenticator` namespace** (`cmd/holos-authenticator/main.go`,
  `Cache.DefaultNamespaces`). A `Backend` created in a tenant namespace is never
  cached, reconciled, served by the ext_authz path, or used for controller-side
  OIDC discovery — closing both the controller-side SSRF (a tenant-chosen
  `issuerURL`) and the privileged-token-injection vectors at the wiring layer.
- The impersonator credential always resolves from the `holos-authenticator`
  namespace (`AuthorizerNamespace()` / `POD_NAMESPACE`), never the Backend's
  namespace, so a `Backend` cannot reference a Secret a tenant controls.
- The platform namespace registry adds `holos-authenticator` to
  `#ReservedNamespaceNames` (`holos/namespaces.cue`), so the `projects` Argo CD
  AppProject denies tenant Applications this namespace as a destination — a
  tenant cannot deploy a `Backend` (or a Secret) into it.

The example `AuthorizationPolicy` (`action: CUSTOM`, `provider.name:
holos-authenticator`) selects the authenticator's own pods. When retargeting it
at a protected workload behind a waypoint, keep the protected workload and its
`Backend`/policy in platform-owned namespaces; do not expose the provider to
tenant-controlled `AuthorizationPolicy` resources.

## Apply ordering (render-here / apply-out-of-band)

The component is **registered in `holos/platform/platform.cue`** (so it renders
to the deploy tree) but is **DELIBERATELY EXCLUDED from both the master
`scripts/apply` `COMPONENTS` floor and the system App-of-Apps**
(`holos/components/app-of-apps/buildplan.cue` `SYSTEM_COMPONENTS`).

The manager Deployment pulls its image from the in-cluster Quay registry
(`quay.holos.internal/holos/holos-authenticator:dev`), which **does not exist on
a freshly bootstrapped cluster** until an operator publishes it *after* the
imperative bootstrap floor (the manual Quay-org/image-push setup `scripts/apply`
stops before). A bootstrap rollout gate — or an Argo CD child Application — would
therefore hang in `ImagePullBackOff`. This is exactly why the `holos-controller`
(also image-from-Quay) is excluded from the bootstrap apply; the authenticator
follows the same precedent.

It is **applied out of band** once its image is published — the deploy step the
next phase wires, alongside the impersonator credential Secret and any waypoint
topology. The `ext_authz` provider (in `istiod`'s `MeshConfig`) and the
ambient-enrolled namespace are both established earlier in the Istio data-plane
phase, so nothing in the bootstrap floor depends on this component.

> **CRD-before-CR within the directory.** The component bundles the Backend CRD
> **and** an example Backend CR in one directory. The out-of-band apply step must
> apply `customresourcedefinition-*.yaml` first and wait for it to be
> `Established` before the example `backend-*.yaml` (a plain
> `kubectl apply -f dir/` applies files lexically, and `backend-*.yaml` sorts
> before `customresourcedefinition-*.yaml`).

## Render workflow

After editing any `.cue` file under `holos/components/holos-authenticator/` (or
the registries above), commit the CUE change, then run `scripts/render` from the
repo root and commit the regenerated `holos/deploy/` tree — the *CUE Component
Rendering* guardrail.

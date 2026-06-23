# Runbook: Holos Authenticator — Istio gRPC ext_authz for OIDC → Kubernetes impersonation

The **Holos Authenticator** ([ADR-23](../adr/ADR-23.md)) is an Istio external
authorizer: an Envoy `ext_authz` gRPC server, hosted on a controller-runtime
manager, that lets an ambient-mesh **waypoint** (with no other reverse proxy in
the path) front one or more Kubernetes API servers — in-cluster or external —
authenticate end users by their **OIDC token**, map the token's claims to
Kubernetes groups, and forward the request as that user via **Kubernetes
impersonation**. Any conformant API server then authorizes the request with the
user's real groups.

This runbook is the operator's guide to the as-built service: the ext_authz
model, the `authenticator.holos.run` `Backend` CR, OIDC validation + CEL group
mapping, the impersonation RBAC the forwarded credential must hold, provisioning
the runtime credential Secret, the Istio `extensionProvider` +
`AuthorizationPolicy` wiring, and verification.

- Component: [`holos/components/holos-authenticator/`](../../holos/components/holos-authenticator/README.md)
- Design: [ADR-23](../adr/ADR-23.md)
- Deferred follow-ups: [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md)
  (*Holos Authenticator: L7 enforcement topology and tenant-policy guard*)

## The gRPC ext_authz model

The authenticator implements Envoy's `envoy.service.auth.v3.Authorization`
service. Envoy calls `Check` on every request routed through it; the authorizer
returns an `OkResponse` (optionally setting/overwriting headers the upstream
then sees) or a `DeniedResponse` (an HTTP status Envoy returns directly). The
server is **failure-closed**: every error path returns Denied, never OK.

On each `Check` the authorizer:

1. **Routes by `Host`.** It looks up the request's `:authority`/`Host` against
   the host-keyed `Backend` registry (`spec.host`). An unknown host is denied
   (HTTP 403).
2. **Sanitizes inbound impersonation headers (failure-closed).** If the inbound
   request carries any `Impersonate-*` header, the request is **denied** — not
   silently scrubbed. Because the authorizer injects a privileged impersonation
   credential downstream, a client that smuggled `Impersonate-Group:
   system:masters` would otherwise be impersonated at that privilege. This is
   the confused-deputy guard; it has explicit spoofed-header unit tests
   (`internal/authenticator/server_test.go`).
3. **Extracts the bearer token.** A missing `Authorization: Bearer …` yields a
   401 with a `WWW-Authenticate: Bearer` challenge.
4. **Validates the OIDC token** (below). An invalid token yields 401.
5. **Resolves the impersonator credential** from the Secret named by the
   Backend's `credentialsSecretRef`. An unavailable Secret yields 403.
6. **Returns the OK response** setting `Impersonate-User` (the username claim),
   one `Impersonate-Group` per mapped group, and overwriting `Authorization`
   with the impersonator credential's `Bearer <token>`. Envoy forwards to the
   upstream API server, which authorizes the request as the impersonated user.

Both the gRPC `Runnable` and the `Backend` reconciler report
`NeedLeaderElection() == false`: **every replica answers Envoy and reconciles
`Backend`s** into its own process-local registry. The reconciler runs on every
replica (not only the leader) precisely so each replica's in-memory registry is
populated and can serve the data path; leader election is not on the
authorization path at all.

Ports (the flag contract in `cmd/holos-authenticator/main.go`):

| Port    | Flag                          | Purpose                          |
| ------- | ----------------------------- | -------------------------------- |
| `:9000` | `--grpc-bind-address`         | Envoy ext_authz gRPC server      |
| `:8080` | `--metrics-bind-address`      | Prometheus metrics               |
| `:8081` | `--health-probe-bind-address` | `/healthz` + `/readyz`           |

## The `authenticator.holos.run` Backend CR

A **`Backend`** (`authenticator.holos.run/v1alpha1`, namespaced) models one
fronted API server: its routing host, the upstream API server, exactly **one**
OIDC client, an optional group-mapping CEL expression, and the impersonator
credential reference. **Multiple `Backend`s** are supported — one per fronted
API server — so a single deployment can serve several clusters, each with its
own OIDC client and mapping.

`spec` fields:

| Field                        | Required | Default                                    | Description                                                                 |
| ---------------------------- | -------- | ------------------------------------------ | --------------------------------------------------------------------------- |
| `host`                       | yes      | —                                          | The request `:authority`/`Host` value this Backend serves.                  |
| `server.url`                 | yes      | —                                          | Upstream API server endpoint (in-cluster or external).                      |
| `server.caBundle`            | no       | system trust                               | PEM x509 CA bundle to trust for the upstream (a privately-signed endpoint). |
| `oidc.issuerURL`             | yes      | —                                          | OIDC issuer base URL (issuer discovery + JWKS).                             |
| `oidc.clientID`              | yes      | —                                          | OAuth2 client ID / token audience (`aud`).                                  |
| `oidc.caBundle`              | no       | system trust                               | PEM x509 CA bundle to trust for the OIDC issuer.                            |
| `oidc.usernameClaim`         | no       | `sub`                                      | Token claim used as the impersonated username.                             |
| `oidc.groupsClaim`           | no       | `groups`                                   | Token claim carrying groups (used by the default mapping).                 |
| `groupMapping.celExpression` | no       | empty → default mapping                    | CEL expression over `claims` producing the Kubernetes group list.          |
| `credentialsSecretRef.name`  | no       | `holos-authenticator-backend-creds`        | Name of the Secret holding the privileged impersonator credential (resolved in the authorizer's own namespace). |
| `credentialsSecretRef.key`   | no       | `token`                                    | Secret key to read the raw bearer token from (the conventional `token` key when omitted). |

`Backend` reports the ADR-22 Gateway-API status contract: a
`status.conditions[]` of `Accepted`/`Programmed`/`Ready`, a
`status.observedGeneration`, and a `Ready` printer column.

> **`Backend` is a platform-owned object.** The manager's cache is scoped to the
> `holos-authenticator` namespace, the impersonator credential always resolves
> from that namespace (never the Backend's), and `holos-authenticator` is in
> `#ReservedNamespaceNames`, so a tenant cannot deploy a `Backend` (or a Secret)
> into it. Keep every protected workload, its `Backend`, and its policy in
> platform-owned namespaces.

### Example: in-cluster API server

```yaml
apiVersion: authenticator.holos.run/v1alpha1
kind: Backend
metadata:
  name: example
  namespace: holos-authenticator
spec:
  host: "api.example.holos.internal"
  oidc:
    issuerURL: "https://keycloak.holos.internal/realms/holos"
    clientID: "holos-authenticator"
  server:
    url: "https://kubernetes.default.svc"
  credentialsSecretRef:
    name: "holos-authenticator-backend-creds"
```

The `caBundle` fields are intentionally omitted so the committed manifest
carries no per-cluster trust material; an operator injects the local-ca PEM out
of band (mirroring the `caBundle` convention the project/application components
use). For the in-cluster API server, `caBundle` may carry the cluster CA so the
authorizer trusts `https://kubernetes.default.svc`.

### Example: external API server

```yaml
apiVersion: authenticator.holos.run/v1alpha1
kind: Backend
metadata:
  name: prod-east
  namespace: holos-authenticator
spec:
  host: "api.prod-east.example.com"
  oidc:
    issuerURL: "https://keycloak.holos.internal/realms/holos"
    clientID: "holos-authenticator"
  server:
    url: "https://prod-east.example.com:6443"
    caBundle: "<PEM x509 CA bundle for the external API server, injected at runtime>"
  groupMapping:
    celExpression: 'claims.groups.map(g, "oidc:" + g)'
  credentialsSecretRef:
    name: "prod-east-impersonator-creds"
```

For an **external** target the credential in `credentialsSecretRef` is the **raw
bearer token** of a principal that holds the impersonator ClusterRole on that
remote cluster, provisioned out-of-band. The authorizer reads the `token` key
verbatim and sends it as `Authorization: Bearer <token>`, so store the raw token
— **not** a kubeconfig blob (the authorizer does not parse one). The full
external-egress waypoint /
`ServiceEntry` topology that fronts an external API server is a **deferred
follow-up** ([`holos/docs/placeholders.md`](../../holos/docs/placeholders.md));
the in-cluster wiring is what ships today.

## OIDC token validation + CEL group mapping

**Validation.** The authorizer performs issuer discovery against
`oidc.issuerURL`, verifies the JWT signature against the issuer's JWKS, and
checks `iss`, `aud` (= `oidc.clientID`), and `exp`/`nbf`. A token that fails any
check is denied (401). The username is taken from `oidc.usernameClaim`
(default `sub`).

**Mapping.** The validated claims are exposed to CEL as a `claims` map, and the
backend's `groupMapping.celExpression` is evaluated against it to produce the
Kubernetes group list. When `celExpression` is **empty** (the default), the
authorizer uses the default expression `claims["<groupsClaim>"]` — i.e.
`claims["groups"]` for the default `groupsClaim`. So with no configuration a
token's `groups` claim maps directly to Kubernetes groups (the default
`groups`→groups mapping).

A backend may override the expression to derive groups from a different claim,
prefix them, or filter them. Examples (the syntax Kubernetes already uses for
admission/CRD validation):

- `claims["groups"]` — the default; the `groups` claim verbatim.
- `claims.groups.map(g, "oidc:" + g)` — prefix every group with `oidc:`.
- `claims.roles` — map a different claim (`roles`) to groups.

A token missing the mapped claim yields an empty group list (the user is
impersonated with no groups), not an error.

## Impersonation RBAC (the forwarded credential)

The credential named by `credentialsSecretRef` is the **impersonator identity**
the upstream API server authenticates Envoy as. It **must** hold RBAC granting
`impersonate` on `users` and `groups`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-authenticator-impersonator
rules:
  - apiGroups: [""]
    resources: ["users", "groups"]
    verbs: ["impersonate"]
```

Bind it to the principal whose token is stored in the credential Secret. If a
mapped group is a Kubernetes **system** group (e.g. anything under
`system:`), grant `impersonate` on the relevant subresource as needed — but
prefer mapping to non-`system:` groups so the blast radius of the impersonator
credential stays bounded.

- **In-cluster API server:** create a ServiceAccount, bind the impersonator
  ClusterRole to it, and store its token as the credential Secret.
- **External API server:** provision the credential out-of-band (the **raw
  bearer token** of a principal that holds the impersonator ClusterRole on that
  remote cluster) and store it under the `token` key of the Secret named by
  `credentialsSecretRef`. A kubeconfig blob is not parsed — store only the raw
  token.

Compromise of this credential lets an attacker impersonate arbitrary
users/groups on that backend's API server, so its handling, the ext_authz trust
boundary, and the failure-closed inbound-header sanitization are all
security-critical.

## Provisioning the credential Secret at runtime

Per the **Runtime Secret Handling** guardrail, the impersonator credential's
**material is never committed**. The component renders only the example
`Backend` CR (which *names* the Secret via `credentialsSecretRef`); the Secret
itself is created at runtime in the `holos-authenticator` namespace, out of
band.

The authorizer reads the credential as the `token` key. For an in-cluster
ServiceAccount whose ClusterRole grants impersonation:

```bash
# Mint a bound token for the impersonator ServiceAccount (in-cluster API server).
TOKEN=$(kubectl -n holos-authenticator create token holos-authenticator-impersonator)

kubectl -n holos-authenticator create secret generic holos-authenticator-backend-creds \
  --from-literal=token="$TOKEN"
```

For an external API server, store the out-of-band **raw bearer token** under the
same `token` key (the authorizer sends it as `Authorization: Bearer <token>` and
does not parse a kubeconfig). Write only the key(s) the authorizer reads — never
carry an extra key.

## Istio extensionProvider + AuthorizationPolicy wiring

`holos/components/istio/istio.cue` declares the gRPC `ext_authz` provider in
`IstioValues.meshConfig.extensionProviders`; the `istiod` component passes
`IstioValues` verbatim, so the provider lands in the mesh `MeshConfig`:

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

The `holos-authenticator` component renders an `AuthorizationPolicy` with
`action: CUSTOM` and `provider.name: holos-authenticator` that attaches the
authorizer to the selected workload:

```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: holos-authenticator
  namespace: holos-authenticator
spec:
  action: CUSTOM
  provider:
    name: holos-authenticator
  selector:
    matchLabels:
      app.kubernetes.io/name: holos-authenticator
  rules:
    - {}
```

The component **also** renders a second policy, `holos-authenticator-grpc-callers`
(`action: ALLOW`), an L4 caller guard on the ext_authz gRPC Service: it permits
calls to port `9000` only from the `holos-authenticator`, `istio-system`, and
`istio-gateways` namespaces (a temporary mitigation until the waypoint topology
lands):

```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: holos-authenticator-grpc-callers
  namespace: holos-authenticator
spec:
  action: ALLOW
  selector:
    matchLabels:
      app.kubernetes.io/name: holos-authenticator
  rules:
    - from:
        - source:
            namespaces: ["holos-authenticator", "istio-system", "istio-gateways"]
      to:
        - operation:
            ports: ["9000"]
```

> **A waypoint outside those namespaces cannot reach ext_authz.** If you place a
> waypoint (or Envoy caller) in a namespace other than `holos-authenticator`,
> `istio-system`, or `istio-gateways`, add that namespace to the
> `holos-authenticator-grpc-callers` `from.source.namespaces` list, or the gRPC
> `Check` call is denied at L4 before the authorizer ever runs.

> **L7 enforcement requires a waypoint.** In ambient mode ztunnel is L4-only, so
> the `CUSTOM` `AuthorizationPolicy` only takes effect once a **waypoint** fronts
> the protected workload. The example policy selects the authenticator's own
> pods as a harmless default; a real deployment retargets the selector at the
> protected workload behind a waypoint. The full waypoint / `ServiceEntry`
> egress topology for an **external** API-server target is deferred — see
> [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md).

## Apply ordering

The component is **registered in `holos/platform/platform.cue`** (so it renders
to the deploy tree) but is **deliberately excluded** from both the imperative
`scripts/apply` floor and the system App-of-Apps `SYSTEM_COMPONENTS`, exactly
like `holos-controller`: the manager Deployment pulls
`quay.holos.internal/holos/holos-authenticator:dev`, which does not exist on a
freshly bootstrapped cluster until an operator publishes it after the bootstrap
floor. It is **applied out of band** once its image is published.

> **CRD-before-CR within the directory.** The component bundles the `Backend`
> CRD **and** the example `Backend` CR in one directory. The out-of-band apply
> must apply `customresourcedefinition-*.yaml` first and wait for it to be
> `Established` before `backend-*.yaml` (a plain `kubectl apply -f dir/` applies
> files lexically, and `backend-*.yaml` sorts before
> `customresourcedefinition-*.yaml`).

## Building and publishing the image

The authenticator's build/release targets are isolated in
`Makefile.authenticator` (all `authenticator-*`), mirroring `holos-controller`:

```bash
make authenticator-build           # build the binary (gofmt + go vet gate)
make authenticator-test            # run tests with the race detector (envtest)
make authenticator-docker-push     # build + push the single-PLATFORM image
make authenticator-docker-buildx   # build + push the multi-arch image index
```

See [README.md](../../README.md) (*Container image* → *Multi-arch images* /
*Publishing images from CI*) for the multi-arch and CI publishing details (the
`holos-authenticator` option of the `images.yaml` workflow).

## Verification

1. **Manager is up.** Confirm the Deployment is rolled out and serving the
   health endpoint:

   ```bash
   kubectl -n holos-authenticator rollout status deploy/holos-authenticator
   kubectl -n holos-authenticator logs deploy/holos-authenticator | grep "starting manager"
   ```

2. **Backend is Ready.** The `Backend` CR reports `Ready=True` once its
   group-mapping CEL compiles, its `spec.server.url` validates as an absolute
   `http`/`https` URL with a host (an invalid URL is rejected `Ready=False` with
   reason `InvalidSpec`), its OIDC issuer discovery succeeds, and it has claimed
   its `host`. **The reconciler does not read the credential Secret** —
   the impersonator credential is resolved later, on the `Check` data path
   (failing closed with 403 if absent), so `Ready=True` is **not** a signal that
   the credential Secret exists. Provision the Secret (next section) regardless
   of the Backend's readiness:

   ```bash
   kubectl -n holos-authenticator get backends
   # NAME      HOST                          READY   AGE
   # example   api.example.holos.internal    True    1m
   ```

3. **Provider is in the mesh config.** Confirm the extension provider landed:

   ```bash
   kubectl -n istio-system get configmap istio -o yaml | grep -A4 extensionProviders
   ```

4. **End-to-end impersonation.** With a waypoint fronting the protected route
   and the `CUSTOM` `AuthorizationPolicy` attached, a request carrying a valid
   OIDC token reaches the API server impersonating the user with mapped groups.
   Confirm by inspecting the API server audit log (or a self-review):

   ```bash
   # A request with the user's bearer token, routed through the waypoint, should
   # land at the API server as the impersonated user with mapped groups.
   curl -sS -H "Authorization: Bearer $USER_OIDC_TOKEN" \
     -H "Host: api.example.holos.internal" \
     https://<waypoint-address>/apis/authentication.k8s.io/v1/selfsubjectreviews \
     -d '{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}'
   # The response's status.userInfo.username/groups reflect the impersonated
   # identity, not the impersonator ServiceAccount.
   ```

## Troubleshooting

- **All requests 403 with no token issue.** The request `Host` does not match
  any `Backend.spec.host`, or the `Backend` is in a namespace other than
  `holos-authenticator` (tenant Backends are never cached). Check
  `kubectl -n holos-authenticator get backends` and the request's `:authority`.
- **403 even with a valid token.** The impersonator credential Secret named by
  `credentialsSecretRef` is missing in the `holos-authenticator` namespace, or
  the principal it holds lacks `impersonate` on `users`/`groups`. Verify the
  Secret and the impersonator ClusterRole binding.
- **Request denied because it "carries impersonation headers."** This is the
  failure-closed sanitization (step 2): a client (or an upstream proxy) sent an
  `Impersonate-*` header. Ensure no proxy in front of the waypoint injects one;
  the authorizer refuses to forward a request that already carries them.
- **401 challenge on every request.** No `Authorization: Bearer …` reached the
  authorizer, or the token failed OIDC validation (issuer unreachable, wrong
  `aud`, expired). Check `oidc.issuerURL`/`oidc.clientID` and the issuer
  `caBundle`, and the manager logs for the validation error.
- **`AuthorizationPolicy` has no effect.** L7 ext_authz requires a **waypoint**
  in ambient mode; ztunnel is L4-only. Confirm a waypoint fronts the protected
  workload (see the deferred topology in
  [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md)).
- **The gRPC `Check` call is denied at L4.** The `holos-authenticator-grpc-callers`
  `ALLOW` policy permits port `9000` only from the `holos-authenticator`,
  `istio-system`, and `istio-gateways` namespaces. A waypoint/caller in any other
  namespace is rejected before the authorizer runs — add its namespace to that
  policy's `from.source.namespaces`.

## References

- [ADR-23](../adr/ADR-23.md) — the binding design record (`Implemented`).
- [`holos/components/holos-authenticator/README.md`](../../holos/components/holos-authenticator/README.md)
  — the component: what it renders, impersonation RBAC, tenant isolation, apply
  ordering.
- [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md) — deferred
  follow-ups: external-egress waypoint/`ServiceEntry` topology, token
  refresh/caching tuning, per-request CEL features beyond the default mapping,
  and the tenant-policy enforcement guard.
- [`holos/docs/mesh-enrollment.md`](../../holos/docs/mesh-enrollment.md) — the
  ambient-mesh enrollment convention for platform namespaces.

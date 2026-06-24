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
| `server.caBundle`            | no       | system trust                               | PEM x509 CA bundle for the upstream. **Not yet consumed** (see note below): the authorizer does not dial the upstream — Envoy/the waypoint forwards — so upstream TLS trust lives at the routing layer. The field is recorded for the deferred external-egress topology. |
| `oidc.issuerURL`             | yes      | —                                          | OIDC issuer base URL (issuer discovery + JWKS). When `oidc.jwks` is set it is the expected `iss` claim value only — no discovery is performed. |
| `oidc.clientID`              | yes      | —                                          | OAuth2 client ID / token audience (`aud`).                                  |
| `oidc.caBundle`              | no       | system trust                               | PEM x509 CA bundle to trust for the OIDC issuer. Unused when `oidc.jwks` is set (no issuer endpoint is dialed). |
| `oidc.jwks`                  | no       | empty → OIDC discovery                     | Static JSON Web Key Set (`{"keys":[…]}`, base64). When set, signatures are validated **offline** against these keys with no discovery/JWKS fetch; `issuerURL` becomes the expected `iss` only. See [*KSA / static-JWKS backends*](#ksa--static-jwks-backends). |
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
use).

> **`server.caBundle` is recorded but not yet consumed.** The authorizer copies
> `server.caBundle` into its in-memory `Backend` entry but **does not dial the
> upstream API server** — Envoy (the waypoint) forwards the impersonated request,
> so the upstream's TLS trust is the waypoint/`ServiceEntry` routing layer's
> concern, not the authorizer's. The field exists for the deferred external-egress
> topology (see [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md));
> setting it today has no effect on the request path. (`oidc.caBundle`, by
> contrast, **is** consumed — the authorizer dials the OIDC issuer directly for
> discovery/JWKS.)

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

**Validation.** The authorizer verifies the JWT signature, then checks `iss`
(= `oidc.issuerURL`), `aud` (= `oidc.clientID`), and `exp`/`nbf`. A token that
fails any check is denied (401). The username is taken from `oidc.usernameClaim`
(default `sub`). Signature verification has two modes depending on whether
`oidc.jwks` is set:

- **Discovery (default, `oidc.jwks` empty).** The authorizer performs issuer
  discovery against `oidc.issuerURL` and verifies the signature against the
  issuer's published JWKS, trusting the issuer endpoint with `oidc.caBundle`.
- **Static JWKS (`oidc.jwks` set).** The authorizer verifies the signature
  **offline** against the static key set in `oidc.jwks` — **no discovery and no
  JWKS HTTP fetch**. `oidc.issuerURL` is the expected `iss` value only (not
  dialed) and `oidc.caBundle` is unused. See [*KSA / static-JWKS
  backends*](#ksa--static-jwks-backends) below.

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

## KSA / static-JWKS backends

A `Backend` can validate tokens **offline** against a static JWKS rather than
doing OIDC discovery. This is how the authenticator accepts **service-account
(KSA) ID tokens minted by a remote cluster**: a workload on a remote cluster
presents its **projected service-account (KSA) ID token** (for example, an
[External Secrets Operator](https://external-secrets.io/) `SecretStore` token
request), and the authenticator validates it against the remote cluster's
published service-account JWKS — which the management cluster generally cannot
reach over the network — then impersonates the service account on the
**management** cluster's API server. The remote cluster is only the token
**issuer / JWKS source**; the impersonated request is forwarded to
`spec.server.url`, the **management** cluster's API server (`spec.host` is the
per-remote-cluster routing key, not the upstream).

Set `spec.oidc.jwks` and the authorizer performs **no OIDC discovery and no JWKS
HTTP fetch**: it verifies the token signature against the keys in `jwks`, treats
`spec.oidc.issuerURL` as the expected `iss` value only (not dialed), ignores
`spec.oidc.caBundle`, and still enforces `iss`/`aud`/`exp`/`nbf`. A malformed or
empty JWKS is rejected at reconcile time as an invalid spec (`Accepted=False`/
`Ready=False`, reason `InvalidSpec`).

> **Key selection matches the discovery path.** Static-JWKS validation currently
> uses the same global supported-algorithm set as discovery, with **no per-`kid`
> key selection or per-key algorithm enforcement** — built to parity with the
> discovery path. Tightening both paths together is the planned hardening tracked
> in HOL-1396.

The model is **1:1 host↔Backend**: each remote cluster gets its own `Backend`
with a **unique `spec.host`** (e.g. `remote-cluster-a.holos.internal`), its own
static JWKS, and its own impersonator credential for the management cluster. The
component renders `remote-cluster-a` as the worked example (its `jwks` is a
redacted placeholder).

### 1. Capture the remote cluster's issuer and JWKS

Run against the **remote** cluster (the one issuing the SA tokens):

```bash
# The issuer => spec.oidc.issuerURL (the expected `iss` claim value).
kubectl get --raw /.well-known/openid-configuration | jq -r .issuer

# The JWKS document => spec.oidc.jwks (holos/Kubernetes base64-encodes []byte).
kubectl get --raw /openid/v1/jwks
```

`spec.oidc.clientID` is the **audience** the remote `SecretStore`'s token request
asks for (the `aud` the SA token is minted with), matched against the token's
`aud`. The JWKS is non-secret public-key material and may live in the CR; per the
**Runtime Secret Handling** guardrail the `credentialsSecretRef` impersonator
token is created at runtime and **never** committed.

### 2. Populate the Backend

```yaml
apiVersion: authenticator.holos.run/v1alpha1
kind: Backend
metadata:
  name: remote-cluster-a
  namespace: holos-authenticator
spec:
  host: "remote-cluster-a.holos.internal"     # unique per remote cluster
  oidc:
    issuerURL: "https://kubernetes.default.svc" # expected `iss` only (not dialed)
    clientID: "holos-authenticator"             # the SA token's `aud`
    usernameClaim: "sub"                        # KSA sub = system:serviceaccount:<ns>:<name>
    jwks: "<the /openid/v1/jwks document, base64-encoded>"
  server:
    url: "https://kubernetes.default.svc"       # the MANAGEMENT cluster's API server
  groupMapping:
    celExpression: '["system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + claims["kubernetes.io"].namespace]'
  credentialsSecretRef:
    name: "remote-cluster-a-impersonator-creds"
```

### 3. The SA-group CEL expression

A KSA ID token's `sub` is `system:serviceaccount:<ns>:<name>`, so the default
`usernameClaim: sub` already reproduces the SA username for `Impersonate-User`.
Projected SA tokens also carry a `kubernetes.io` claim whose `namespace` field is
the SA's namespace. To reproduce the SA's three Kubernetes **virtual groups**,
set:

```
["system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + claims["kubernetes.io"].namespace]
```

For a token from namespace `app` (the namespace used in the RBAC and ESO
examples below) this yields
`["system:authenticated", "system:serviceaccounts", "system:serviceaccounts:app"]`
— exactly the groups the API server would assign the SA itself, so RBAC bound to
`system:serviceaccounts` or `system:serviceaccounts:app` authorizes the
impersonated request as the SA would be authorized. (The expression compiles and
evaluates under the authenticator's CEL environment; see the
`internal/authenticator/mapping_test.go` SA-virtual-groups case.)

### 4. Impersonation RBAC for the SA virtual groups

The `credentialsSecretRef` identity must hold `impersonate` on the impersonated
principal — both the SA **identity** and the SA **virtual groups** the CEL
expression emits. **A subtlety:** when `Impersonate-User` has the form
`system:serviceaccount:<ns>:<name>`, the Kubernetes API server authorizes the
impersonation against the **`serviceaccounts`** resource (in namespace `<ns>`,
name `<name>`) — **not** the `users` resource. Granting `impersonate` on `users`
with that value as a `resourceName` does **not** work and the request still 403s.
So scope the SA identity with a **namespaced `Role`** on `serviceaccounts`, and
the (cluster-scoped) virtual groups with a `ClusterRole` on `groups`. On the
**management** cluster:

```yaml
# 1. The SA identity: a namespaced Role in the SA's namespace (app), scoped to
#    the exact ServiceAccount, on the serviceaccounts resource.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: holos-authenticator-ksa-impersonator
  namespace: app            # the impersonated SA's namespace
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    verbs: ["impersonate"]
    resourceNames: ["eso-reader"]   # the exact remote SA served
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: holos-authenticator-ksa-impersonator
  namespace: app
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: holos-authenticator-ksa-impersonator
subjects:
  # The management-cluster ServiceAccount whose token is stored in
  # remote-cluster-a-impersonator-creds.
  - kind: ServiceAccount
    name: holos-authenticator-impersonator
    namespace: holos-authenticator
```

```yaml
# 2. The SA virtual groups: a ClusterRole on the groups resource, constrained
#    with resourceNames to exactly the groups the CEL expression emits.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-authenticator-ksa-impersonator
rules:
  - apiGroups: [""]
    resources: ["groups"]
    verbs: ["impersonate"]
    resourceNames:
      - "system:authenticated"
      - "system:serviceaccounts"
      - "system:serviceaccounts:app"   # one per remote namespace served
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: holos-authenticator-ksa-impersonator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: holos-authenticator-ksa-impersonator
subjects:
  - kind: ServiceAccount
    name: holos-authenticator-impersonator
    namespace: holos-authenticator
```

Scoping the `serviceaccounts` rule with `resourceNames` (and the namespaced
`Role`) and the `groups` rule with `resourceNames` keeps the impersonator's blast
radius bounded to the exact SA and SA virtual groups — never "all users" or "all
groups". This mirrors the existing [*Impersonation
RBAC*](#impersonation-rbac-the-forwarded-credential) shape; the KSA-specific
points are impersonating the SA via the `serviceaccounts` resource (not `users`)
and the SA-virtual-groups `resourceNames`. Provision the credential Secret at
runtime as in [*Provisioning the credential Secret at
runtime*](#provisioning-the-credential-secret-at-runtime), using a bound token
for the management-cluster impersonator ServiceAccount.

### 5. End-to-end verification (External Secrets Operator)

On the **remote** cluster, an ESO `SecretStore` (or `ClusterSecretStore`)
fetches from the management cluster's API server **through the authenticator's
host**, authenticating with a projected SA token whose audience is the Backend's
`clientID`:

```yaml
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: holos-management
  namespace: app
spec:
  provider:
    kubernetes:
      # The authenticator host for this remote cluster (routed by Host).
      server:
        url: "https://remote-cluster-a.holos.internal"
        # base64-encoded CA bundle trusting the authenticator/waypoint serving
        # cert (the ESO kubernetes provider's caBundle is base64, not raw PEM).
        caBundle: "<base64 CA bundle>"
      remoteNamespace: shared-secrets
      auth:
        serviceAccount:
          name: eso-reader            # SA on the remote cluster
          # The token audience must equal the Backend's spec.oidc.clientID; the
          # ESO kubernetes provider sets it under auth.serviceAccount.audiences.
          audiences: ["holos-authenticator"]
```

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: shared-config
  namespace: app
spec:
  secretStoreRef:
    name: holos-management
    kind: SecretStore
  target:
    name: shared-config
  data:
    - secretKey: value
      remoteRef:
        key: shared-config
        property: value
```

Impersonation only lets the authenticator's credential **act as** the SA; the
**impersonated** SA still needs its own RBAC on the management cluster to read the
secret. Grant the impersonated identity read access to Secrets in
`shared-secrets` — bind to the SA's virtual group (so any SA in `app` is covered)
or to the specific SA. On the **management** cluster:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: shared-secrets-reader
  namespace: shared-secrets
rules:
  # ESO's kubernetes provider needs get/list/watch on Secrets in the remote
  # namespace. resourceNames CANNOT restrict list/watch (RBAC only scopes named
  # resources for get/update/delete), so grant list/watch namespace-wide and
  # keep the namespace itself the boundary (a dedicated shared-secrets namespace).
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: shared-secrets-reader
  namespace: shared-secrets
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: shared-secrets-reader
subjects:
  # The impersonated SA virtual group (system:serviceaccounts:<ns>) — or bind to
  # the exact SA user system:serviceaccount:app:eso-reader for tighter scope.
  - kind: Group
    name: "system:serviceaccounts:app"
    apiGroup: rbac.authorization.k8s.io
```

> If ESO's SecretStore validation issues a `SelfSubjectRulesReview`, no extra
> grant is needed: `selfsubjectrulesreviews` is **cluster-scoped** and the
> default `system:basic-user` ClusterRoleBinding already authorizes every
> authenticated identity (including the impersonated SA) to `create` it — a
> namespaced `Role` could not authorize a non-namespaced request anyway. Keep the
> shared secrets in a **dedicated** namespace (`shared-secrets`) so the
> namespace-wide `list`/`watch` is the access boundary.

Verify:

```bash
# The Backend is Ready (offline validation, no discovery needed).
kubectl -n holos-authenticator get backend remote-cluster-a
# NAME               HOST                              READY   AGE
# remote-cluster-a   remote-cluster-a.holos.internal   True    1m

# On the remote cluster the ExternalSecret syncs (SecretSynced=True).
kubectl -n app get externalsecret shared-config
kubectl -n app get secret shared-config -o jsonpath='{.data.value}' | base64 -d
```

A failure to sync points at the SA token's `aud` not matching `clientID`, the
SA-group impersonation RBAC (step 4) missing on the management cluster, the
impersonated SA lacking **read** RBAC on the `shared-secrets` Secret (the
`shared-secrets-reader` Role above — impersonation grants the right to act as the
SA, not the SA's own read access), or the Backend's `jwks` not matching the remote
cluster's current signing keys (a key rotation on
the remote cluster requires re-capturing `/openid/v1/jwks` into `spec.oidc.jwks`).

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
> CRD **and** the example `Backend` CRs in one directory. The out-of-band apply
> must apply `customresourcedefinition-*.yaml` first and wait for it to be
> `Established` before `backend-*.yaml` (a plain `kubectl apply -f dir/` applies
> files lexically, and `backend-*.yaml` sorts before
> `customresourcedefinition-*.yaml`).

> **The rendered example Backends are placeholders — edit before applying.** Both
> `backend-example.yaml` (discovery) and `backend-remote-cluster-a.yaml`
> (static-JWKS) ship with **non-functional placeholder values**: `example` names a
> `credentialsSecretRef` Secret created out of band, and `remote-cluster-a`
> carries a **redacted placeholder `jwks`** that does not parse to a usable key —
> applied as-is it reconciles `Ready=False` (reason `InvalidSpec`). They document
> the shape; before the out-of-band apply, replace `remote-cluster-a`'s `jwks`
> with the real base64-encoded `/openid/v1/jwks` document (and its
> `host`/`issuerURL`/`clientID`) per [*KSA / static-JWKS
> backends*](#ksa--static-jwks-backends) above, or delete the example you are not
> using. Do not treat the placeholder `remote-cluster-a` as a ready-to-apply
> Backend.

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
   reason `InvalidSpec`), its verifier is built — OIDC issuer discovery succeeds
   for a discovery backend, or `spec.oidc.jwks` parses to at least one usable key
   for a static-JWKS backend (a malformed JWKS is rejected `Ready=False` with
   reason `InvalidSpec`) — and it has claimed its `host`. **The reconciler does
   not read the credential Secret** —
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

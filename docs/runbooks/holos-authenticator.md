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
mapping, **delegated impersonation** (`kubectl --as` passthrough via
`spec.impersonation` — the group allowlist, `extra` actor attribution, and
the impersonator RBAC it requires), the impersonation RBAC the forwarded credential
must hold, the two
mutually-exclusive credential sources — the controller-minted `serviceAccountRef`
(the shipped impersonate-only `holos-authenticator-impersonator` SA, TokenRequest
mint/cache/rotation) and the runtime `credentialsSecretRef` Secret — the Istio
`extensionProvider` + `AuthorizationPolicy` wiring, the **required** split Lua
filter that unpacks the comma-joined groups header into one `Impersonate-Group`
per group (ordered after ext_authz with `filterClass: AUTHZ`) plus the **optional**
reject Lua filter that rejects the configured groups header and inbound
`Impersonate-Extra-*` headers as defense in depth, and verification.

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
2. **Guards inbound impersonation headers (failure-closed).** A **copy of the
   configured groups header** (default `X-Impersonate-Groups`) and **every inbound
   `Impersonate-Extra-*` header** are **always denied** — the groups header because
   the split Lua filter would turn `X-Impersonate-Groups: system:masters` into a
   real `Impersonate-Group`, and extra headers because the authorizer derives
   extras from the validated token (`spec.oidc.extra` in self mode,
   `spec.impersonation.extra` in delegated mode), never from client input. Other
   inbound **`Impersonate-*` headers** (`Impersonate-User`/`-Group`/`-Uid`) are
   handled by **mode** ([*Delegated
   impersonation*](#delegated-impersonation-kubectl---as-passthrough) below): a
   Backend with `spec.impersonation` **nil** denies them fail-closed exactly as
   before (self mode only), and a Backend that opts in treats them as the
   **delegated-mode switch** — forwarding them only for an **authorized** actor
   (whose mapped groups are on the allowlist) and denying an unauthorized actor
   fail-closed. This is the confused-deputy guard; it has explicit spoofed-header
   unit tests
   (`internal/authenticator/server_test.go`). **Preventing header smuggling is the
   authenticator's responsibility, enforced server-side on every `Check`, so no
   Envoy `EnvoyFilter` is required to strip client-supplied impersonation headers.**
   The optional reject Lua filter (below) re-enforces the same guard at the proxy,
   before ext_authz, as defense in depth. On delegated routes it must preserve
   `Impersonate-User`/`Impersonate-Group`/`Impersonate-Uid` so the authorizer can
   adjudicate `kubectl --as`, while still rejecting the groups header and every
   inbound `Impersonate-Extra-*`.
3. **Extracts the bearer token.** A missing `Authorization: Bearer …` yields a
   401 with a `WWW-Authenticate: Bearer` challenge.
4. **Validates the OIDC token** (below). An invalid token yields 401.
5. **Resolves the impersonator credential.** If the Backend sets
   `serviceAccountRef`, the authorizer presents the cached/minted token of that
   ServiceAccount (minted/rotated via TokenRequest, [below](#provisioning-the-credential-serviceaccountref-or-a-runtime-secret));
   otherwise it reads the Secret named by `credentialsSecretRef`. An
   unavailable credential (missing Secret, or a TokenRequest failure) yields 403.
6. **Returns the OK response**, overwriting `Authorization` with the impersonator
   credential's `Bearer <token>` in **both** modes. Which impersonation headers it
   sets depends on the mode ([*Delegated
   impersonation*](#delegated-impersonation-kubectl---as-passthrough) below):
   - **Self mode** (no inbound `Impersonate-*` — the only OK path for a
     nil-`spec.impersonation` Backend, which otherwise denies an inbound
     `Impersonate-*` at step 2) — the **derived** identity: `Impersonate-User` (the
     username claim), the mapped groups as the single comma-joined groups header, and
     the derived `Impersonate-Uid`/`spec.oidc.extra`.
   - **Delegated mode** (an authorized actor sent an inbound `Impersonate-*`) — the
     **actor-supplied target** forwarded verbatim (`Impersonate-User`/
     `Impersonate-Uid`, and the actor's `--as-group` values re-emitted through the
     same comma-joined groups header), plus **only** `spec.impersonation.extra`
     resolved from the actor token; the actor-**derived**
     `Impersonate-User`/groups/`Impersonate-Uid`/`spec.oidc.extra` are **not**
     emitted (the AC6 rule). Inbound `Impersonate-Extra-*` is denied before this
     path, so delegated extras are never client-supplied.

   In either mode the **groups** are carried as a **single comma-joined groups
   header** (`oidc:dev,oidc:ops`). The groups header name defaults to
   `X-Impersonate-Groups` and is configurable with the `--impersonate-groups-header`
   flag (HOL-1416). Every header the authorizer emits uses the **overwrite/set**
   action (not append): an authorizer-returned **append** header is dropped by
   Envoy's ext_authz path unless the request *already* carries that header — and the
   authorizer never emits `Impersonate-Group` directly (in delegated mode the actor's
   inbound `Impersonate-Group` is removed and its values re-emitted through the
   comma-joined groups header instead) — so an appended `Impersonate-Group` would be
   silently discarded before reaching the API server. A **set** into a distinct,
   non-`Impersonate-*` header is added unconditionally (`setCopy`) and survives. The
   header is **not** what the API server consumes:
   the API server requires **one `Impersonate-Group` header per group** and does
   not split a comma list, so this must be paired with a Lua filter that unpacks
   the CSV into one `Impersonate-Group` per group (see [*Splitting the comma-joined
   groups header*](#splitting-the-comma-joined-groups-header) below). Envoy
   then forwards to the upstream API server, which authorizes the request as the
   impersonated user.

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
| `oidc.usernamePrefix`        | no       | empty → no prefix                          | Prefix prepended to the impersonated username (the apiserver `--oidc-username-prefix` equivalent; recommended `oidc:`). Always applied (the username is a direct claim read, not a CEL mapping). Intended to be set **as a pair** with `oidc.groupsPrefix` (both `oidc:` by convention), independent in the implementation. See [*OIDC token validation + CEL group mapping*](#oidc-token-validation--cel-group-mapping). |
| `oidc.groupsClaim`           | no       | `groups`                                   | Token claim carrying groups (used by the default mapping).                 |
| `oidc.groupsPrefix`          | no       | empty → no prefix                          | Prefix prepended to every group from the default mapping (the apiserver `--oidc-groups-prefix` equivalent; recommended `oidc:`). Honored only with the default mapping and **mutually exclusive** with `groupMapping.celExpression`. See [*OIDC token validation + CEL group mapping*](#oidc-token-validation--cel-group-mapping). |
| `oidc.uidClaim`              | no       | empty → no UID                             | Token claim emitted as `Impersonate-Uid` (recommended `sub`, a stable identifier for audit). When set, the claim must be a non-empty string on every token or the request is denied. See [*OIDC token validation + CEL group mapping*](#oidc-token-validation--cel-group-mapping). |
| `oidc.extra[].key`           | yes (per entry) | —                                   | Extra key, emitted as the `Impersonate-Extra-<key>` header suffix. Must be a **canonical** (lowercase, no `%`) HTTP header token so it round-trips through the API server's lowercase + percent-unescape; unique within a Backend (`listType=map`). |
| `oidc.extra[].valueClaim`    | yes (per entry) | —                                   | Token claim read for the extra value. Absent (or null) → entry skipped; present string → emitted (incl. empty); present-but-non-string → request denied. Single-valued in this phase. |
| `groupMapping.celExpression` | no       | empty → default mapping                    | CEL expression over `claims` producing the Kubernetes group list. Mutually exclusive with `oidc.groupsPrefix`. |
| `credentialsSecretRef.name`  | no       | `holos-authenticator-backend-creds`        | Name of the Secret holding the privileged impersonator credential (resolved in the authorizer's own namespace). Mutually exclusive with `serviceAccountRef`. |
| `credentialsSecretRef.key`   | no       | `token`                                    | Secret key to read the raw bearer token from (the conventional `token` key when omitted). |
| `serviceAccountRef.name`     | no       | `holos-authenticator-impersonator`         | Name of a ServiceAccount in the `holos-authenticator` namespace whose token the controller mints/rotates via TokenRequest as the impersonator credential. Mutually exclusive with `credentialsSecretRef`. See [*Provisioning the credential*](#provisioning-the-credential-serviceaccountref-or-a-runtime-secret). |
| `serviceAccountRef.audience` | no       | API server default audience                | Audience the minted SA token is requested with (empty → the API server's default audience). |
| `serviceAccountRef.expirationSeconds` | no | `3600` (min `600`)                      | Requested lifetime of the minted SA token; the controller rotates it before expiry. |
| `impersonation`              | no       | nil → self mode only                       | Opt-in to **delegated impersonation** (`kubectl --as` passthrough). Nil is byte-for-byte the self-only behavior (inbound `Impersonate-*` denied). See [*Delegated impersonation*](#delegated-impersonation-kubectl---as-passthrough). |
| `impersonation.groups[]`     | no       | empty → allowlists nothing                 | The actor allowlist: an actor may delegate-impersonate only when their **mapped** Kubernetes groups intersect this set (`listType=set`; each entry `MinLength=1`). Empty leaves delegated impersonation effectively disabled. |
| `impersonation.extra[].key` | yes (per entry) | —                               | Actor-attribution extra key, emitted as `Impersonate-Extra-<key>` from the validated **actor** token in delegated mode only. Canonical (lowercase, no `%`) and unique within `impersonation.extra` (`listType=map`). It may overlap `oidc.extra` because the two fields are emitted in different modes. Never client-settable: every inbound `Impersonate-Extra-*` is denied in both modes. |
| `impersonation.extra[].valueClaim` | yes (per entry) | —                        | Token claim read for the actor attribution value (same absent-skip / present-string-emit / non-string-deny semantics as `oidc.extra`). |

> **At most one credential source.** `credentialsSecretRef` and
> `serviceAccountRef` are **mutually exclusive** — a CRD-level CEL validation
> (`!(has(self.credentialsSecretRef) && has(self.serviceAccountRef))`) rejects a
> `Backend` that sets both. Set at most one; a `Backend` that sets neither
> resolves the default `credentialsSecretRef` Secret. This is the **outbound**
> impersonator credential the authorizer presents to `spec.server.url`, not the
> Rev 3 `oidc.jwks` **inbound** validation key set — the two SA-related features
> are independent.

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
(default `sub`); the optional `oidc.uidClaim` and `oidc.extra[]` add the
`Impersonate-Uid` and `Impersonate-Extra-<key>` dimensions (see *UID
(`oidc.uidClaim`) and extra fields* below). Signature verification has two modes
depending on whether `oidc.jwks` is set:

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

**Prefixing the default mapping (`oidc.groupsPrefix`).** Set `oidc.groupsPrefix`
to prepend a prefix to **every** group the default mapping produces — the
equivalent of the apiserver `--oidc-groups-prefix=oidc:` flag. With
`oidc.groupsClaim: groups` and `oidc.groupsPrefix: "oidc:"` a token's `dev`,
`ops` groups are impersonated as `oidc:dev`, `oidc:ops`. **The recommended value
is `oidc:`**, and it is a security recommendation: prefixing isolates the
external IdP's group namespace so a token cannot impersonate Kubernetes built-in
`system:` groups — a claim asserting `system:masters` becomes
`oidc:system:masters`, which holds no privilege, instead of the real
cluster-admin group. Without a prefix the groups claim is impersonated verbatim,
so an IdP able to mint an arbitrary `groups` claim could assert a `system:` group
directly. `oidc.groupsPrefix` is honored **only with the default mapping** and is
**mutually exclusive** with `groupMapping.celExpression` — a CEL expression
returns the final group set itself, so it must encode any prefix it wants. The
API server rejects a `Backend` that sets both (a CEL `XValidation` rule on
`BackendSpec`). Omit the field to prepend nothing; there is no default and an
explicit empty string is rejected.

**Prefixing the username (`oidc.usernamePrefix`).** Set `oidc.usernamePrefix` to
prepend a prefix to the impersonated username — the username-side companion to
`oidc.groupsPrefix` and the equivalent of the apiserver
`--oidc-username-prefix=oidc:` flag. With `oidc.usernameClaim: sub` and
`oidc.usernamePrefix: "oidc:"` a token whose `sub` is `alice` is impersonated as
the Kubernetes user `oidc:alice`. **The recommended value is `oidc:`**, the same
security rationale as the group prefix: it isolates the external IdP's username
namespace so a token cannot impersonate a Kubernetes built-in `system:` user — a
claim of `system:admin` becomes `oidc:system:admin`. Configure
`oidc.usernamePrefix` and `oidc.groupsPrefix` **as a pair**, both `oidc:`, to
match the upstream apiserver guidance that the two flags are set together; the
implementation keeps them independent, so a `Backend` may set one without the
other. Unlike `groupsPrefix`, the username prefix is **always applied** (the
username is a direct claim read, not a CEL mapping) and has **no** mutual-exclusion
with `groupMapping.celExpression`. Omit the field to prepend nothing; there is no
default and an explicit empty string is rejected.

**UID (`oidc.uidClaim`) and extra fields (`oidc.extra[]`).** Beyond the username
and groups, a backend can populate the other two Kubernetes impersonation
dimensions:

- **`oidc.uidClaim` → `Impersonate-Uid`.** Set it to the claim carrying a stable,
  non-reassignable identifier — **`sub` is the recommended value** — so audit logs
  and any UID-based policy track the original principal even if the human-friendly
  username (e.g. `email`) is later renamed or recycled. There is no default; omit
  the field to emit no UID. When set, the claim **must** be present and a non-empty
  string on every token, or the request is **denied (HTTP 401)** — a stable UID the
  operator asked for is never silently dropped.
- **`oidc.extra[]` → `Impersonate-Extra-<key>`.** A list of `{key, valueClaim}`
  entries; each emits the value of `valueClaim` as the `Impersonate-Extra-<key>`
  header so downstream authorizers and audit tooling can key off it (e.g. carry
  `email` or a tenant id). Keys are **unique** (the API server rejects duplicates)
  and must be **canonical** — lowercase HTTP header tokens with no `%` (the
  reconciler rejects a `Backend` with a bad key, `Accepted=False`) — so the emitted
  header round-trips through the API server's lowercase + percent-unescape to the
  same extra key. An entry whose claim is **absent** on a token is **skipped**
  (optional context); a **present string** is emitted verbatim (including an empty
  value); an entry whose claim is **present but not a string** **denies (HTTP 401)**
  — a misconfiguration pointing at a list/object claim. Each extra is single-valued
  in this phase.

Both are single values, so the authorizer sets `Impersonate-Uid` and each
`Impersonate-Extra-<key>` directly with the overwrite action, exactly like
`Impersonate-User` — **no comma-join + Lua split** (that is only for the
multi-valued groups header). No additional `EnvoyFilter` or flag is needed.

A backend may instead override the expression to derive groups from a different
claim, prefix them, or filter them. Examples (the syntax Kubernetes already uses
for admission/CRD validation):

- `claims["groups"]` — the default; the `groups` claim verbatim.
- `claims["groups"].map(g, "oidc:" + g)` — prefix every group with `oidc:`. This
  is the CEL equivalent of `oidc.groupsPrefix: "oidc:"`; prefer the first-class
  field for a plain prefix and reserve the CEL form for prefixing combined with
  other transforms.
- `claims.roles` — map a different claim (`roles`) to groups.

A token missing the mapped claim yields an empty group list (the user is
impersonated with no groups), not an error — and this holds with a prefix set:
the `.map(g, "<prefix>" + g)` form preserves the missing-claim → no-groups
behavior.

### Example: default mapping with the username + group prefix pair

```yaml
apiVersion: authenticator.holos.run/v1alpha1
kind: Backend
metadata:
  name: prefixed
  namespace: holos-authenticator
spec:
  host: "api.prefixed.holos.internal"
  oidc:
    issuerURL: "https://keycloak.holos.internal/realms/holos"
    clientID: "holos-authenticator"
    usernamePrefix: "oidc:"
    groupsPrefix: "oidc:"
  server:
    url: "https://kubernetes.default.svc"
  credentialsSecretRef:
    name: "holos-authenticator-backend-creds"
```

### Example: stable UID from `sub` and an `email` extra field

The motivating case (HOL-1419): humans sign in via Keycloak with their employee
id as the username, but audit and policy key off the stable `sub` as the UID and
carry `email` as an extra field.

```yaml
apiVersion: authenticator.holos.run/v1alpha1
kind: Backend
metadata:
  name: kubectl-humans
  namespace: holos-authenticator
spec:
  host: "api.holos.internal"
  oidc:
    issuerURL: "https://keycloak.holos.internal/realms/holos"
    clientID: "holos-authenticator"
    usernameClaim: "preferred_username"
    usernamePrefix: "oidc:"
    groupsPrefix: "oidc:"
    uidClaim: "sub"            # -> Impersonate-Uid
    extra:                     # -> Impersonate-Extra-<key>
      - key: "email"
        valueClaim: "email"
  server:
    url: "https://kubernetes.default.svc"
  credentialsSecretRef:
    name: "holos-authenticator-backend-creds"
```

A token with `preferred_username: alice`, `sub: 1a2b`, and
`email: alice@example.com` is then impersonated as the Kubernetes user
`oidc:alice` with `Impersonate-Uid: 1a2b` and `Impersonate-Extra-email:
alice@example.com`. Because this Backend emits the UID and an extra, its
impersonator credential additionally needs `impersonate` on the
`authentication.k8s.io` `uids` and `userextras/email` resources — see
[*Impersonation RBAC (the forwarded
credential)*](#impersonation-rbac-the-forwarded-credential).

With this `Backend` a token whose `sub` is `alice` and whose `groups` claim is
`["dev", "ops"]` is impersonated as the Kubernetes user `oidc:alice` with the
groups `oidc:dev` and `oidc:ops` — `usernamePrefix` and `groupsPrefix` set as a
pair, the recommended configuration. Do **not** also set
`groupMapping.celExpression` — it is mutually exclusive with `groupsPrefix` and the
API server rejects a `Backend` that sets both (`usernamePrefix` carries no such
restriction).

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
`aud`. The JWKS is non-secret public-key material and may live in the CR. The
**outbound** impersonator credential is separate from this inbound validation: for
an in-cluster management API server the rendered example uses `serviceAccountRef`
(the controller mints/rotates the token, no Secret); if you instead use
`credentialsSecretRef`, that impersonator token is created at runtime and **never**
committed per the **Runtime Secret Handling** guardrail.

### 2. Populate the Backend

This mirrors the rendered `remote-cluster-a` example: `oidc.jwks` validates the
**inbound** remote SA token offline, and `serviceAccountRef` supplies the
**outbound** management-cluster impersonator credential (the controller mints and
rotates the `holos-authenticator-impersonator` SA's token — no Secret to create):

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
  serviceAccountRef: {}                         # mint/rotate the default impersonator SA token
```

> For an **external** management API server (the cluster cannot mint a token for a
> remote SA), replace `serviceAccountRef: {}` with a `credentialsSecretRef` naming
> a runtime Secret that holds the out-of-band impersonator token — the two are
> mutually exclusive. The impersonation RBAC in step 4 is identical either way;
> it applies to whichever impersonator identity the Backend uses.

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

The impersonator identity (the `serviceAccountRef` SA — `holos-authenticator-impersonator`
by default — or a `credentialsSecretRef` token for an external management cluster)
must hold `impersonate` on the impersonated
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
  # The management-cluster impersonator ServiceAccount the Backend's
  # serviceAccountRef names (holos-authenticator-impersonator by default; the
  # controller mints/rotates its token). For a credentialsSecretRef Backend,
  # bind the principal whose token is stored in that Secret instead.
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
and the SA-virtual-groups `resourceNames`. Provision the management-cluster
impersonator credential as in [*Provisioning the
credential*](#provisioning-the-credential-serviceaccountref-or-a-runtime-secret)
— reference the impersonator SA via `serviceAccountRef` (the controller mints and
rotates its token), or store a bound token in a runtime Secret via
`credentialsSecretRef`.

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

## Delegated impersonation (`kubectl --as` passthrough)

By default a `Backend` runs **self impersonation**: the authorizer validates the
caller's OIDC token and impersonates *that caller*, and any inbound `Impersonate-*`
header is denied fail-closed. **Delegated impersonation** (ADR-23 Revision 12) lets
a Backend opt an **authorized actor** into forwarding an actor-specified target —
exactly like `kubectl --as <someone-else>` — without the authorizer holding a
per-user credential. It is entirely opt-in: a Backend with `spec.impersonation`
nil is byte-for-byte unchanged (self mode only).

### Enabling it

Add a `spec.impersonation` block with two fields:

```yaml
spec:
  # ... host / server / oidc / credential ...
  impersonation:
    # The actor allowlist: an actor may impersonate a target only when their
    # MAPPED Kubernetes groups (what the CEL/default mapping computes, not the raw
    # claim) intersect this set. Omitted/empty allowlists nothing (opt-in default).
    groups:
      - "oidc:platform-admins"
    # Actor-attribution headers, stamped from the validated actor token as
    # Impersonate-Extra-<key> in delegated mode only. Keys may overlap
    # spec.oidc.extra because oidc.extra is self-mode only.
    extra:
      - key: "actor-sub"
        valueClaim: "sub"
      - key: "actor-email"
        valueClaim: "email"
      - key: "actor-preferred_username"
        valueClaim: "preferred_username"
```

### How a delegated request flows

- **The mode switch is the presence of an inbound non-extra `Impersonate-*`
  header.** No inbound `Impersonate-User`/`Impersonate-Group`/`Impersonate-Uid` →
  **self mode** (the caller's derived identity, including `spec.oidc.extra`).
  An inbound non-extra impersonation header → **delegated mode**. Every inbound
  `Impersonate-Extra-*` is denied fail-closed before mode selection.
- **The group allowlist gates it against *mapped* groups.** The actor's token is
  validated and mapped to Kubernetes groups (the default groups-claim mapping or
  `spec.groupMapping.celExpression`); delegated mode is authorized only if those
  mapped groups intersect `spec.impersonation.groups`. An unauthorized actor — or a
  Backend with `spec.impersonation` nil — is denied **403** fail-closed (this is the
  same denial that protected self-only backends before Revision 11).
- **`extra` identifies the actor in delegated mode.** The authorizer resolves each
  `spec.impersonation.extra` claim from the validated actor token and emits it as
  `Impersonate-Extra-<key>` only after delegated mode is authorized. Self mode
  emits `spec.oidc.extra` only. The two fields are independent, so overlapping
  keys are legal. Every inbound `Impersonate-Extra-*` is rejected fail-closed in
  **both** modes.
- **AC6 — the delegated target replaces the derived identity.**
  In delegated mode the authorizer forwards the actor-supplied
  `Impersonate-User`/`Impersonate-Uid` **verbatim** (and the actor's `--as-group`
  values re-emitted through the comma-joined groups header per the
  split-then-re-emit above) and does **not** emit the derived
  `Impersonate-User`/groups/`Impersonate-Uid`/`spec.oidc.extra`. The only
  Backend-derived impersonation headers in delegated mode are
  `spec.impersonation.extra`.
- **A delegated request must name a target user.** Kubernetes rejects impersonation
  that sets only groups/UID/extras with no `Impersonate-User`, so a groups-only
  delegated request is denied **403** rather than forwarded with the bare
  impersonator credential.
- **Groups passthrough already round-trips.** The actor's `--as-group` values arrive
  Envoy-comma-joined as one `Impersonate-Group: a,b`; the authorizer **splits that
  value on commas** into the individual groups and re-emits them through the
  configured groups header (`s.groupsHeaderName()`, default `x-impersonate-groups`),
  so **the paired split Lua filter unpacks them into `Impersonate-Group` lines
  exactly as in self mode** — no extra wiring. Because the inbound value is split on
  commas first, a comma is interpreted as a **group separator** (so `dev,ops` is two
  groups, not denied), and the `firstUnsafeGroup` guard then applies to each split
  element and denies **403** only a **surrounding-whitespace** element (a leading/
  trailing space the split filter would trim into a different group). A single group
  name that itself contains a literal comma therefore cannot be represented on this
  Envoy-comma-joined path — it is indistinguishable from two groups — so it is
  unsupported by design.

### The operator flow (`kubectl --as`)

An authorized operator impersonates a target through the authenticator's host with
the standard `kubectl --as` / `--as-group` flags, presenting their **own** OIDC
token:

```bash
# The operator's own token authenticates them as the ACTOR; --as/--as-group name
# the target. The authorizer forwards the target (allowlist permitting) and stamps
# spec.impersonation.extra as Impersonate-Extra-<key> from the operator's token.
kubectl --server https://api.example.holos.internal \
  --token "$ACTOR_OIDC_TOKEN" \
  --as alice --as-group dev --as-group ops \
  get pods -n some-namespace
```

### The audit log distinguishes three identities

When `spec.impersonation.extra` is configured, the upstream API server's audit log
can record **three** distinct identities, so an auditor can attribute the action
precisely:

- the **impersonator ServiceAccount** — the authenticated identity of the request
  Envoy makes (`user.username` = the `serviceAccountRef`/`credentialsSecretRef` SA,
  e.g. `system:serviceaccount:holos-authenticator:holos-authenticator-impersonator`);
- the **impersonated principal** — the target the actor named
  (`impersonatedUser.username`/`groups`, e.g. `alice` with `dev`/`ops`); and
- the **actor** — recorded in the `Impersonate-Extra-<key>` headers generated from
  `spec.impersonation.extra`, surfaced in the audit event's
  `impersonatedUser.extra` map (for example, `actor-sub`, `actor-email`) so the
  real human who ran `kubectl --as` is never conflated with either the
  impersonator SA or the impersonated target.

Configure `spec.impersonation.extra` whenever enabling delegated impersonation. If
it is omitted, the upstream API server's audit log sees only the impersonator
ServiceAccount and the target principal; the authorizer's Info-level delegated
allow/deny decision log is then the remaining actor record.

### Required: the impersonator SA needs `impersonate` RBAC for the target

**Delegated impersonation does not widen the impersonator's RBAC** — the authorizer
allows the *request* (the actor is on the allowlist) but the **API server** still
authorizes the *impersonation* against the impersonator ServiceAccount's RBAC.
Target authorization is **delegated to that RBAC**: there is no target allowlist in
the authorizer. The shipped default `holos-authenticator-impersonator` ClusterRole
is **impersonate-only on the two SA virtual groups** and is **not broadened** for
delegated impersonation (consistent with ADR-23 Revision 4). So the impersonator SA
**must be granted `impersonate` for the intended target users/groups**, applied
**per `Backend`**, exactly as in [*Adding per-Backend impersonate
scope*](#adding-per-backend-impersonate-scope) below. For a human-target Backend
mapping to non-`system:` groups, that means `impersonate` on `users` (scoped by
`resourceNames` to the expected usernames where practical) and on `groups` (scoped
to the mapped group names); add `uids` grants if the actor may supply
`Impersonate-Uid`, and add `userextras/<key>` grants for each
`spec.impersonation.extra[]` key the Backend emits as actor attribution. Without
these grants a delegated request the authorizer **allows** is then **rejected 403
by the API server**. Never widen the shipped default ClusterRole to satisfy this.

## Impersonation RBAC (the forwarded credential)

The impersonator credential — whether `serviceAccountRef` or
`credentialsSecretRef` supplies it — is the **impersonator identity** the
upstream API server authenticates Envoy as. It **must** hold RBAC granting the
`impersonate` verb on whatever identity the CEL mapping emits (in delegated mode,
on whatever target an authorized actor may name — see [*Delegated
impersonation*](#delegated-impersonation-kubectl---as-passthrough) above).

### The shipped default impersonator SA and its scoped ClusterRole

The component ships a dedicated **`holos-authenticator-impersonator`**
ServiceAccount (the `serviceAccountRef.name` default) in the
`holos-authenticator` namespace — **distinct from the manager's own
`holos-authenticator` SA** — bound to a deliberately **narrow** impersonate-only
ClusterRole. The shipped default grants `impersonate` on **`groups` only**,
scoped with `resourceNames` to exactly the two namespace-independent
service-account **virtual groups**:

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

> **The shipped default is impersonate-only and bounded by design (ADR-23 Rev
> 4).** It grants **nothing** on `users` or `serviceaccounts`, and only the two
> always-present SA virtual groups on `groups`. An **unbounded** `impersonate` on
> `users`/`groups` (no `resourceNames`) is a cluster-wide privilege-escalation
> credential — it can impersonate *any* user or group, including
> `system:masters`. The parent issue's literal AC asked for `users`/`groups`/
> `serviceaccounts`, but the as-shipped default was narrowed to the SA virtual
> groups for security; **ADR-23 Revision 4 ratifies this scoping**. Grant any
> additional per-identity / per-namespace impersonate scope **per `Backend`**,
> never by widening this default ClusterRole.

### Adding per-Backend impersonate scope

A real `Backend` almost always impersonates **more** than the two default
virtual groups — a specific user, a per-namespace `system:serviceaccounts:<ns>`
group, or a specific ServiceAccount. Add those grants alongside the default,
scoped to exactly what the Backend's CEL mapping emits. The worked example for a
KSA Backend (the SA identity on the `serviceaccounts` resource, plus the
per-namespace SA virtual group) is in [*Impersonation RBAC for the SA virtual
groups*](#4-impersonation-rbac-for-the-sa-virtual-groups). For a human-OIDC
Backend mapping to non-`system:` groups, grant `impersonate` on `users`
(scoped by `resourceNames` to the expected usernames where practical) and on
`groups` (scoped to the mapped group names). Prefer mapping to non-`system:`
groups so the blast radius stays bounded.

> **A Backend that emits `Impersonate-Uid` or `Impersonate-Extra-*` needs `uids`
> / `userextras/<key>` grants too.** The API server authorizes
> `Impersonate-Uid` against the **`uids`** resource and
> `Impersonate-Extra-<key>` against the **`userextras/<key>`** subresource, **both
> in the `authentication.k8s.io` API group** — separately from `users`/`groups`.
> This applies to `oidc.uidClaim` and `oidc.extra[]` in self mode, and to
> `spec.impersonation.extra[]` in delegated mode. Without these grants a request
> the authorizer **allows** (it emitted the headers) is then **rejected 403 by the
> API server**. Add one rule per emitted UID/extra dimension, scoped with
> `resourceNames` where practical (the `uids` resource generally cannot be usefully
> name-scoped since the UID is per-principal). Append to the impersonator's role:
>
> ```yaml
> rules:
>   # Impersonate-Uid (oidc.uidClaim)
>   - apiGroups: ["authentication.k8s.io"]
>     resources: ["uids"]
>     verbs: ["impersonate"]
>   # Impersonate-Extra-<key> — one subresource per emitted extra key
>   # (oidc.extra[].key in self mode, impersonation.extra[].key in delegated mode)
>   - apiGroups: ["authentication.k8s.io"]
>     resources: ["userextras/email"]
>     verbs: ["impersonate"]
>     resourceNames: ["alice@example.com"]   # scope to expected values where practical
> ```

Compromise of this credential lets an attacker impersonate whatever the granted
RBAC allows on that backend's API server, so keeping each grant scoped (never an
unbounded `users`/`groups` impersonate), the ext_authz trust boundary, and the
failure-closed inbound-header sanitization are all security-critical.

## Provisioning the credential: `serviceAccountRef` or a runtime Secret

A `Backend` gets its outbound impersonator credential one of two ways. They are
**mutually exclusive** (the CRD's CEL validation rejects setting both); pick the
one that fits the upstream.

### Option A — `serviceAccountRef` (controller mints the token, in-cluster)

For an **in-cluster** management API server, reference a ServiceAccount and let
the controller mint and rotate its token — **no manual `kubectl create token`,
no Secret to create**:

```yaml
spec:
  # ... host / server / oidc ...
  serviceAccountRef: {}     # all defaults: name holos-authenticator-impersonator,
                            # API-server default audience, expirationSeconds 3600
```

- `serviceAccountRef.name` defaults to the shipped
  **`holos-authenticator-impersonator`** SA; set it to your own SA in the
  `holos-authenticator` namespace to use a different (e.g. more broadly-scoped
  per the section above) impersonator identity.
- The authorizer mints the SA's bearer token by `create` on the
  `serviceaccounts/token` subresource — the manager's namespaced `Role` grants
  exactly that, scoped by `resourceNames` to `holos-authenticator-impersonator`
  (widen the grant if you point `serviceAccountRef.name` at a different SA).
- **Caching and rotation.** The minted token is cached keyed by **name +
  audience + expirationSeconds** (Backends naming the same SA/audience/lifetime
  share one cached token) and **rotated before expiry** with a margin of the
  **smaller of 5 minutes or 20% of the token's lifetime**. Tokens are minted
  **without** a `BoundObjectRef` (exactly like `kubectl create token`). The
  default `expirationSeconds` is `3600` (minimum `600`).
- The impersonator SA still needs the impersonation RBAC above (the shipped
  `holos-authenticator-impersonator` SA already has the bounded default; add
  per-`Backend` scope as needed).

### Option B — `credentialsSecretRef` (runtime Secret)

For an **external** API server (the management cluster cannot mint a token for a
remote cluster's SA), or any case where the credential is an out-of-band token,
use `credentialsSecretRef`. Per the **Runtime Secret Handling** guardrail the
credential's **material is never committed**; the Secret is created at runtime in
the `holos-authenticator` namespace, out of band. The authorizer reads the
`token` key:

```bash
# In-cluster: a bound token for an impersonator ServiceAccount.
TOKEN=$(kubectl -n holos-authenticator create token holos-authenticator-impersonator)

kubectl -n holos-authenticator create secret generic holos-authenticator-backend-creds \
  --from-literal=token="$TOKEN"
```

For an external API server, store the out-of-band **raw bearer token** under the
same `token` key (the authorizer sends it as `Authorization: Bearer <token>` and
does not parse a kubeconfig). Write only the key(s) the authorizer reads — never
carry an extra key.

> **`serviceAccountRef` vs. the Rev 3 `oidc.jwks` (don't conflate the two
> SA-related features).** `serviceAccountRef` is the **outbound** impersonator
> credential — *whom the authorizer authenticates as* to `spec.server.url`. The
> Rev 3 `oidc.jwks` / KSA path (below) is **inbound** — *which remote-cluster SA
> token the authorizer accepts and validates*. A KSA Backend commonly uses
> **both**: `oidc.jwks` to validate the inbound remote SA token, and
> `serviceAccountRef` (or `credentialsSecretRef`) for the outbound
> management-cluster impersonator credential.

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

## Splitting the comma-joined groups header

The authorizer returns the mapped groups as a **single comma-joined value** under
the configured groups header (default `X-Impersonate-Groups`, set with the
`--impersonate-groups-header` flag), e.g.

```text
X-Impersonate-Groups: oidc:dev,oidc:ops
```

with the **overwrite/set** action (HOL-1416). It deliberately does **not** emit
per-group `Impersonate-Group` **append** options: Envoy's ext_authz path classifies
an authorizer-returned `append: true` header into the `headers_to_append` bucket,
which it applies with `appendCopy` **only if the request already carries that
header** — and the inbound request never carries `Impersonate-Group` (it is rejected
fail-closed), so an appended `Impersonate-Group` is **silently dropped** before it
reaches the split filter or the API server (the original symptom: `Impersonate-User`
present, every group missing). A **set** header (`headers_to_set` → `setCopy`) is
added unconditionally, even when the request does not already carry it, so the
comma-joined groups header survives; routing it through a **distinct,
non-`Impersonate-*` name** also keeps it clear of the inbound-rejection guard that
denies `Impersonate-*`.

This is **not** the value the API server ultimately needs: the Kubernetes API
server's impersonation feature expects **one `Impersonate-Group` header per
group** and treats a comma-separated value as a **single literal group name**
(`"oidc:dev,oidc:ops"`), so left as-is the user would be impersonated into a
non-existent group and lose their real group memberships. A **split** Lua filter
that runs **after** ext_authz completes the round-trip, and it is the **only**
filter required on the request path. An optional **reject** filter before ext_authz
adds defense in depth but is **not** required: preventing header smuggling is the
authenticator's own responsibility (it denies any request carrying impersonation
headers server-side, step 2), so the proxy needs no filter for that purpose. The
split filter is a header-*shape* adaptation (CSV → one header per group), not a
security control. Where each filter must sit **relative to the ext_authz callout**,
and the version-stable `filterClass: AUTHZ` way to express it, are covered in
[*Filter ordering relative to the ext_authz chain*](#filter-ordering-relative-to-the-ext_authz-chain-filterclass-authz)
below.

> **Group values must contain no comma and no surrounding whitespace — the
> authorizer enforces this.** The comma-join + split round-trip is only lossless if
> no single group value contains a comma **or** has leading/trailing whitespace.
> `dev,system:masters` would be split into two impersonated groups (smuggling
> `system:masters`), and ` system:masters` would be **trimmed** by the split filter
> into the bare `system:masters` — both privilege-escalation vectors. The authorizer
> therefore **denies (HTTP 403, fail-closed) any request whose mapped groups include
> a comma or surrounding whitespace** (`internal/authenticator/server.go`,
> `firstUnsafeGroup`), so the Lua split below can never fan one group into many or
> normalize a padded value into a privileged one. Whitespace *interior* to a value
> is left intact (the filter only strips surrounding whitespace). The username is set
> with a single overwrite header (not comma-joined, not split) and needs no such
> guard. Do not weaken the guard or the filter's trim independently — they are a
> matched pair; changing one without the other reopens the smuggling vector.

### Rejecting inbound impersonation headers (optional, before ext_authz)

Because the split filter turns the groups header into `Impersonate-Group` lines the
API server trusts, a client must never be able to **supply** the configured groups
header itself. The authorizer **already** denies such requests server-side (step 2)
— that, not this filter, is the smuggling defense — so this Lua **reject** filter is
**optional** and **not required** for the security property. It runs **before**
ext_authz and rejects the subset of headers that are invalid in both self and
delegated mode as proxy-level defense in depth, returning HTTP 403 before any
header mutation. This delegated-compatible form refuses the configured groups
header (substitute the value of `--impersonate-groups-header`; the default
`x-impersonate-groups` is shown) **and** every inbound `Impersonate-Extra-*`
header on the incoming request:

> **Delegated-mode caveat.** Do not reject `Impersonate-User`,
> `Impersonate-Group`, or `Impersonate-Uid` at the proxy on a route that uses
> delegated impersonation; those are the legitimate `kubectl --as` target headers
> the authorizer adjudicates. It is safe, and recommended defense in depth, to
> reject the configured groups header and the `Impersonate-Extra-` prefix because
> the authorizer denies those inbound headers fail-closed in both modes. The split
> filter below is unaffected — deploy it as usual.
>
> On a route that will never enable delegated impersonation, an operator may use a
> stricter self-mode variant that rejects the whole `impersonate-` prefix at the
> proxy, mirroring the self-mode authorizer guard. Do not use that stricter variant
> on delegated routes.

```lua
function envoy_on_request(handle)
  local headers = handle:headers()
  -- The configured groups header (default x-impersonate-groups). Header names are
  -- matched case-insensitively; Envoy presents request header keys in lowercase.
  if headers:get("x-impersonate-groups") ~= nil then
    handle:respond({[":status"] = "403"}, "client-supplied impersonation header not allowed")
    return
  end
  -- Every inbound Kubernetes impersonation extra header. Envoy's Lua header
  -- iteration must run to completion — do not break or call handle:respond()
  -- inside the loop — so record a flag and respond after the loop finishes.
  local denied = false
  for key, _ in pairs(headers) do
    if string.sub(string.lower(key), 1, 18) == "impersonate-extra-" then
      denied = true
    end
  end
  if denied then
    handle:respond({[":status"] = "403"}, "client-supplied impersonation header not allowed")
  end
end
```

> The `impersonate-extra-` prefix check covers every `Impersonate-Extra-*` header
> while preserving `Impersonate-User`, `Impersonate-Group`, and `Impersonate-Uid`
> for delegated-mode evaluation by the authorizer. If you configure a non-default
> groups header,
> reject **that** name here instead of (or in addition to) `x-impersonate-groups` —
> the reject and split filters must name the same header.

### Splitting the groups header into one `Impersonate-Group` per group (after ext_authz)

The **split** filter runs **after** ext_authz (so it sees the authorizer's injected
groups header) and **before** the request egresses to the API server. It reads the
comma-joined groups header, removes it, and re-adds one `Impersonate-Group` header
per element:

```lua
function envoy_on_request(handle)
  -- Must match the authorizer's --impersonate-groups-header (default
  -- x-impersonate-groups). The authorizer rejects any client-supplied copy
  -- server-side, so the only copy on the request is the one the authorizer set
  -- (the optional reject filter re-enforces this at the proxy).
  local joined = handle:headers():get("x-impersonate-groups")
  if joined == nil or joined == "" then
    return
  end
  handle:headers():remove("x-impersonate-groups")
  for group in string.gmatch(joined, "([^,]+)") do
    -- trim surrounding whitespace, then add one Impersonate-Group header per group
    local g = group:gsub("^%s*(.-)%s*$", "%1")
    if g ~= "" then
      handle:headers():add("Impersonate-Group", g)
    end
  end
end
```

The split filter **removes** the `x-impersonate-groups` header after unpacking it,
so the API server never sees the comma-joined helper header — only the per-group
`Impersonate-Group` lines it expects.

Wire the **required** split filter as an Istio `EnvoyFilter` on **the waypoint that
fronts the protected route** — the *same* waypoint the `CUSTOM` `AuthorizationPolicy`
attaches to, where the ext_authz filter actually runs. It must **not** target the
authenticator's own pods (`app.kubernetes.io/name: holos-authenticator`): the
authenticator is the ext_authz *service*, not the proxy on the request path, so a
filter on it never sees the forwarded request. Target the waypoint with `targetRefs`
to its `gateway.networking.k8s.io` `Gateway` (a waypoint is a Gateway), and order the
filter **after** ext_authz with `filterClass: AUTHZ` — preferred over matching the
unstable generated ext_authz filter name (see [*Filter ordering relative to the
ext_authz chain*](#filter-ordering-relative-to-the-ext_authz-chain-filterclass-authz)):

```yaml
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  # Co-locate with the waypoint Gateway.
  name: holos-authenticator-split-groups
  namespace: <waypoint-namespace>
spec:
  # Target the WAYPOINT Gateway that fronts the protected route — the same proxy
  # the CUSTOM AuthorizationPolicy targets — NOT the authenticator workload.
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: <protected-route-waypoint>
  configPatches:
    # Split filter — filterClass: AUTHZ places it AFTER Istio's authz filters,
    # including CUSTOM ext_authz, so the authorizer's injected x-impersonate-groups
    # header is present (no need to name the generated, version-unstable ext_authz
    # filter). `context` is omitted: targetRefs already scopes the patch to the
    # waypoint proxy.
    - applyTo: HTTP_FILTER
      match:
        listener:
          filterChain:
            filter:
              name: envoy.filters.network.http_connection_manager
      patch:
        operation: ADD
        filterClass: AUTHZ
        value:
          name: holos-authenticator.split-groups
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
            inlineCode: |
              function envoy_on_request(handle)
                local joined = handle:headers():get("x-impersonate-groups")
                if joined == nil or joined == "" then
                  return
                end
                handle:headers():remove("x-impersonate-groups")
                for group in string.gmatch(joined, "([^,]+)") do
                  local g = group:gsub("^%s*(.-)%s*$", "%1")
                  if g ~= "" then
                    handle:headers():add("Impersonate-Group", g)
                  end
                end
              end
```

> **Adding the optional reject filter.** To layer in the defense-in-depth reject
> filter (above), append a **second** `configPatch` to this same `EnvoyFilter` that
> `INSERT_BEFORE`s the `envoy.filters.http.ext_authz` subFilter with the reject Lua.
> A reject filter **cannot** use `filterClass: AUTHZ`: an AUTHZ-class filter runs
> only when ext_authz *allows* the request, but the reject filter must act on the
> inbound request *before* the callout (see [*Filter ordering relative to the
> ext_authz chain*](#filter-ordering-relative-to-the-ext_authz-chain-filterclass-authz)).
> It is optional — smuggling prevention is the authorizer's responsibility, so the
> split filter alone is the minimal required wiring.

> **Robust to both Envoy representations.** Whether Envoy materializes the groups
> header as a single comma-joined value or as duplicate header entries can vary by
> Envoy version and HTTP transport. The split filter handles **both**: the Lua
> `Headers:get()` returns all values for a given header **concatenated with commas**,
> so `get("x-impersonate-groups")` yields `oidc:dev,oidc:ops` regardless of which
> on-the-wire form Envoy chose, and the `remove` + per-element `add` then re-emits
> clean, one-per-group `Impersonate-Group` headers either way. The matching
> server-side guard (`firstUnsafeGroup`) guarantees no individual group value
> contains a comma or surrounding whitespace, so this split never fans one group into
> many or trims a padded value into a privileged one.
>
> **Verify against the deployed Envoy before relying on it.** Because these filters
> are exercised only at runtime on a real waypoint — there is no live Envoy in the
> repo's test suite, and the unit tests cover only the ext_authz response options,
> not Envoy's header mutation plus the Lua pass — **confirm the end-to-end behavior
> against the actual Envoy/Istio version** when the waypoint topology is built:
> assert that a multi-group token results in one `Impersonate-Group` header **per
> group** reaching the API server, and that a client-supplied `X-Impersonate-Groups`
> is rejected. This runtime proof is part of the deferred waypoint work below.
>
> **Not yet rendered by the component.** Like the `CUSTOM` `AuthorizationPolicy`,
> this Lua `EnvoyFilter` only has an effect once a **waypoint** fronts the
> protected route, and it must target that same waypoint. The full
> waypoint / `ServiceEntry` egress topology is deferred (see
> [*Istio extensionProvider + AuthorizationPolicy wiring*](#istio-extensionprovider--authorizationpolicy-wiring)
> and [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md)), so the
> split filter above (and the optional reject filter) are documented here as the
> companion to that topology rather than shipped in the deploy tree today.

### Filter ordering relative to the ext_authz chain (`filterClass: AUTHZ`)

What makes the split filter work is **where it sits relative to the CUSTOM
ext_authz callout**, not how it is named. The split filter must run **after**
ext_authz (so the authorizer's injected `x-impersonate-groups` header is present);
the optional reject filter must run **before** it (so a client-supplied header is
refused before any mutation). There are two ways to express that ordering:

- **`filterClass: AUTHZ` (preferred).** Istio injects the CUSTOM ext_authz filter
  into the **AUTHZ** filter group of the gateway/waypoint HTTP connection manager.
  An `EnvoyFilter` that adds the Lua filter with `filterClass: AUTHZ` lands **after**
  Istio's authz filters — including CUSTOM ext_authz — so it sees the injected
  header, **without** naming the generated ext_authz filter. Prefer this: the
  generated filter name is **not a stable contract** across Istio/Envoy versions,
  whereas the AUTHZ filter class is.
- **`INSERT_AFTER` matched on the ext_authz subFilter name.** A patch with
  `operation: INSERT_AFTER` and a `subFilter.name: envoy.filters.http.ext_authz`
  match pins insertion to just after the ext_authz filter (and `INSERT_BEFORE` for
  a reject filter). It works, but couples the patch to the generated filter name —
  re-verify it after an Istio upgrade. Prefer `filterClass: AUTHZ` above.

A reject filter (optional defense in depth) **cannot** use `filterClass: AUTHZ`: an
AUTHZ-class filter's `envoy_on_request` runs **only when ext_authz allows** the
request — a denied request short-circuits the chain and never reaches it — and the
reject filter must act on the inbound request *before* the callout. Place it with
`filterClass: AUTHN`, or `INSERT_BEFORE` the ext_authz subFilter. But recall the
reject filter is not required at all: smuggling prevention is the authenticator's
responsibility, so the minimal wiring is a **single AUTHZ-class split filter**.

#### Gateway (ingress) example

The split filter applies to an **ingress gateway** as well as an ambient waypoint.
On a gateway the `EnvoyFilter`, its `workloadSelector`, and the CUSTOM
`AuthorizationPolicy` all target the gateway workload and live in the gateway's
namespace; `filterClass: AUTHZ` orders the split filter after the callout on the
gateway's serving-port listener:

```yaml
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: holos-authenticator-split-groups
  namespace: istio-gateways          # the gateway's namespace
spec:
  workloadSelector:
    labels:
      istio: ingressgateway          # must match the gateway pod's labels
  configPatches:
    - applyTo: HTTP_FILTER
      match:
        context: GATEWAY
        listener:
          portNumber: 8443           # the gateway's serving port, not 15006
          filterChain:
            filter:
              name: envoy.filters.network.http_connection_manager
      patch:
        operation: ADD
        filterClass: AUTHZ           # after Istio's authz, incl. CUSTOM ext_authz
        value:
          name: holos-authenticator.split-groups
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
            inlineCode: |
              function envoy_on_request(handle)
                local joined = handle:headers():get("x-impersonate-groups")
                if joined == nil or joined == "" then
                  return
                end
                handle:headers():remove("x-impersonate-groups")
                for group in string.gmatch(joined, "([^,]+)") do
                  local g = group:gsub("^%s*(.-)%s*$", "%1")
                  if g ~= "" then
                    handle:headers():add("Impersonate-Group", g)
                  end
                end
              end
```

Notes on the gateway YAML:

- `workloadSelector.labels` must match the gateway deployment's pod labels.
  `istio: ingressgateway` is the classic injected ingress gateway; a Gateway
  API / Helm gateway may use different labels — check with
  `kubectl get pod -n <ns> --show-labels`.
- Set `listener.portNumber` to the gateway's actual serving port (e.g. 8080/8443),
  **not** the sidecar inbound `15006` — a gateway has no `15006` listener. Matching
  the HCM via `filterChain.filter.name` keeps the patch off non-HTTP listeners.
- The matching CUSTOM `AuthorizationPolicy` must also select this gateway workload
  in the gateway's namespace (or the mesh root namespace), or the ext_authz filter
  is not present on this listener at all.

#### The provider must forward the groups header upstream

The split filter only sees `x-impersonate-groups` if ext_authz actually added it to
the upstream request:

- **`envoyExtAuthzGrpc` provider (what this platform uses).** Headers returned in
  the OK `Check` response are added to the upstream request automatically — no
  allowlist required. This is one reason the gRPC provider is the right fit.
- **`envoyExtAuthzHttp` provider.** The groups header must be listed in
  `headersToUpstreamOnAllow` in the provider definition, or it is dropped before the
  Lua filter runs — the most common cause of a no-op split filter.

#### Verifying filter order

Confirm the split filter sits **after** ext_authz in the chain. A gateway has no
inbound `15006` listener; inspect its serving-port listener instead:

```bash
istioctl proxy-config listener <gateway-pod> -n <gateway-ns> --port 8443 -o json
```

In that listener's HCM `httpFilters`, verify `holos-authenticator.split-groups`
appears **after** the ext_authz filter. If it runs before the callout, the injected
header is not yet present and the filter is a no-op — that ordering is the thing to
investigate. Then confirm the backend (API server audit log or a debug echo
endpoint) receives one `Impersonate-Group` header **per** CSV value.

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
   not resolve the impersonator credential** — neither reading a
   `credentialsSecretRef` Secret nor minting a `serviceAccountRef` token. The
   credential is resolved later, on the `Check` data path (failing closed with 403
   if absent — a missing Secret, or a TokenRequest/RBAC failure for the SA path),
   so `Ready=True` is **not** a signal that the credential is usable. Ensure the
   credential is provisioned (the [credential section](#provisioning-the-credential-serviceaccountref-or-a-runtime-secret)
   — for `serviceAccountRef` confirm the impersonator SA and its impersonation
   RBAC exist; for `credentialsSecretRef` create the Secret) regardless of the
   Backend's readiness:

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
- **403 even with a valid token.** The impersonator credential could not be
  resolved or lacks the needed `impersonate` RBAC. For a `credentialsSecretRef`
  Backend, the named Secret is missing in the `holos-authenticator` namespace. For
  a `serviceAccountRef` Backend, the referenced SA does not exist or the manager's
  `Role` cannot `create` on its `serviceaccounts/token` (the shipped grant is
  scoped by `resourceNames` to `holos-authenticator-impersonator` — widen it if you
  point `serviceAccountRef.name` elsewhere). In either case the impersonator
  identity may also lack `impersonate` on what the CEL mapping emits — the shipped
  default ClusterRole grants only the SA virtual groups `system:authenticated`/
  `system:serviceaccounts`, so impersonating a specific user, a per-namespace
  `system:serviceaccounts:<ns>` group, or a specific SA (via the `serviceaccounts`
  resource, not `users`) needs the per-`Backend` grants from the *Impersonation
  RBAC* section. Verify the credential source, the manager `Role`, and the
  impersonator ClusterRole/Role bindings.
- **Request denied because it "carries impersonation headers."** On a Backend with
  `spec.impersonation` **nil** (self mode) this is the failure-closed guard (step
  2): a client or upstream proxy sent an `Impersonate-*` header and the authorizer
  refuses to forward it — ensure no proxy in front of the waypoint injects one. On a
  Backend with **delegated impersonation** enabled, an inbound non-extra
  `Impersonate-*` is instead the mode switch, so a 403 here means the **actor is
  not authorized** (their mapped groups do not intersect
  `spec.impersonation.groups`), the request set any inbound `Impersonate-Extra-*`
  header (never client-settable), it carried an unrecognized `Impersonate-*`
  header, it named no `Impersonate-User` target, or a passthrough `--as-group`
  element had **surrounding whitespace** (a comma is not a denial — it separates
  groups). Check the actor's mapped groups against the
  allowlist and the [*Delegated
  impersonation*](#delegated-impersonation-kubectl---as-passthrough) rules.
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
- **Impersonation works to the authorizer but the API server rejects it (or the
  groups go missing), and you need to see exactly which headers were
  returned.** Raise the manager's log verbosity to `V(1)` — pass
  `--zap-log-level=1` (or higher) to the `holos-authenticator` binary. On every
  `Check` the authorizer then logs, at one line per header, every header it returns
  to Envoy: the decision branch (`ok`/`denied`, plus the HTTP status on a denial),
  and each header's `name`, `value`, `appendAction`, and the deprecated `append`
  bool. As of HOL-1416 every header the authorizer emits uses the **overwrite/set**
  action with `append` **false** — including the single comma-joined groups header
  (default `x-impersonate-groups`); a logged `append=true` (the dropped-by-Envoy
  encoding HOL-1414/Revision 6 used) would be a regression. This is the fast way to
  tell whether the authorizer emitted the headers correctly (one
  `x-impersonate-groups` header carrying the CSV of mapped groups, overwrite/set) or
  whether the Lua split filter or Envoy mishandled them downstream. The
  `Authorization` value is redacted to a byte-length marker — the impersonator
  credential is never logged. Lower verbosity back to the default once done, since
  the per-header lines are high-volume.

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

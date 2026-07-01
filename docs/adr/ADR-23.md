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
| 4        | 2026-06-25 | @jeffmccune | `serviceAccountRef` credential source — additive TokenRequest-minted impersonator credential, default impersonate-only SA, impersonation-scope ratification (below) |
| 5        | 2026-06-25 | @jeffmccune | Group-prefix extension — additive `spec.oidc.groupsPrefix` for the default groups-claim mapping (below) |
| 6        | 2026-06-26 | @jeffmccune | Group encoding correction — `Impersonate-Group` is emitted as `APPEND_IF_EXISTS_OR_ADD`, Envoy comma-joins it, and it must be paired with a Lua split filter (below) |
| 7        | 2026-06-27 | @jeffmccune | Group encoding fix (HOL-1416) — groups emitted as a single comma-joined **overwrite/set** header (configurable, default `X-Impersonate-Groups`), not per-group append options Envoy silently drops; paired reject + split Lua filters (below) |
| 8        | 2026-06-27 | @jeffmccune | Smuggling-prevention ownership + Lua filter ordering (HOL-1417) — smuggling prevention is the authenticator's responsibility (no `EnvoyFilter` required; the reject filter is optional defense in depth); the split filter is ordered after ext_authz with the version-stable `filterClass: AUTHZ`, on a gateway as on a waypoint (below) |
| 9        | 2026-06-27 | @jeffmccune | Username-prefix extension (HOL-1418) — additive `spec.oidc.usernamePrefix` for the impersonated username, the apiserver `--oidc-username-prefix=oidc:` equivalent, paired by convention with `groupsPrefix` (below) |
| 10       | 2026-06-27 | @jeffmccune | UID + extra-fields extension (HOL-1419) — additive `spec.oidc.uidClaim` → `Impersonate-Uid` and `spec.oidc.extra[]` → `Impersonate-Extra-<key>`, both single-valued overwrite headers (no Lua split), completing the four impersonation dimensions (below) |
| 11       | 2026-07-01 | @jeffmccune | Delegated impersonation (`kubectl --as` passthrough, HOL-1429/HOL-1430/HOL-1433) — additive `spec.impersonation` (`groups` allowlist + reserved `actorExtra[]`); inbound `Impersonate-*` is no longer *always* denied but is the self-vs-delegated **mode switch**; an authorized actor's target passes through, the reserved `Impersonate-Extra-actor-*` namespace is never client-settable, target authz is delegated to the impersonator SA's API-server RBAC, and the AC6 rule disables all Backend-derived headers except `actorExtra` in delegated mode (below) |

## As-built (Revision 2)

The implementation phases (HOL-1385..HOL-1390) shipped the design as proposed,
with the deviations noted below. `Status` is now `Implemented`.

- **As built.** The `cmd/holos-authenticator` controller-runtime manager runs
  the Envoy ext_authz gRPC server as a `manager.Runnable` with
  `NeedLeaderElection() == false`; `Check` routes by `Host`, sanitizes inbound
  `Impersonate-*` headers (failure-closed — denies rather than scrubs; made
  conditional for an authorized actor by Revision 11's delegated mode), validates
  the OIDC token (issuer discovery + JWKS, `iss`/`aud`/`exp`), maps claims to
  groups via CEL (default `claims["<groupsClaim>"]`, i.e. `claims["groups"]`),
  and returns `Impersonate-User` plus a single comma-joined groups header and the
  impersonator credential, overwriting `Authorization` (the groups are emitted as
  one comma-joined **overwrite/set** header — `X-Impersonate-Groups` by default,
  configurable — that a paired Lua split filter unpacks into one
  `Impersonate-Group` per group, **not** per-group append options Envoy drops —
  Revision 7, superseding Revision 6). The
  `authenticator.holos.run/v1alpha1`
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
  model. **(Superseded in part by Revision 11.)** The "inbound `Impersonate-*`
  headers are rejected fail-closed" clause here describes the **self-only**
  behavior. Revision 11 (delegated impersonation) makes that rejection
  **conditional**: it is the default (nil `spec.impersonation`) and still applies
  to an unauthorized actor, but a Backend may opt an **authorized** actor into
  forwarding an actor-supplied `Impersonate-*` target (`kubectl --as`
  passthrough). See Revision 11 below.
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
**offline** against a static JSON Web Key Set, so the authenticator can accept
**service-account (KSA) ID tokens minted by a remote cluster** whose token issuer
(its service-account JWKS endpoint) is unreachable from this cluster. The
motivating case is a remote workload (e.g. an External Secrets Operator
`SecretStore`) presenting its projected KSA ID token to be impersonated on the
**management** cluster. The remote cluster is only the token **issuer / JWKS
source**; the impersonated request is forwarded to the API server named by the
`Backend`'s `spec.server.url` — the **management** cluster's API server, not the
remote one. (`spec.host` is the per-remote-cluster routing key; `spec.server.url`
is the upstream the impersonated request lands on.)

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
  the SA would be. The impersonator identity (the `serviceAccountRef` SA — Rev 4 —
  or a `credentialsSecretRef` token) must therefore hold `impersonate` on the SA
  username and those SA virtual groups (runbook).
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

## `serviceAccountRef` credential source (Revision 4)

Revision 4 (HOL-1399..HOL-1402, under parent HOL-1398) adds a second way to
supply a `Backend`'s **outbound impersonator credential**: instead of an
operator minting a token and committing it to a Secret, the `Backend` references
a **management-cluster ServiceAccount** and the authorizer mints, caches, and
rotates that SA's bearer token itself via the Kubernetes **TokenRequest** API.
This is the credential the authorizer presents *to the upstream API server*
(`spec.server.url`) to authorize the impersonated request — the **outbound**
side. It is **distinct from** Revision 3's KSA / static-JWKS work, which is about
validating an **inbound** remote-cluster SA token (`spec.oidc.jwks`); the two are
independent SA-related features that the runbook and README keep explicitly
separate.

- **Additive `spec.serviceAccountRef` (no new CRD, mutually exclusive with
  `credentialsSecretRef`).** The `Backend` CR gains an optional
  `*ServiceAccountReference` field
  (`api/authenticator/v1alpha1/{backend_types.go,common_types.go}`):
  `serviceAccountRef.name` (defaults to **`holos-authenticator-impersonator`**,
  `MinLength=1`), `serviceAccountRef.audience` (optional — empty means the API
  server's default audience), and `serviceAccountRef.expirationSeconds`
  (`*int64`, defaults to **3600**, `Minimum=600`). `credentialsSecretRef` becomes
  an optional pointer (`*SecretReference`) alongside it. The two credential
  sources are **mutually exclusive**, enforced by a CRD-level CEL `XValidation`
  on the spec — `!(has(self.credentialsSecretRef) && has(self.serviceAccountRef))`
  (*"credentialsSecretRef and serviceAccountRef are mutually exclusive; set at
  most one"*) — and by runtime precedence (the Check path checks
  `serviceAccountRef` first). A `Backend` that sets neither still resolves the
  conventional default Secret on the `credentialsSecretRef` path; backends that
  omit `serviceAccountRef` are fully backward-compatible.
- **Runtime TokenRequest mint / cache / rotate (HOL-1400).** A `TokenManager`
  (`internal/authenticator/token_manager.go`) mints SA tokens by `create` on the
  `serviceaccounts/token` subresource (no `BoundObjectRef`, exactly like
  `kubectl create token`), caches them keyed by **name + audience +
  expirationSeconds** (so multiple Backends naming the same SA/audience/lifetime
  share one cached token), and **rotates** each token before expiry with a margin
  of the **smaller of 5m or 20% of the token's lifetime**. The Check data path
  resolves the credential through `resolveCredential`
  (`internal/authenticator/credentials.go`): `serviceAccountRef` set → mint/cache
  via the `TokenManager`; otherwise → read the `credentialsSecretRef` Secret. The
  reconciler normalizes `serviceAccountRef` defaults before storing the in-memory
  `Entry`, so the Check path applies no further defaulting.
- **Default impersonate-only ServiceAccount + scoped RBAC (HOL-1401).** The
  `holos-authenticator` component now ships a dedicated
  **`holos-authenticator-impersonator`** ServiceAccount (the
  `serviceAccountRef.name` default) — **distinct from the manager's own
  `holos-authenticator` SA** — plus an impersonate-only ClusterRole and
  ClusterRoleBinding. The manager's namespaced `Role` gains `create` on
  `serviceaccounts/token`, scoped by `resourceNames` to exactly
  `holos-authenticator-impersonator` (matching the kubebuilder
  `+kubebuilder:rbac:…resources=serviceaccounts/token,resourceNames=holos-authenticator-impersonator,verbs=create`
  marker in `cmd/holos-authenticator/main.go` and the generated
  `config/authenticator/rbac/role.yaml`).
- **Deviation (ratified): the shipped impersonator ClusterRole is impersonate-on-
  `groups`-only, scoped to SA virtual groups — not unbounded
  `users`/`groups`/`serviceaccounts`.** Parent issue HOL-1398's AC #3 literally
  asked for a ClusterRole granting `impersonate` on `users`, `groups`, **and**
  `serviceaccounts`. As built, the shipped default
  `holos-authenticator-impersonator` ClusterRole grants `impersonate` on **`groups`
  only**, narrowed with `resourceNames` to the two namespace-independent SA
  virtual groups **`system:authenticated`** and **`system:serviceaccounts`** —
  and grants **nothing** on `users` or `serviceaccounts`:

  ```yaml
  rules:
    - apiGroups: [""]
      resources: ["groups"]
      verbs: ["impersonate"]
      resourceNames: ["system:authenticated", "system:serviceaccounts"]
  ```

  An **unbounded** `impersonate` on `users`/`groups`/`serviceaccounts` is a
  cluster-wide privilege-escalation credential (it can impersonate *any* user or
  *any* group, including `system:masters`) — Codex flagged exactly this on the
  implementation PR, and the runbook's *Impersonation RBAC* guidance already
  forbids it ("prefer mapping to non-`system:` groups so the blast radius … stays
  bounded"). The shipped default therefore gives the impersonator only the
  always-present SA virtual groups a KSA-token Backend needs, and **per-identity /
  per-namespace impersonate scope** (a specific SA via the `serviceaccounts`
  resource, or a per-namespace `system:serviceaccounts:<ns>` group) is
  **operator-applied per `Backend`**, following the worked RBAC in the runbook's
  *KSA / static-JWKS backends* section. **This Revision 4 ratifies the
  impersonate-only / virtual-groups-scoped default as the intended security
  posture**, so the deviation from AC #3's literal wording is a deliberate,
  reviewed decision rather than an open `needs-human-review` item.
- **Provisioning shifts from manual to controller-managed.** With
  `serviceAccountRef`, the operator does **not** `kubectl create token` and create
  a Secret (the `credentialsSecretRef` path); they reference the shipped
  `holos-authenticator-impersonator` SA (or their own SA in the
  `holos-authenticator` namespace) and the controller mints/rotates the token. The
  `credentialsSecretRef` path remains fully supported — for an **external** API
  server whose impersonator credential is an out-of-band raw bearer token, it is
  still the right choice (the management cluster cannot mint a token for a remote
  cluster's SA). The component renders one example of each path:
  `backend-example` (`credentialsSecretRef`) and `backend-remote-cluster-a`
  (`serviceAccountRef: {}`, all defaults).

The operator procedure (referencing the default impersonator SA, the
controller's `serviceaccounts/token` grant, the caching/rotation behavior, and
adding per-`Backend` impersonate scope) is in the runbook's [*Provisioning the
credential*](../runbooks/holos-authenticator.md#provisioning-the-credential-serviceaccountref-or-a-runtime-secret)
and [*Impersonation RBAC*](../runbooks/holos-authenticator.md#impersonation-rbac-the-forwarded-credential)
sections; the component resources are in its
[README](../../holos/components/holos-authenticator/README.md).

### Backend spec model (as built, Rev 4)

```text
spec:
  host:        <string, required>          # request :authority/Host routing key
  server:
    url:       <string, required>          # upstream API server (in-cluster or external)
    caBundle:  <[]byte, optional>          # recorded, not yet consumed (Rev 2 deviation)
  oidc:
    issuerURL:      <string, required>     # issuer base URL, or expected `iss` when jwks is set
    clientID:       <string, required>     # token audience (`aud`)
    caBundle:       <[]byte, optional>     # trust for issuer dial; unused when jwks is set
    jwks:           <[]byte, optional>     # static JWKS → offline validation (Rev 3)
    usernameClaim:  <string, optional>     # default `sub`
    usernamePrefix: <string, optional>     # prefix the impersonated username (Rev 9); recommended `oidc:`
    groupsClaim:    <string, optional>     # default `groups`
    groupsPrefix:   <string, optional>     # prefix the default mapping (Rev 5); excl. celExpression
  groupMapping:
    celExpression:  <string, optional>     # default `claims["<groupsClaim>"]`; excl. oidc.groupsPrefix
  # At most one credential source (mutually exclusive, CEL-enforced; neither set
  # → the default credentialsSecretRef Secret):
  credentialsSecretRef:                    # *SecretReference, optional
    name:  <string, default holos-authenticator-backend-creds>
    key:   <string, default token>
  serviceAccountRef:                       # *ServiceAccountReference, optional (Rev 4)
    name:               <string, default holos-authenticator-impersonator>
    audience:           <string, optional>   # empty → API server default audience
    expirationSeconds:  <int64,  default 3600, min 600>
```

## Group-prefix extension (Revision 5)

Revision 5 (HOL-1406..HOL-1408) adds an optional **`spec.oidc.groupsPrefix`** to
the `Backend`, the equivalent of the apiserver `--oidc-groups-prefix=oidc:` flag.
When set, the prefix is prepended to **every** group produced by the **default
groups-claim mapping** before it is impersonated, so a `Backend` with
`oidc.groupsClaim: groups` and `oidc.groupsPrefix: "oidc:"` impersonates
`oidc:dev`, `oidc:ops`, … instead of `dev`, `ops`.

- **Additive `spec.oidc.groupsPrefix` (no new CRD).** A single optional string
  field on the existing `Backend` CR. There is no default — an omitted
  `groupsPrefix` prepends nothing, so backends that do not set it are
  byte-for-byte backward compatible. (An explicit empty string is rejected by
  `MinLength=1`: omit the field to prepend nothing rather than carry an ambiguous
  no-op.)
- **Recommended `oidc:`, to isolate the IdP's group namespace.** Like the
  apiserver flag, the recommended value is `oidc:`. Prefixing isolates the
  external identity provider's group namespace so it **cannot impersonate
  Kubernetes built-in `system:` groups** — a token asserting `system:masters`
  becomes `oidc:system:masters`, which holds no privilege, rather than the real
  cluster-admin group. Without a prefix the groups claim is impersonated
  verbatim, so an IdP that can mint an arbitrary `groups` claim could assert a
  `system:` group directly; the prefix is the recommended mitigation.
- **Default-mapping only; mutually exclusive with `celExpression`.** The prefix
  is honored only with the **default** group mapping (when
  `spec.groupMapping.celExpression` is empty, where the authorizer reads the
  groups claim directly). A custom `celExpression` returns the final group set
  itself, so it must encode any prefix it wants — `claims["groups"].map(g,
  "oidc:" + g)` is the explicit CEL form. The two are therefore **mutually
  exclusive**, enforced by a CEL `XValidation` rule on `BackendSpec`
  (`!(has(self.oidc.groupsPrefix) && has(self.groupMapping.celExpression))`); a
  `Backend` setting both is rejected by the API server.
- **Implemented inside the single CEL evaluation path.** The prefix is not a
  second Go code path: when set, the default-mapping builder
  (`authenticator.DefaultGroupExpression`) emits
  `claims["<groupsClaim>"].map(g, "<prefix>" + g)`, with the prefix embedded as a
  CEL string literal (via `%q`) so an operator-supplied value cannot inject CEL.
  This preserves the missing-claim → no-groups semantics (a token with no groups
  claim yields no groups, not an error) and type-checks to `list(string)`,
  exactly as the unprefixed `claims["<groupsClaim>"]` default does.

The operator procedure (the recommended `oidc:` value, the `system:`-group
rationale, the mutual-exclusion rule, and an example) is in the
[runbook's *OIDC token validation + CEL group
mapping*](../runbooks/holos-authenticator.md#oidc-token-validation--cel-group-mapping)
section.

## Username-prefix extension (Revision 9)

Revision 9 (HOL-1418) adds an optional **`spec.oidc.usernamePrefix`** to the
`Backend`, the equivalent of the apiserver `--oidc-username-prefix=oidc:` flag and
the username-side companion to `spec.oidc.groupsPrefix` (Revision 5). When set,
the prefix is prepended to the username read from the username claim (the claim
named by `oidc.usernameClaim`) before it is impersonated, so a `Backend` with
`oidc.usernameClaim: sub` and `oidc.usernamePrefix: "oidc:"` impersonates the
Kubernetes user `oidc:alice` instead of `alice`.

- **Additive `spec.oidc.usernamePrefix` (no new CRD).** A single optional string
  field on the existing `Backend` CR. There is no default — an omitted
  `usernamePrefix` prepends nothing, so backends that do not set it are
  byte-for-byte backward compatible. (An explicit empty string is rejected by
  `MinLength=1`: omit the field to prepend nothing rather than carry an ambiguous
  no-op, exactly as `groupsPrefix` does.)
- **Recommended `oidc:`, to isolate the IdP's username namespace.** Like the
  apiserver flag, the recommended value is `oidc:`. Prefixing isolates the
  external identity provider's username namespace so a token **cannot impersonate
  Kubernetes built-in `system:` users** — a token whose username claim is
  `system:admin` becomes `oidc:system:admin`, which holds no privilege, rather
  than the real in-cluster identity. Without a prefix the username claim is
  impersonated verbatim, so an IdP that can mint an arbitrary username claim could
  assert a `system:` user directly; the prefix is the recommended mitigation.
- **Configured as a pair with `groupsPrefix`, independent in implementation.**
  The maintainer intention — matching the upstream apiserver guidance, where
  `--oidc-username-prefix` and `--oidc-groups-prefix` are intended to be set
  together — is that operators configure `usernamePrefix` and `groupsPrefix` **as
  a pair**, both conventionally `oidc:`. The convention is documentation, not a
  hard-coded default: the implementation keeps the two fields **independent** —
  each is its own optional string with no coupling and no enforcement — so a
  `Backend` may set one without the other. There is deliberately **no**
  kubebuilder default of `oidc:` on either field, which keeps them symmetric (a
  hard default on only one would silently prefix one identity dimension but not
  the other) and backward compatible.
- **Always applied; no `celExpression` exclusion.** Unlike `groupsPrefix`, which
  is honored only with the default groups-claim mapping and is mutually exclusive
  with `spec.groupMapping.celExpression` (groups flow through the single CEL
  evaluation path), the username is a **direct claim read** with no CEL mapping.
  The prefix is therefore applied unconditionally in the `Authenticator` after the
  username claim is read, and there is no analogous mutual-exclusion rule.

The operator procedure (the recommended `oidc:` value, the `system:`-user
rationale, and the pairing convention) is in the [runbook's *OIDC token
validation + CEL group
mapping*](../runbooks/holos-authenticator.md#oidc-token-validation--cel-group-mapping)
section.

## UID and extra-fields extension (Revision 10)

Revision 10 (HOL-1419) completes the four Kubernetes impersonation dimensions.
The `Backend` already mapped claims to the impersonated **username**
(`oidc.usernameClaim` / `usernamePrefix`) and **groups** (`oidc.groupsClaim` /
`groupsPrefix`, or `groupMapping.celExpression`); Revision 10 adds the remaining
two — the **UID** (`Impersonate-Uid`) and **extra fields**
(`Impersonate-Extra-<key>`) — via two additive `spec.oidc` fields:

- **`spec.oidc.uidClaim` → `Impersonate-Uid`.** An optional claim name (no
  default — omitting it emits no UID, byte-for-byte backward compatible). The
  recommended value is `sub`: a stable, non-reassignable identifier the apiserver's
  own structured authentication config encourages as the UID, so audit logs and any
  UID-based policy can distinguish a renamed or recycled username from the original
  principal while `usernameClaim` carries a human-friendly identity (e.g.
  `preferred_username` / `email`). When configured the claim **must** resolve to a
  non-empty string on every token — a missing, empty, or non-string UID claim
  **denies the request fail-closed** rather than silently impersonating without the
  stable UID the operator asked for.
- **`spec.oidc.extra[]` → `Impersonate-Extra-<key>`.** An optional list of
  `{key, valueClaim}` entries, each emitting the value of `valueClaim` as an
  `Impersonate-Extra-<key>` header. It is a CRD **`listType=map` keyed by `key`**,
  so the API server rejects duplicate keys at admission; each `key` must also be a
  **canonical** extra key — a lowercase HTTP header field-name token with no `%`
  (the reconciler validates it and rejects the spec `Accepted=False` otherwise,
  mirroring the `--impersonate-groups-header` guard). It is emitted verbatim as the
  header suffix, and the API server derives the extra-map key from that suffix by
  **lowercasing and percent-unescaping** it; requiring the key be already canonical
  keeps the case-sensitive `listMapKey` uniqueness aligned with the API server's
  lowercased keys and stops a `%` from decoding into a different key. An entry whose
  claim is **absent** (or null) on a given token is **skipped** (the extra is
  optional context, like a missing groups claim); a **present string** is emitted
  verbatim (including an empty value); an entry whose claim is **present but not a
  string** **denies fail-closed** (a misconfiguration pointing at a
  list/object/number claim). Each extra is **single-valued** in this phase;
  multi-valued extras are a deferred follow-up.
- **Single-valued → direct overwrite headers, no Lua split.** Both `Impersonate-Uid`
  and each `Impersonate-Extra-<key>` are single values, so they are emitted directly
  under their `Impersonate-*` names with the **overwrite/set** action — exactly like
  `Impersonate-User`. The comma-join + Lua split the multi-valued **groups** header
  requires (Revision 7, HOL-1416, to survive Envoy's ext_authz append-drop) does
  **not** apply: a single overwrite header is added unconditionally by Envoy
  (`setCopy`). No new Lua filter or `--impersonate-groups-header`-style flag is
  introduced. The existing inbound-smuggling guard already rejects any client-supplied
  `Impersonate-*` header (the `impersonate-` prefix covers `Impersonate-Uid` and
  `Impersonate-Extra-*`), so the only such headers the upstream sees are the
  authorizer's derived ones.

The operator field reference and a paired example are in the [runbook's *OIDC
token validation + CEL group
mapping*](../runbooks/holos-authenticator.md#oidc-token-validation--cel-group-mapping)
section.

## Delegated impersonation: `kubectl --as` passthrough (Revision 11)

Revision 11 (HOL-1429 → HOL-1430 → HOL-1433) adds **delegated impersonation** —
the `kubectl --as` passthrough mode. Until now the authorizer ran **self
impersonation** only: it validated the caller's OIDC token and impersonated *that
caller*, and any inbound `Impersonate-*` header was **always denied fail-closed**
(the confused-deputy guard: a client must not smuggle a privileged group under the
backend's impersonation credential). Revision 11 makes that denial **conditional**:
a Backend may opt an **authorized actor** into forwarding the actor-specified
target — exactly as `kubectl --as <someone-else>` — without the authorizer holding
a per-user credential.

This revision supersedes the Revision 2 as-built claim that inbound
`Impersonate-*` is *always* denied, and the Design step 2 / Consequences
"inbound header sanitization is non-negotiable" framing: those describe the
self-only behavior, which remains the default (nil `spec.impersonation`) but is no
longer universal. The security property is preserved — an **unauthorized** actor,
and a Backend that does not opt in, still deny any inbound `Impersonate-*`
fail-closed — but it is enforced by an authorization check, not a blanket reject.

### The two modes

- **Self impersonation (default, `spec.impersonation` nil).** Unchanged,
  byte-for-byte: validate the caller's token, impersonate the caller's derived
  identity (`Impersonate-User` + the comma-joined groups header + derived
  `Impersonate-Uid`/`spec.oidc.extra`), and **deny any inbound `Impersonate-*`
  header** fail-closed. This is the only mode when `spec.impersonation` is nil.
- **Delegated impersonation (`spec.impersonation` non-nil).** The presence of any
  inbound `Impersonate-*` header — other than the reserved actor-extra namespace
  (below) — is the **mode switch**. The validated token identifies an **actor**;
  if the actor is authorized (below) the actor-supplied target
  (`Impersonate-User`/`Impersonate-Uid`/`--as-group`/non-reserved
  `Impersonate-Extra-*`) is forwarded to the upstream verbatim, and the
  authorizer stamps the actor's own identity into the reserved
  `Impersonate-Extra-actor-*` headers. Absence of any inbound `Impersonate-*`
  keeps self mode — now additionally stamping the `actorExtra` headers so the
  actor identity is always recorded.

### `spec.impersonation` — the additive opt-in

A single additive `spec.impersonation` block (`*ImpersonationConfig`, no new CRD;
a Backend that omits it is fully backward-compatible):

- **`groups` (`[]string`, `listType=set`) — the actor allowlist.** Delegated
  impersonation is permitted only when the actor's **mapped** Kubernetes groups —
  the groups the authorizer computes for the validated actor token via the default
  groups-claim mapping or `spec.groupMapping.celExpression`, **not** the raw token
  claim — intersect this list. An omitted or empty `groups` allowlists nothing, so
  a `spec.impersonation` present but with empty `groups` leaves delegated
  impersonation effectively disabled (a deliberate opt-in default).
- **`actorExtra[]` (`[]ExtraMapping`, `listType=map` keyed by `key`) — the
  reserved actor-identity headers.** Maps token claims to
  `Impersonate-Extra-<key>` headers describing the **actor** (e.g.
  `actor-sub`/`actor-email`), exactly as `spec.oidc.extra` maps claims for the
  impersonated user. Its values are always set authoritatively by the authorizer
  from the validated actor token; they are a **reserved namespace** — an inbound
  `Impersonate-Extra-<actorKey>` header is rejected fail-closed in **both** modes,
  so an actor can never spoof their own actor-identity. `actorExtra` keys must be
  disjoint from `spec.oidc.extra` keys (both share the single
  `Impersonate-Extra-<key>` header space); the reconciler validates disjointness
  and per-key canonicality (`Accepted=False` on violation).

### Semantics (the AC6 rule and the security invariants)

- **Allowlist matches mapped groups, not the raw claim.** The authz test is the
  actor's post-mapping Kubernetes groups intersected with `spec.impersonation.groups`.
- **AC6 — user-supplied headers disable all Backend-derived headers *except*
  `actorExtra`.** In delegated mode the actor's target **replaces** the derived
  self identity entirely: the authorizer does **not** emit the derived
  `Impersonate-User`/groups/`Impersonate-Uid`/`spec.oidc.extra`. The **only**
  Backend-derived headers that survive the delegation are the reserved
  `Impersonate-Extra-actor-*` headers, which record who actually performed the
  request (distinct from the impersonated target and from the impersonator SA) so
  audit tooling can attribute a delegated request to its real actor.
- **Target authorization is delegated to API-server RBAC — no target allowlist
  here.** The authorizer does not constrain *which* identity an authorized actor
  may impersonate; that is enforced by the **impersonator ServiceAccount's
  `impersonate` RBAC on the upstream API server**. The shipped default
  `holos-authenticator-impersonator` ClusterRole remains impersonate-only on the
  two SA virtual groups and is **not** broadened (Revision 4); an operator grants
  the impersonator SA `impersonate` on the intended target users/groups
  **per-`Backend`**. A target the impersonator SA cannot impersonate is rejected by
  the API server (403), not by the authorizer.
- **Every failure path stays fail-closed.** An unauthorized actor, a nil-
  `spec.impersonation` Backend receiving an inbound `Impersonate-*`, an inbound
  reserved `Impersonate-Extra-actor-*`, an unrecognized `Impersonate-*` header, a
  delegated request with no `Impersonate-User` target, or a passthrough group with
  **surrounding whitespace** — all Denied (403), never OK. The delegated-mode
  passthrough groups round-trip through the same comma-joined groups header + Lua
  split filter self mode uses (Revision 7): the actor's inbound `Impersonate-Group`
  is **split on commas** into individual groups (a comma is a group *separator*, not
  a denial — `dev,ops` is two groups), and the `firstUnsafeGroup` guard then denies
  only a surrounding-whitespace element on the split result.
- **The impersonator credential is unchanged.** Both modes present the same
  `serviceAccountRef`/`credentialsSecretRef` impersonator credential (Revision 4)
  as `Authorization` to the upstream.

The Check-path implementation is `internal/authenticator/server.go` (the reordered
flow, the reserved-actor-extra inbound guard, the mode detection, and the
delegated OK response), with table-driven `server_test.go` coverage. The operator
procedure (enabling delegated impersonation, the group-allowlist semantics,
configuring `actorExtra`, the `kubectl --as` flow, the audit-log distinction, and
the **required** per-`Backend` impersonator RBAC) is in the [runbook's *Delegated
impersonation*](../runbooks/holos-authenticator.md#delegated-impersonation-kubectl---as-passthrough)
section; the component ships a worked example `Backend` with `spec.impersonation`.

## Group encoding: comma-joined `Impersonate-Group` + Lua split filter (Revision 6)

> **Superseded by Revision 7 (HOL-1416).** This revision's encoding — per-group
> `Impersonate-Group` `APPEND_IF_EXISTS_OR_ADD` options relying on Envoy to
> comma-join them — is exactly what broke in production: Envoy's ext_authz path
> applies an authorizer's append header only if the request *already* carries it,
> and the inbound request never carries `Impersonate-Group`, so every group was
> silently dropped. Revision 7 replaces the per-group append encoding with a single
> comma-joined **overwrite/set** header under a distinct, configurable name
> (default `X-Impersonate-Groups`). The fail-closed unsafe-group guard and the Lua
> split filter survive into Revision 7; only the response encoding and the paired
> filters change. The section below is retained for historical context.

Revision 6 (HOL-1413) corrects how the multi-group `Impersonate-Group` encoding is
described, and records the Envoy Lua filter it must be paired with. The original
as-built note (Revision 2) claimed the authorizer's per-group
`APPEND_IF_EXISTS_OR_ADD` header options produced **repeated `Impersonate-Group`
header lines** "compatible with any conformant cluster." That is not how Envoy
applies them.

- **Envoy comma-joins repeated `APPEND_IF_EXISTS_OR_ADD` options.** The authorizer
  emits one `Impersonate-Group` `APPEND_IF_EXISTS_OR_ADD` `HeaderValueOption` per
  mapped group (so the groups accumulate rather than the last value overwriting the
  rest). When Envoy applies several append options for the **same** header name it
  **comma-concatenates** their values into a **single** header line —
  `Impersonate-Group: dev,ops` — it does **not** emit one `Impersonate-Group` line
  per value.
- **The API server does not split a comma-separated value.** Kubernetes
  impersonation requires **one `Impersonate-Group` header per group** and treats a
  comma-separated value as a single literal group name (`"dev,ops"`). Left as-is,
  the impersonated user lands in a non-existent group and loses their real group
  memberships — a correctness (and potential authorization) defect, not merely a
  cosmetic one.
- **Pair the response with a Lua split filter on the waypoint.** The comma-joined
  header must be unpacked back into one header per group by an Envoy **Lua HTTP
  filter** that runs **after** ext_authz (so it sees the injected header) and
  **before** egress to the API server: it reads `Impersonate-Group`, removes it, and
  re-adds one header per comma-delimited element. The filter attaches to the **same
  waypoint** the `CUSTOM` `AuthorizationPolicy` targets — where ext_authz actually
  runs — **not** the authenticator's own pods (the authenticator is the ext_authz
  *service*, not a proxy on the request path). The worked `EnvoyFilter` (`targetRefs`
  to the waypoint `Gateway`, the `INSERT_AFTER` `envoy.filters.http.ext_authz` patch,
  and the `inline_code` Lua) is in the [runbook's *Splitting the comma-joined groups
  header*](../runbooks/holos-authenticator.md#splitting-the-comma-joined-groups-header)
  section (updated for Revision 7's encoding).
- **Unsafe group values are denied fail-closed.** The comma-join + split round-trip
  is lossless only if no single group value contains a comma **or** has
  leading/trailing whitespace. A mapped group like `dev,system:masters` would be
  split into two impersonated groups, and ` system:masters` would be **trimmed** by
  the split filter into the bare `system:masters` — both privilege-escalation
  smuggling vectors. The authorizer therefore **denies (HTTP 403, fail-closed) any
  request whose mapped groups include a comma or surrounding whitespace** (the
  `firstUnsafeGroup` guard in `internal/authenticator/server.go`, with unit tests),
  so the split filter can never fan one group into many or normalize a padded value
  into a privileged one. The guard and the filter's whitespace trim are a matched
  pair — changing one without the other reopens the vector. This is the one
  behavioral change in Revision 6; the per-group append-option encoding itself is
  unchanged.
- **Not yet rendered — it belongs to the deferred waypoint topology.** Like the
  `CUSTOM` `AuthorizationPolicy`, the Lua `EnvoyFilter` only takes effect once a
  **waypoint** fronts the protected route and must target that same waypoint. The
  full waypoint / `ServiceEntry` egress topology remains a deferred follow-up
  (recorded in [`holos/docs/placeholders.md`](../../holos/docs/placeholders.md)),
  so Revision 6 documents the required filter as the companion to that topology
  rather than shipping it in the deploy tree today. The authorizer's response
  *encoding* (per-group append options) is unchanged; Revision 6's only behavioral
  change is the fail-closed comma-bearing-group guard above, plus the corrected
  documentation and the recorded paired filter.

## Group encoding fix: single overwrite/set groups header + reject/split Lua (Revision 7)

Revision 7 (HOL-1416) fixes the **production failure** Revision 6's encoding caused.
With the per-group `Impersonate-Group` append encoding deployed, requests reached the
API server with the impersonated **user** but **zero** groups — every RBAC binding to
a Keycloak group was ignored.

- **Root cause: Envoy drops an authorizer's append header when the request lacks it.**
  Envoy's ext_authz gRPC client classifies each authorizer-returned header by the
  deprecated `append` bool: `append=false` → `headers_to_set` (applied with `setCopy`,
  which adds the header unconditionally); `append=true` → `headers_to_append`, which
  `ext_authz.cc` `onComplete` applies with `appendCopy` **only if the request already
  carries that header** (`if (!header_to_modify.empty())`). The inbound request never
  carries `Impersonate-Group` (clients are rejected from supplying one), so the
  `append=true` group entries find nothing to append onto and are **discarded** before
  the split Lua filter ever sees them. `Impersonate-User` survived only because it uses
  `set`/`setCopy`. Revision 6's analysis traced only the *classification*
  (`ext_authz_grpc_impl.cc`), missing the `onComplete` "only if it already exists"
  guard one layer later.
- **Fix: one comma-joined overwrite/set header under a distinct, configurable name.**
  The authorizer now emits the mapped groups as a **single** `HeaderValueOption` whose
  value is the groups pre-joined on commas (`oidc:dev,oidc:ops`), with the
  **overwrite/set** action (`headers_to_set` → `setCopy`), so Envoy adds it even when
  the request did not already carry it. The header is **not** `Impersonate-Group` but a
  distinct, non-`Impersonate-*` name — **`X-Impersonate-Groups` by default**,
  configurable per deployment via the `--impersonate-groups-header` flag — so it does
  not collide with the inbound-rejection guard for `Impersonate-*` and the API server
  never receives the comma-joined helper directly. This removes the dependency on
  Envoy's deprecated-`append` comma-join behavior entirely.
- **Split Lua filter (required) + reject Lua filter (optional defense in depth).**
  The split filter (after ext_authz) reads the configured groups header, removes it,
  and re-adds one `Impersonate-Group` per comma-delimited element for the API server.
  An optional **reject** filter (before ext_authz) refuses any request carrying the
  configured groups header or any `Impersonate-*` header — the same fail-closed guard
  the authorizer enforces server-side (now extended to the configured groups header),
  at the proxy as defense in depth. The worked split `EnvoyFilter` and the
  optional-reject guidance are in the [runbook's *Splitting the comma-joined groups
  header*](../runbooks/holos-authenticator.md#splitting-the-comma-joined-groups-header)
  section. (**Revision 8** ratifies that only the split filter is required — smuggling
  prevention is the authenticator's responsibility — and that it is ordered after
  ext_authz with `filterClass: AUTHZ`.)
- **Unchanged.** The fail-closed `firstUnsafeGroup` guard (a mapped group with a comma
  or surrounding whitespace is denied 403) carries over verbatim — the comma-join +
  split round-trip has the same losslessness requirement. The username is still a single
  overwrite header. Like Revision 6's filter, the reject/split filters belong to the
  deferred waypoint topology and are not yet rendered by the component.

## Smuggling prevention is the authenticator's responsibility; Lua filter ordering (Revision 8)

Revision 8 (HOL-1417) records two clarifications about the boundary between the
authenticator and the Envoy proxy on the request path, prompted by HOL-1416's
group-dropping investigation. Neither changes the shipped code; both document the
as-built contract so an operator does not reach for an `EnvoyFilter` to do work the
authorizer already does, or couple a filter to an unstable Envoy filter name.

- **Preventing header smuggling is the authenticator's responsibility — no
  `EnvoyFilter` is required for it.** The authorizer denies, fail-closed on every
  `Check`, any request carrying an `Impersonate-*` header or a copy of the
  configured groups header (default `X-Impersonate-Groups`), and denies any request
  whose mapped groups contain a comma or surrounding whitespace (`firstUnsafeGroup`,
  `internal/authenticator/server.go`). Those server-side guards — with spoofed-header
  unit tests (`internal/authenticator/server_test.go`) — are the smuggling defense:
  a client cannot inject a privileged group under the backend's impersonation
  credential. The **reject** Lua filter the runbook documents (before ext_authz) is
  therefore **optional defense in depth**, not a requirement. The only Lua filter
  actually required on the request path is the **split** filter that unpacks the
  comma-joined groups header into one `Impersonate-Group` per group — a header-*shape*
  adaptation the Kubernetes API server's one-header-per-group impersonation contract
  forces, not a security control.
- **Filter ordering relative to ext_authz: prefer `filterClass: AUTHZ`.** The split
  filter must run **after** the CUSTOM ext_authz callout so it sees the authorizer's
  injected groups header. Istio injects the CUSTOM ext_authz filter into the
  **AUTHZ** filter group of the HTTP connection manager, so an `EnvoyFilter` adding
  the Lua filter with `filterClass: AUTHZ` lands after it — **without** naming the
  generated ext_authz filter, whose name is not a stable contract across Istio/Envoy
  versions. This is preferred over an `INSERT_AFTER` patch matched on the
  `envoy.filters.http.ext_authz` subFilter name. The same approach applies on an
  **ingress gateway**, not only an ambient waypoint: the `EnvoyFilter`, its
  `workloadSelector`/`targetRefs`, and the CUSTOM `AuthorizationPolicy` all target
  the gateway workload in the gateway's namespace, and `filterClass: AUTHZ` orders
  the split filter after the callout on the gateway's serving-port listener (not the
  sidecar `15006`, which a gateway lacks). Two corollaries the runbook now records:
  the `envoyExtAuthzGrpc` provider this platform uses forwards an OK response's
  headers upstream automatically (an HTTP `envoyExtAuthzHttp` provider would instead
  need the groups header listed in `headersToUpstreamOnAllow`, or it is dropped
  before the Lua filter runs), and an AUTHZ-class filter's `envoy_on_request` runs
  **only on allow** (a denied request short-circuits and never reaches it) — so a
  reject filter, which must act on the inbound request *before* the callout,
  necessarily sits before ext_authz (`AUTHN` class / `INSERT_BEFORE`), not in the
  AUTHZ class.

The operator procedure (the `filterClass: AUTHZ` gateway example, the optional
reject filter, the provider header-forwarding constraint, and verifying filter
order with `istioctl proxy-config listener`) is in the runbook's [*Filter ordering
relative to the ext_authz
chain*](../runbooks/holos-authenticator.md#filter-ordering-relative-to-the-ext_authz-chain-filterclass-authz)
subsection.

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
   `Authorization` headers. **(Revision 11 makes the `Impersonate-*` rejection
   conditional.)** This blanket rejection is the self-only behavior and remains the
   default; a Backend that opts into delegated impersonation (`spec.impersonation`)
   instead treats an inbound `Impersonate-*` header as the mode switch and forwards
   an **authorized** actor's target — see Revision 11. The `Authorization`
   overwrite is unchanged in both modes.
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
   `OkResponse` injecting `Impersonate-User` and a single comma-joined groups
   header (the mapped groups as one CSV value under the configured groups header,
   default `X-Impersonate-Groups`), plus the backend's own
   privileged credential (an `Authorization: Bearer <token>` for a ServiceAccount
   holding the `impersonate` verb), having first removed every inbound
   impersonation/`Authorization` header per step 2 so only the derived values reach
   the upstream. The groups header uses the **overwrite/set** action (Envoy adds it
   unconditionally), **not** per-group append options Envoy silently drops; because
   the API server requires one `Impersonate-Group` header per group and does not
   split a comma-separated value, this header **must be paired with an Envoy Lua
   filter** that unpacks the comma list into one `Impersonate-Group` per group before
   the request reaches the upstream — see Revision 7 below and the
   [runbook](../runbooks/holos-authenticator.md). Envoy then forwards the request
   to the upstream API server with those headers and **no other reverse proxy** in
   the path; the API server authorizes the request as the impersonated user with
   their real groups.

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
  **(Refined by Revision 11.)** "Reject every inbound `Impersonate-*`" is the
  self-only rule; delegated impersonation replaces the blanket reject with an
  **authorization check** — an unauthorized actor (and a Backend that does not opt
  in) is still denied fail-closed, but an authorized actor's target passes through.
  The reserved `Impersonate-Extra-actor-*` namespace is never client-settable in
  either mode, and impersonation-target authorization is delegated to the
  impersonator SA's API-server RBAC. See Revision 11.
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

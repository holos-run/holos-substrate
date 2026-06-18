# Runbook: Holos Controller — Quay credential wiring

Operator-facing guide for running the **Holos Controller**
([ADR-18](../adr/ADR-18.md)) and wiring it to the Quay superuser credential its
`quay.holos.run` Organization and Repository resources ([ADR-19](../adr/ADR-19.md))
authenticate with. The controller installs to the **`holos-controller`**
namespace and is built/deployed with the isolated `controller-*` make targets
(`Makefile.controller`), separate from `scripts/apply`, `scripts/render`, and the
`holos-paas` image.

This runbook covers the **AC #3** assumption — *a superuser-account OAuth
Application token authenticates all controller-managed Quay operations* — and how
to satisfy it. The credential itself is minted by the companion
[Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md);
this runbook is the consumer side: where that token lands and how the controller
reads it.

## The superuser-token assumption (AC #3)

Every Quay operation the controller performs — create/adopt an Organization,
create a Repository, register a `repo_push` webhook — is authenticated with **one
OAuth-Application token that acts as a Quay superuser**, the
`svc-quay-resource-controller` realm user ([ADR-15](../adr/ADR-15.md) Revisions
4–5). This is a deliberate design assumption, not an incidental one:

- Under `AUTHENTICATION_TYPE: OIDC` there is no local `admin` and no headless
  token-mint path, so the token is minted **by hand, once**, per the credentials
  runbook.
- The token carries `super:user` (plus `org:admin`/`repo:create`) and the
  instance has `FEATURE_SUPERUSERS_FULL_ACCESS: true`, so the controller can both
  **create** orgs and **adopt** orgs other identities created — it is the system
  of record for the Quay data plane and must be able to converge any org it owns
  or is told to adopt.
- Because that reach is instance-wide, the Organization reconciler enforces the
  **ownership/claim model** (ADR-19): it never silently seizes a pre-existing,
  externally-created org — adoption is an explicit `spec.adopt: true` opt-in, and
  the durable `status.created` marker records whether the controller created
  (deletes on removal) or adopted (releases on removal) each org.

There is **one** credential for the whole controller, resolved from its own
namespace; resources do not each carry distinct credentials (they may *name* a
different Secret via `credentialsSecretRef`, but the conventional and default
case is the single shared Secret below).

## The credential Secret: `holos-controller-quay-creds`

The controller resolves the Quay credential from a Secret named by each
resource's `spec.credentialsSecretRef` (a `{name, key}` reference), defaulting to
**`holos-controller-quay-creds`** when omitted. Two properties matter:

- **Namespace = the controller's own namespace.** The resolver reads the Secret
  from the controller's namespace — **`holos-controller`** — taken from the
  `POD_NAMESPACE` downward-API env the manager Deployment sets (default
  `holos-controller`), **not** the resource's namespace. So one operator-managed
  credential in `holos-controller` serves every tenant Organization/Repository
  across all namespaces.
- **Keys the controller reads** (`internal/controller/quay/credentials.go`):

  | Key | Required | Meaning |
  |-----|----------|---------|
  | `url` | yes | the Quay API base URL (e.g. `https://quay.holos.localhost`). |
  | `token` | yes | the superuser OAuth-Application access token. |
  | `username` | no | informational — the identity the token acts as (`svc-quay-resource-controller`). |

  `credentialsSecretRef.key`, when set, narrows the **token** lookup to a specific
  key; `url` and `username` always use the conventional key names. When the Secret
  or a required key is missing, the reconciler sets `Programmed`/`Ready` `False`
  with reason `CredentialsNotFound` and requeues — it does not crash.

The Deployment does **not** `envFrom`/mount a fixed credential Secret; credentials
are resolved per-resource via `credentialsSecretRef` at reconcile time (HOL-1313).
The Secret's material is created at **runtime** and **never committed** (the
runtime-secret guardrail, [`AGENTS.md`](../../AGENTS.md) Conventions /
[`holos/docs/secret-handling.md`](../../holos/docs/secret-handling.md)).

## Wiring the credential

1. **Mint the token** by hand following
   [quay-resource-controller-credentials.md](quay-resource-controller-credentials.md):
   sign in to Quay via "Holos SSO" as `svc-quay-resource-controller`, create the
   OAuth Application in the `platform-automation` org, and generate a token with
   the authoritative scope set that runbook specifies (the same set the helper
   script selects — `super:user`/`org:admin`/`repo:create` plus the repo
   read/write/admin and user scopes the Repository reconciler needs).

2. **Create the Secret** with the operator helper
   [`scripts/apply-svc-quay-resource-controller-creds`](../../scripts/apply-svc-quay-resource-controller-creds).
   The script keeps its historical name (the token acts as the
   `svc-quay-resource-controller` superuser identity) but now creates the Secret
   the controller actually reads:

   ```bash
   scripts/apply-svc-quay-resource-controller-creds
   # prompts for the token; sets QUAY_URL (default https://quay.holos.localhost)
   ```

   It produces, in the `holos-controller` namespace:

   | Field | Value |
   |-------|-------|
   | Namespace | `holos-controller` |
   | Secret name | `holos-controller-quay-creds` |
   | Keys | `url`, `token`, optional `username` |

3. **Reference it** (or rely on the default). A resource that omits
   `credentialsSecretRef` resolves `holos-controller-quay-creds` automatically;
   set the field only to point at a differently-named Secret.

## Deploy and verify the controller

The controller's lifecycle is driven by the isolated `controller-*` targets — they
never touch `scripts/apply`/`scripts/render` or the `holos-paas` image:

```bash
make controller-manifests        # regenerate CRDs + RBAC from Go markers
make controller-manifests-build  # render config/default (CRDs, RBAC, manager, metrics Service)
make controller-deploy           # kubectl apply -k config/default into holos-controller
```

The `holos-controller` namespace itself is owned by the central registry
(`holos/namespaces.cue`, `_ambient: true`) and rendered by `scripts/render`; the
kustomize tree targets it but does not create the Namespace object.

Verify:

```bash
# Manager is running in holos-controller:
kubectl -n holos-controller get deploy,pod

# The credential Secret exists with the expected keys:
kubectl -n holos-controller get secret holos-controller-quay-creds \
  -o jsonpath='{.data}' | tr ',' '\n'   # expect url, token (username optional)

# Metrics are scrapable (AC #4):
kubectl -n holos-controller get svc        # holos-controller-manager-metrics-service:8080
# controller-runtime reconcile metrics plus the custom collectors:
#   holos_controller_reconcile_total{kind,outcome}
#   holos_controller_quay_api_requests_total{operation,outcome}
```

A reconciling resource reports Gateway-API conditions
(`Accepted`/`Programmed`/`Ready`, and `WebhookConfigured` on a Repository); a
`CredentialsNotFound` reason on `Ready=False` means the Secret/key wiring above is
incomplete.

## Cluster bring-up — provisioning the `my-project` sample

Once the controller is deployed and the credential Secret is wired, the
`my-project` Layer 3 delivery sample is applied **separately** from the master
platform apply. As of HOL-1322, `my-project` is **removed from `scripts/apply`**
and applied by the dedicated
[`scripts/apply-my-project`](../../scripts/apply-my-project), because its
`quay.holos.run` Organization carries a per-cluster `caBundle` that must be
injected at apply time and never committed.

Run the bring-up steps **in order**:

1. **`scripts/local-ca`** — establishes the cert-manager `local-ca` whose
   certificate the in-cluster Quay serves TLS with, and whose PEM the next step
   injects as the Organization's `caBundle`.
2. **`scripts/apply`** — applies the platform (including the Quay registry the
   controller and credential mint target, and the `holos-controller` Namespace,
   which is owned by the central namespace registry and applied here, **not** by
   `make controller-deploy`).
3. **`make controller-deploy`** — installs the `quay.holos.run` CRDs and the
   manager into the `holos-controller` namespace (the *Deploy and verify the
   controller* steps above). It targets, but does not create, that Namespace, and
   `scripts/apply` does **not** install the CRDs; `scripts/apply-my-project` fails
   fast if the Organization CRD is absent.
4. **The manual credential mint** — `scripts/apply-svc-quay-resource-controller-creds`
   plus the `platform-automation` org / OAuth-Application token, per the
   [credentials runbook](quay-resource-controller-credentials.md). This creates
   the `holos-controller-quay-creds` Secret the Organization's
   `credentialsSecretRef` resolves.
5. **`scripts/apply-my-project`** — reads the local-ca PEM, renders the platform
   with it injected via the `ca_bundle_pem` CUE tag, and applies the `my-project`
   Namespace + Organization (and the rest of the component). It gates the
   Organization reaching `Ready`.

```bash
scripts/apply-my-project
```

**TLS trust comes from the resource's `caBundle`, not the pod's system store.**
The in-cluster Quay (`quay.holos.localhost`) serves a certificate signed by the
per-cluster mkcert local CA, which is **not** in the controller pod's system
trust store. The controller therefore establishes TLS to Quay by trusting the
**`spec.caBundle`** the `my-project` Organization carries (the standardized
cross-Kind field, [ADR-19](../adr/ADR-19.md) — appended to the system roots, not
replacing them) — `scripts/apply-my-project` populates it with the local-ca PEM
at apply time. An Organization applied **without** a `caBundle` (e.g. by `kubectl
apply` of the committed manifest, which carries none) would fail to reach `Ready`
with an `x509: certificate signed by unknown authority` TLS error against Quay;
always provision `my-project` through `scripts/apply-my-project` so the trust
anchor is injected.

## See also

- [ADR-18 — The Holos Controller](../adr/ADR-18.md) — the controller, its
  `holos-controller` namespace, and the AC #7 API-group dependency boundary.
- [ADR-19 — Quay API Group (`quay.holos.run`) CRDs](../adr/ADR-19.md) — the
  Organization/Repository schemas, the `credentialsSecretRef` design, the
  `url`/`urlSecretRef` webhook (AC #8), the repos-only-via-Repository rule
  (AC #9), and the conditions/reasons.
- [Quay Resource Controller credentials runbook](quay-resource-controller-credentials.md)
  — minting the superuser OAuth-Application token this controller consumes.
- [Quay↔Keycloak OIDC runbook](quay-keycloak-oidc.md) — the SSO/superuser model.
- [`holos/docs/secret-handling.md`](../../holos/docs/secret-handling.md) — the
  runtime-secret guardrail the credential Secret follows.

# Claude Code Guide for holos-paas

## Guard Rails

### CUE Component Rendering
- **Rule:** All changes to files under `holos/components/` MUST be followed by running `scripts/render` to regenerate the corresponding manifests under `holos/deploy/`.
- **Why:** The render script enforces that the committed deploy tree matches the CUE source exactly. Drift between source and deployed manifests can mask outdated or broken configurations.
- **How to apply:** After editing any `.cue` file:
  1. Commit the CUE changes
  2. Run `scripts/render` (it will fail if holos/ has uncommitted changes)
  3. Commit the regenerated YAML in `holos/deploy/` together with the source changes
  - See `holos/docs/component-guidelines.md` for full workflow details.

### Known Issues & Workarounds

#### Quay OIDC PKCE — disabled (HOL-1233, resolved by HOL-1257)
- **Issue:** Quay's OIDC client does not fully implement PKCE — it fails to send the code_verifier during token exchange, producing "code_verifier_missing" / `Got non-2XX response for code exchange: 400` SSO failures.
- **Resolution (HOL-1257):** PKCE is **disabled** for the `quay` client on both ends. The Keycloak `quay` client carries **no** `pkce.code.challenge.method` attribute (Keycloak treats a client that sets it as *requiring* PKCE), and Quay's `KEYCLOAK_LOGIN_CONFIG` no longer sets `USE_PKCE`/`PKCE_METHOD`. Quay authenticates as a plain confidential client with its client secret — Red Hat's recommended baseline integration.
- **History:** An earlier workaround kept PKCE *optional* (advertising `pkce.code.challenge.method: "S256"` while relying on Keycloak's default `pkce.force: "false"`). HOL-1257 superseded that by removing the attribute entirely, which is the current state.
- **Status:** Resolved. If a future Quay release fully implements PKCE, consider re-enabling it by restoring the `pkce.code.challenge.method: "S256"` attribute on the client and `USE_PKCE`/`PKCE_METHOD` in Quay's config.
- **Related:** `holos/components/keycloak/realm-config/buildplan.cue` (the `quay` client — no `attributes`), `holos/components/quay/buildplan.cue` (the `KEYCLOAK_LOGIN_CONFIG` block), `docs/adr/ADR-15.md` (Revision 2), `holos/docs/keycloak-clients.md` (the PKCE guardrail checklist), and `docs/runbooks/quay-keycloak-oidc.md` (the operational runbook, including the no-PKCE exception and the `code exchange: 400` troubleshooting).

### Keycloak Configuration as Code
- **Pattern:** The holos realm (users, groups, clients, roles, protocol mappers) is fully declarative, reconciled on every `scripts/apply` via a keycloak-config-cli Job.
- **Scope:** The Job imports only `realm: "holos"` — it does NOT manage `enabled` or `identity-provider` fields, which are owned by the KeycloakRealmImport CR in the instance component. This prevents contention between the two reconciliation paths.
- **Generate-once guarantee:** Secrets generated at runtime (e.g., Quay OIDC client secret) are created once and never rotated, so they remain stable across reconciles. Bootstrap Jobs idempotently check for existing secrets before creating.

### OIDC Client Secrets
- **Rule:** OIDC client secrets are generated at runtime, never committed.
- **Pattern:** A bootstrap Job generates the secret once and writes it to both the owning component's namespace and any consuming namespace (e.g., keycloak and quay for the Quay OIDC secret).
- **Reference:** `holos/components/keycloak/realm-config/buildplan.cue`, QUAY_OIDC_BOOTSTRAP section

### Project Delivery Scaffold (my-project pattern)
- **Pattern:** A project that receives Kargo-driven OCI delivery is laid down as a **single component** that emits, together, the project Namespace (registered centrally, **never** inline), a hand-authored Argo CD AppProject + OCI-source Application (with `kargo.akuity.io/authorized-stage` and `targetRevision` omitted so Kargo owns it), the Kargo Project/ProjectConfig/Warehouse/Stage, and a Quay org/repo/webhook/pull-robot **bootstrap Job**. The Kargo Project namespace doubles as the workload namespace (no separate `kargo-project-*` sibling — that split is only the echo spike). `my-project` is the reference instance and the template for a future self-service `ProjectRequest`.
- **Quay org/repo/webhook bootstrap Job convention:** Model it on `holos/components/quay/buildplan.cue`'s `BOOTSTRAP_JOB`/`BOOTSTRAP_SCRIPT` and `scripts/quay-init`. Run it in the **`quay` namespace** (the admin OAuth token in `quay-initial-admin` and the local-CA cert in `quay-local-ca` live there), make every step idempotent (check-then-create), write the Argo CD pull-credential repository Secret into `argocd`, and register a `repo_push` webhook pointing at the Kargo receiver URL read from `ProjectConfig.status`. Order the component **last** in `scripts/apply` (after `quay`, `argocd`, `kargo`, and any Kargo pipeline), with a `pre_*` Job-delete hook and a `wait_*` completion gate (the `pre_keycloak_config`/`wait_keycloak_config` precedent). The Application stays `Unknown`/`Missing` until the first config artifact is published — expected scaffolding.
- **Hand-authored Application vs. the deferred projection:** The sample Applications (`echo`, `my-project`) are hand-authored **OCI**-source Applications, distinct from the deferred per-component `argoAppDisabled` **git**-source projection (`holos/docs/placeholders.md` → *ArgoCD gitops delivery*). Do not conflate them.
- **Reference:** `holos/components/my-project/buildplan.cue`, `holos/README.md` (*The `my-project` delivery scaffold*), `holos/docs/oci-publish-workflow.md` (*Downstream: the `my-project` delivery scaffold*), `docs/adr/ADR-16.md`.

### Adding a Keycloak OIDC (PKCE) Client
- **Pattern:** The realm's OIDC clients (argocd, quay) are declared in `realm-config/buildplan.cue` and reconciled by the `keycloak-config` keycloak-config-cli Job. The conventional declarative-client pattern — public vs confidential decision, the `S256` attribute, the confidential secret-bootstrap Job, `IMPORT_VARSUBSTITUTION_ENABLED`, the three mappers that feed the shared `groups` claim, the role model, and the render-then-commit workflow — is documented as a guardrail checklist.
- **Before adding another PKCE client:** Read `holos/docs/keycloak-clients.md` and follow its guardrail checklist rather than rediscovering the pattern. Relax or skip requiring PKCE only for a client with a demonstrated implementation gap — the `quay` client is the documented exception (HOL-1257 disabled PKCE for it entirely; see the *Quay OIDC PKCE* note above, the runbook `docs/runbooks/quay-keycloak-oidc.md`, and `docs/adr/ADR-15.md`). The public `argocd` and `kargo` clients keep `pkce.code.challenge.method: "S256"`.
- **Reference:** `holos/docs/keycloak-clients.md`, `docs/runbooks/quay-keycloak-oidc.md`

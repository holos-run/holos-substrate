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

#### Quay OIDC PKCE Implementation (HOL-1233)
- **Issue:** Quay's OIDC client does not fully implement PKCE — it fails to send the code_verifier during token exchange, causing "code_verifier_missing" errors in Keycloak logs.
- **Workaround:** The Quay client in Keycloak is configured with `pkce.force: "false"` (optional PKCE) rather than required. This allows Quay to fall back to client-secret authentication if its PKCE implementation is incomplete.
- **Status:** Temporary workaround. Monitor Quay releases for PKCE fix; if fixed, consider re-enabling PKCE requirement.
- **Related:** `holos/components/keycloak/realm-config/buildplan.cue`, line 238

### Keycloak Configuration as Code
- **Pattern:** The holos realm (users, groups, clients, roles, protocol mappers) is fully declarative, reconciled on every `scripts/apply` via a keycloak-config-cli Job.
- **Scope:** The Job imports only `realm: "holos"` — it does NOT manage `enabled` or `identity-provider` fields, which are owned by the KeycloakRealmImport CR in the instance component. This prevents contention between the two reconciliation paths.
- **Generate-once guarantee:** Secrets generated at runtime (e.g., Quay OIDC client secret) are created once and never rotated, so they remain stable across reconciles. Bootstrap Jobs idempotently check for existing secrets before creating.

### OIDC Client Secrets
- **Rule:** OIDC client secrets are generated at runtime, never committed.
- **Pattern:** A bootstrap Job generates the secret once and writes it to both the owning component's namespace and any consuming namespace (e.g., keycloak and quay for the Quay OIDC secret).
- **Reference:** `holos/components/keycloak/realm-config/buildplan.cue`, QUAY_OIDC_BOOTSTRAP section

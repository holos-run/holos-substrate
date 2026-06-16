# Secret-handling guardrails: secrets are created at runtime, never committed

This is a **binding guardrail** for humans and AI coding agents working on the
Holos deployment configuration. It exists so that an ambiguous acceptance
criterion about a `Secret` is always resolved the same way — directly, at
runtime — instead of being deferred to human review. It was made binding in
HOL-1274 after HOL-1270 deferred exactly this kind of AC.

## Rule

Secrets MUST be created at runtime and MUST NOT be committed to this repository
— not as data, not as empty-data placeholders, not as base64 stubs. The deploy
tree under `holos/deploy/` never contains a `Secret`'s sensitive material.

Two runtime mechanisms are sanctioned, in order of preference:

1. **`ExternalSecret`** (external-secrets.io) when the value already lives in, or
   belongs in, an external secret store and a `SecretStore`/`ClusterSecretStore`
   is available to project it. The `ExternalSecret` resource is committed (it
   carries no secret material); the operator materializes the `Secret` at
   runtime. The CUE schema already admits these (`holos/resources.cue`).
2. **A create-if-absent bootstrap `Job`** when the value is generated in-cluster
   and there is no external source — the established pattern in this repo. The
   Job generates the value **once**, checks for an existing `Secret` first, and
   leaves it untouched if present (generate-once = stable across re-applies). The
   token is piped to `kubectl create -f -` on stdin so it never appears in the
   container's argv.

A committed empty-data `Secret` is specifically forbidden: `scripts/apply` would
create it before the bootstrap Job runs, tripping the Job's create-if-absent
guard and leaving the value permanently empty.

## Resolving an ambiguous or literal AC about a Secret

When a plan's acceptance criterion says a `Secret` should "render", "be
committed", "appear under `holos/deploy/`", or names a specific key — and
satisfying it literally would require committing secret material or a placeholder
— **resolve it directly by handling the Secret at runtime**. This is the
unambiguous resolution of that ambiguity:

- Do **not** mark the issue `needs-human-review` and do **not** leave it open for
  a human to confirm the tradeoff. The tradeoff is already decided by this
  guardrail.
- Implement the runtime mechanism (ExternalSecret or bootstrap Job), note in the
  PR that the literal "renders a committed Secret" wording is satisfied by the
  runtime creation per this guardrail, and close the issue.
- The Argo CD `Application` or consuming resource staying `Unknown`/`Missing`
  until the bootstrap Job first runs is expected scaffolding, not a deferral.

## Key naming: write exactly the keys the consumer reads

The bootstrap Job (or ExternalSecret) writes **only** the key(s) the consumer
actually reads — verified against the consumer's contract (e.g. the vendored CRD
for a Kargo webhook receiver). Never add an extra key "for AC compliance" or
"for forward-compatibility": an unread key is dead code that drifts from the
contract and misleads the next reader. If an AC names a key that the consumer
does not read, the AC is wrong — use the correct key and remove the named one.

## Precedents in this repo

- **Quay webhook receiver token** —
  `holos/components/my-project/buildplan.cue` (`WEBHOOK_BOOTSTRAP_SCRIPT`): a
  create-if-absent Job generates the receiver's shared token under the single
  `secret` key the Kargo quay receiver reads.
- **Quay OIDC client secret** — `holos/components/keycloak/realm-config/`
  bootstrap Job: generated once, written into both the owning and consuming
  namespaces (see CLAUDE.md "OIDC Client Secrets").
- **Quay secret-keys** — `holos/components/quay/` bootstrap Job: the original
  generate-once, create-if-absent precedent this guardrail generalizes.

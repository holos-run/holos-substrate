# Runbook: External Resource Rename and Transfer

Operator-facing guide for renaming or transferring a `holos.run` custom
resource that fronts a durable external object in Quay or Keycloak. Use this
procedure when the Kubernetes object name changes but the external identity must
survive.

The supported transfer flow is:

1. Patch the old CR with `spec.deletionPolicy: Orphan`.
2. Delete the old CR.
3. Apply the new CR with the same immutable external identity fields and
   `spec.adopt: true`.
4. Optionally patch the new CR with `spec.deletionPolicy: Delete`.

Step 4 is required when the new CR should regain delete authority. Omitted
`deletionPolicy` follows provenance: created resources are deleted, but adopted
resources are released by default.

Do not run this while Argo CD or another GitOps loop would immediately recreate
the old CR. Update or pause the rendered manifests first. For project/application
template resources, make the rename in the CUE registration and run
`scripts/render` before applying the transfer.

## Identity Fields

Carry these fields verbatim to the new CR:

| Kind | External identity fields |
| ---- | ------------------------ |
| `quay.holos.run/Organization` | `spec.name` |
| `quay.holos.run/Repository` | `spec.name`, plus `spec.organizationRef` to the Organization CR that resolves to the same Quay organization. Carry it verbatim for a Repository-only rename; update it when transferring repositories as part of an Organization CR rename. |
| `keycloak.holos.run/KeycloakGroup` | `spec.instanceRef` and `spec.path` |
| `keycloak.holos.run/KeycloakUser` | `spec.instanceRef`, `spec.email`, and `spec.username` when set |
| `keycloak.holos.run/KeycloakClient` | `spec.instanceRef` and `spec.clientId` |

`KeycloakGroupMembership` has no single ownable external object to adopt. Recreate
the membership CR under the new name. Use `deletionPolicy: Orphan` on the old CR
when the membership edges must remain during the swap.

## Per-Kind Verification

After applying the new CR with `adopt: true`, verify `Ready=True` and the
provenance/identity status for the kind you transferred.

Quay Organization:

```bash
kubectl -n my-project get organization new-org \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.created}{"\n"}'
```

Quay Repository:

```bash
kubectl -n my-project get repository new-repo \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.created}{"\n"}{.status.quayRepository}{"\n"}{.status.webhookNotificationUUID}{"\n"}'
```

KeycloakGroup:

```bash
kubectl -n my-project get keycloakgroup new-owner \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.created}{"\n"}{.status.adopted}{"\n"}{.status.groupID}{"\n"}'
```

KeycloakUser:

```bash
kubectl -n my-project get keycloakuser new-user \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.created}{"\n"}{.status.adopted}{"\n"}{.status.userID}{"\n"}'
```

KeycloakClient:

```bash
kubectl -n my-project get keycloakclient new-client \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.created}{"\n"}{.status.adopted}{"\n"}{.status.clientUUID}{"\n"}'
```

For an adopted transfer, Quay `status.created` should be `false`. Keycloak
resources should report `status.created=false`, `status.adopted=true`, and the
pinned UUID field (`groupID`, `userID`, or `clientUUID`) should be populated.

## General Procedure

Patch the old CR:

```bash
kubectl -n my-project patch organization old-org \
  --type merge \
  -p '{"spec":{"deletionPolicy":"Orphan"}}'
```

Delete the old CR:

```bash
kubectl -n my-project delete organization old-org
```

Apply the new CR with the same external identity and `adopt: true`:

```yaml
apiVersion: quay.holos.run/v1alpha1
kind: Organization
metadata:
  name: new-org
  namespace: my-project
spec:
  name: my-project
  email: owner@example.com
  adopt: true
```

Verify the new CR is ready and adopted:

```bash
kubectl -n my-project get organization new-org \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.created}{"\n"}'
```

For adopted Quay and Keycloak resources, `status.created` should be `false`. For
Keycloak resources, also check the adopted flag:

```bash
kubectl -n my-project get keycloakgroup new-owner \
  -o jsonpath='{.status.adopted}{"\n"}{.status.groupID}{"\n"}'
```

Restore delete authority when the new CR should own destructive cleanup:

```bash
kubectl -n my-project patch organization new-org \
  --type merge \
  -p '{"spec":{"deletionPolicy":"Delete"}}'
```

## Organization Example

Rename the Kubernetes `Organization` object while preserving the Quay
organization named `my-project`:

```bash
kubectl -n my-project patch organization old-org \
  --type merge \
  -p '{"spec":{"deletionPolicy":"Orphan"}}'
kubectl -n my-project delete organization old-org
kubectl -n my-project apply -f organization-new-org.yaml
kubectl -n my-project wait organization/new-org \
  --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True \
  --timeout=2m
kubectl -n my-project get organization new-org \
  -o jsonpath='{.status.created}{"\n"}'
```

`organization-new-org.yaml` must keep the same `spec.name` and set `spec.adopt:
true`. If the new CR should delete the Quay organization when it is later
removed, patch `spec.deletionPolicy: Delete` after adoption is Ready.

## KeycloakGroup Example

Rename a `KeycloakGroup` CR while preserving the Keycloak group at the same path:

```bash
kubectl -n my-project patch keycloakgroup old-owner \
  --type merge \
  -p '{"spec":{"deletionPolicy":"Orphan"}}'
kubectl -n my-project delete keycloakgroup old-owner
kubectl -n my-project apply -f keycloakgroup-new-owner.yaml
kubectl -n my-project wait keycloakgroup/new-owner \
  --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True \
  --timeout=2m
kubectl -n my-project get keycloakgroup new-owner \
  -o jsonpath='{.status.created}{"\n"}{.status.adopted}{"\n"}{.status.groupID}{"\n"}'
```

`keycloakgroup-new-owner.yaml` must keep the same `spec.path` and set
`spec.adopt: true`. Patch `spec.deletionPolicy: Delete` only if the new CR should
delete the Keycloak group after verifying the pinned `status.groupID`.

## Reference Cascades

Renaming an `Organization` CR forces a transfer of its `Repository` CRs.
`Repository.spec.organizationRef` names the Organization CR, not only the Quay
organization. Orphan the old repositories, delete them, and recreate them with
the new `organizationRef`, the same `spec.name`, and `spec.adopt: true`.

Organization `spec.syncedTeams[]` entries do not transfer automatically. Orphaning
the Organization strips the org marker, but team descriptions still carry the old
CR's managed-team marker. A replacement Organization that declares those teams
will report `Ready=False` with `TeamConflict`, even if each entry sets `adopt:
true`, because per-team adoption only claims unmarked teams. During an
Organization transfer, either leave `spec.syncedTeams[]` out of the replacement
until the old team markers have been deliberately cleared or reassigned by an
operator, or restore the old CR and perform a different handoff. There is no
per-team `deletionPolicy: Orphan` field.

Renaming a `KeycloakClient` CR requires updating every `clientRef` that points to
the old CR name. Check `KeycloakGroup.spec.clientRoles[]` and
`KeycloakClient.spec.clientRoles[]`, then apply those referencing CR changes with
the client transfer.

Cross-namespace moves also change which namespace owns RBAC, ReferenceGrants, and
same-namespace references. Confirm any `security.holos.run/ReferenceGrant`
authorization and repository organization references before applying the new CR.

## Caveats

Legacy adopted Quay organizations that do not yet have a kind-prefixed ownership
marker must reconcile once before explicit `Delete` can remove the external org.
That reconcile heals the marker to `adopted:<uid>`.

Orphaned repositories keep their `repo_push` webhook. This is intentional; the
adopting Repository CR claims the existing webhook and retitles it instead of
creating a duplicate.

`deletionPolicy: Orphan` on Keycloak resources makes no Keycloak calls. It is the
escape hatch when the backing realm or credential is gone.

For confidential `KeycloakClient` resources, the delivered Secret named by
`spec.secretRef` is Kubernetes state owned by the old CR, not part of the remote
Keycloak client identity. Reusing the same Secret name before ownership is
resolved will surface as a Secret collision. Use a new `secretRef` for the
replacement or deliberately transfer/remove the old Secret owner reference before
expecting the replacement to deliver the client secret.

## Rollback and Conflict Inspection

If adoption reports `Ready=False` with a conflict reason, inspect the remote
ownership marker before retrying.

For Quay organizations, inspect the marker robot named `<org>+holos-owner`; its
description is expected to be `created:<uid>` or `adopted:<uid>` for the owning
CR. For repositories, inspect the repository description and look for the
`holos-owner:` line. A foreign UID means another CR owns the external resource.

Resolve the conflict by restoring the old CR, transferring from the owning CR, or
choosing a different external identity. Do not remove a foreign marker unless the
platform owner has confirmed that the previous owner is abandoned.

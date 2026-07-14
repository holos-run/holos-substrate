# Argo CD health for `holos.run` resources

Argo CD must assess a custom resource from the controller's status rather than
from the fact that Kubernetes accepted its manifest. The
`keycloak.holos.run` APIs use the common `Ready` condition and
`status.observedGeneration` contract, so one health check covers all five
kinds: `Instance`, `Group`, `GroupMembership`, `User`, and `Client`.

## Health mapping

The Argo CD `argocd-cm` ConfigMap contains an aggregate
`resource.customizations` entry for `keycloak.holos.run/*`. Argo CD requires
this aggregate form for wildcard group/kind matches; the split
`resource.customizations.health.<group>_<kind>` ConfigMap keys do not support
wildcards.

| Resource status | Argo CD health | Message |
| --- | --- | --- |
| `Ready=True`, with a current `observedGeneration` when both generations are present | `Healthy` | The Ready condition message, falling back to its reason |
| `Ready=False` | `Degraded` | The Ready condition message, falling back to its reason |
| `Ready=Unknown` | `Progressing` | The Ready condition message, falling back to its reason |
| No status, no conditions, or no Ready condition | `Progressing` | Waiting for the Ready condition |
| `status.observedGeneration` differs from `metadata.generation` | `Progressing` | Waiting for the controller to observe the latest generation |

The generation check prevents an old `Ready=True` condition from making a
newly edited resource appear healthy before the controller reconciles it.

## Lua health check

The shared script is defined in
`holos/components/argocd/controller/buildplan.cue` and rendered into
`argocd-cm`:

```lua
hs = {}
hs.status = "Progressing"
hs.message = "Waiting for the Ready condition"

if obj.status == nil or obj.status.conditions == nil then
  return hs
end

if obj.status.observedGeneration ~= nil and
   obj.metadata ~= nil and
   obj.metadata.generation ~= nil and
   obj.status.observedGeneration ~= obj.metadata.generation then
  hs.message = "Waiting for the controller to observe the latest generation"
  return hs
end

for _, condition in ipairs(obj.status.conditions) do
  if condition.type == "Ready" then
    if condition.message ~= nil and condition.message ~= "" then
      hs.message = condition.message
    elseif condition.reason ~= nil and condition.reason ~= "" then
      hs.message = condition.reason
    end

    if condition.status == "True" then
      hs.status = "Healthy"
    elseif condition.status == "False" then
      hs.status = "Degraded"
    end
    return hs
  end
end

return hs
```

## Verification

After deploying the rendered Argo CD component, confirm the wildcard and Lua
script are present:

```sh
kubectl -n argocd get configmap argocd-cm \
  -o jsonpath='{.data.resource\.customizations}'
```

Inspect an Application that owns `keycloak.holos.run` resources:

```sh
argocd app get <application>
argocd app resources <application> --group keycloak.holos.run
```

The resource health column and Argo CD UI should follow the mapping above.
Compare a resource's source status when diagnosing a mismatch:

```sh
kubectl get <kind>.keycloak.holos.run <name> -n <namespace> \
  -o jsonpath='{.metadata.generation}{"\n"}{.status.observedGeneration}{"\n"}{.status.conditions}{"\n"}'
```

Before committing a customization change, render twice around the generated
manifest commit as required by the repository guardrail:

```sh
scripts/render
git diff --exit-code -- holos/deploy
```

## Extending the pattern

Future `holos.run` API groups that implement the same `Ready` and
`observedGeneration` contract can reuse this script. Add another wildcard GVK
under the marshalled `RESOURCE_CUSTOMIZATIONS` value, for example
`quay.holos.run/*`, and point its `health.lua` field at the shared script.
Review the new group's status contract first; a group with different condition
semantics needs its own health check rather than a broad wildcard.

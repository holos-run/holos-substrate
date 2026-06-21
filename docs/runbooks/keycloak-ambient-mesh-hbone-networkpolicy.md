# Runbook: Ambient mesh HBONE blocked by a NetworkPolicy (port 15008)

Operational runbook for a class of failure that breaks **in-cluster
connectivity to a workload after its namespace is enrolled in the Istio ambient
mesh**, caused by a `NetworkPolicy` that does not permit ztunnel HBONE traffic
on TCP **15008**. Written for platform engineers and SREs: the symptom, an
efficient way to confirm the diagnosis, the remediation, and how to prevent it
when standing up new clusters or adding operator-managed components.

This has bitten us on more than one cluster. The worked example is Keycloak
(the operator ships its own `NetworkPolicy`), fixed in HOL-1370, but the failure
mode is **general** — any component whose chart or operator installs a
restrictive `NetworkPolicy` into an ambient namespace is a candidate.

Companion to [Ambient Mesh Enrollment](../../holos/docs/mesh-enrollment.md)
(how namespaces enroll and how to verify enrollment).

## TL;DR

In ambient mode, ztunnel does **not** hand meshed traffic to a pod on its
application port. It delivers the connection to the pod over **HBONE** (mTLS) on
TCP **15008**, then forwards it to the app port on loopback *inside* the pod. A
`NetworkPolicy` that selects the pod and lists only the app ports therefore
**silently drops every meshed connection** — the app port rule never matches
mesh traffic, because mesh traffic arrives on 15008.

> **Any NetworkPolicy applied to a pod in an ambient namespace must permit
> inbound TCP 15008**, or all meshed clients lose connectivity to it (the app
> port rules are irrelevant to mesh traffic).

## Symptom

The most common signature is the Keycloak `keycloak-config-cli` import Jobs
(`keycloak-config`, `keycloak-esso-config`) failing to reach Keycloak, which
makes `scripts/apply` time out waiting on the Job:

```text
ERROR: timed out after 300s waiting for job/keycloak-esso-config-... to complete.
```

```text
d.a.k.config.provider.KeycloakProvider : Wait 120 seconds until http://keycloak-service:8080 is available ...
d.a.k.config.KeycloakConfigRunner      : Error during Keycloak import: Could not connect to keycloak in 120 seconds:
  RESTEASY004655: Unable to invoke request: java.net.SocketException: Connection reset
```

Generalized, the symptom is: **a client pod in (or routed through) the mesh gets
`Connection reset` / `Connection refused` to a Service whose backend pods are
healthy and `Ready`**, while the same Service works from outside the mesh.

A strong tell that distinguishes this from an app-down problem:

- The destination pod is `Running`/`Ready` and its app port answers when probed
  **from a non-meshed namespace** (e.g. `default` with no
  `istio.io/dataplane-mode` label).
- The same request **from a meshed namespace** is reset.

## Why it happens

ztunnel listens, inside each enrolled pod's network namespace, on:

| Port  | Purpose                                              |
|-------|-----------------------------------------------------|
| 15008 | **HBONE inbound** (mTLS) — how meshed peers reach the pod |
| 15006 | inbound plaintext capture (redirected app traffic)  |
| 15001 | outbound capture                                    |
| 15053 | DNS proxy                                           |

A meshed client → ztunnel (source) → **`dst:15008`** on the destination pod →
ztunnel (dest) → app port on loopback. If a `NetworkPolicy` selecting the
destination pod omits 15008, the CNI drops the SYN to 15008 and the source
ztunnel reports `Connection refused`, surfaced to the application as
`Connection reset`.

Non-mesh traffic is unaffected because it never uses 15008 — it hits the app
port directly (and the app-port rule, if present, admits it). That is exactly
why "it works from outside the mesh but not inside."

### The Keycloak instance (HOL-1370)

The Keycloak operator creates a `NetworkPolicy` named `keycloak-network-policy`
(owned by the `Keycloak` CR) that admits ingress **only** on `8080` (http),
`9000` (management), and `7800`/`57800` (JGroups, from peer pods). It has no
15008 rule, and the operator's CRD schema **cannot express one** —
`spec.networkPolicy` on the `Keycloak` CR only lets you *narrow the sources* of
those fixed ports, not add a port. So the operator's policy cannot be made
ambient-compatible by configuration.

The fix ships in the `keycloak-instance` component
([buildplan.cue](../../holos/components/keycloak/instance/buildplan.cue),
resource `HBONE_NETWORK_POLICY`): an **additive** sibling `NetworkPolicy`
(`keycloak-allow-ztunnel-hbone`) that admits TCP 15008 to the same Keycloak
server pods. `NetworkPolicy` rules are a **union** across all policies that
select a pod, so this restores HBONE without fighting the operator — its
app-port rules stay intact for the kubelet probe path. The source is left
unrestricted because ztunnel authenticates every HBONE peer with mTLS. This
mirrors the optional 15008 allow-policies Istio's own ztunnel/cni/istiod charts
ship for NetworkPolicy-enabled clusters.

## Efficient diagnosis

Work top-down; each step is a few seconds and rules in or out a layer.

### 1. Confirm the pod is actually enrolled (not an enrollment bug)

```bash
istioctl ztunnel-config workloads | grep <namespace>
```

Enrolled pods report protocol `HBONE`. If they show `TCP`, this is an
*enrollment* problem, not a NetworkPolicy one — see
[mesh-enrollment.md](../../holos/docs/mesh-enrollment.md). If they show `HBONE`,
continue.

### 2. Probe 15008 vs the app port directly (the decisive test)

Pick a backend pod IP, then probe its HBONE port and its app port from a
throwaway pod. **15008 refused while the app port is open ⇒ a NetworkPolicy is
blocking HBONE.**

```bash
NS=keycloak
POD_IP=$(kubectl get pod -n "$NS" -l app=keycloak \
  -o jsonpath='{.items[0].status.podIP}')

kubectl run hbone-check --rm -it --restart=Never --image=busybox:1.36 -- \
  sh -c "for p in 15008 8080; do
           nc -z -w3 $POD_IP \$p && echo \"\$p OPEN\" || echo \"\$p REFUSED\";
         done"
```

```text
15008 REFUSED      # <- HBONE blocked
8080 OPEN          # <- app port still reachable (kubelet probes, non-mesh)
```

For contrast, the same probe against a healthy meshed pod (e.g.
`keycloak-operator`) returns `15008 OPEN`.

### 3. Confirm at the source from ztunnel logs

The source-side ztunnel logs the failure unambiguously — note `dst.addr`/
`dst.hbone_addr` ending in `:15008` and `error="...Connection refused"`:

```bash
kubectl logs -n istio-system -l app=ztunnel --tail=2000 \
  | grep -E ':15008' | grep -i 'connection refused' | tail
```

```text
... src.workload="keycloak-esso-config-..." dst.hbone_addr=10.42.0.22:8080
    dst.workload="keycloak-0" direction="outbound"
    error="io error: Connection refused (os error 111)"
```

### 4. Inspect the NetworkPolicies on the namespace

```bash
kubectl get networkpolicy -n <namespace>
kubectl get networkpolicy -n <namespace> -o yaml \
  | grep -A3 -E 'ports:|port:'
```

You are looking for a policy that selects the destination pods and whose
`ingress.ports` list does **not** include `15008`. (There may be a legitimate
operator/chart-owned policy plus our additive allow-policy; the bug is when the
additive one is missing.)

## Remediation

The standing fix is in source — a fresh `scripts/apply` renders and applies the
additive `NetworkPolicy` as part of `keycloak-instance`. If you are firefighting
a live cluster *before* re-applying, apply the allow-policy directly:

```bash
kubectl apply -n keycloak -f \
  holos/deploy/clusters/<cluster>/components/keycloak/networkpolicy-keycloak-allow-ztunnel-hbone.yaml
```

Or, for a namespace that has no rendered policy yet, the minimal hotfix is:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: <workload>-allow-ztunnel-hbone
  namespace: <namespace>
spec:
  podSelector:
    matchLabels: { <selector matching the backend pods> }
  policyTypes: ["Ingress"]
  ingress:
    - ports:
        - { protocol: TCP, port: 15008 }
```

No pod restart is needed — `NetworkPolicy` changes take effect on the CNI
immediately. Re-run step 2 to confirm `15008 OPEN`, then re-drive the stuck Job:

```bash
kubectl delete job -n keycloak <job-name>          # then re-apply its manifest
kubectl wait --for=condition=complete job/<job-name> -n keycloak --timeout=180s
```

## Prevention

When standing up a new cluster or adding a component to an **ambient** namespace:

- **Audit chart/operator-shipped NetworkPolicies.** After enrolling a namespace,
  run `kubectl get networkpolicy -n <ns>`. If a chart or operator installs one,
  confirm it permits 15008 — or add an additive allow-policy. The Keycloak
  operator is the known offender; treat any operator that manages its own
  `NetworkPolicy` with suspicion.
- **Prefer additive allow-policies over disabling the vendor policy.** A union
  policy that opens 15008 preserves the vendor's app-port restrictions (and the
  kubelet probe path) while restoring mesh connectivity. Disabling the vendor
  policy outright is a bigger blast radius.
- **Don't try to express 15008 through the operator's own knobs** unless its CRD
  supports an arbitrary ingress port. Keycloak's `spec.networkPolicy` only
  narrows sources of its fixed ports; a sibling policy is the supported path.
- **Smoke test after enrollment.** The fastest signal is the step-2 probe:
  `15008 OPEN` on a backend pod means the mesh data path is intact.

## References

- [HOL-1370 fix — `keycloak-instance` HBONE allow-policy](../../holos/components/keycloak/instance/buildplan.cue)
  (`HBONE_NETWORK_POLICY`) and the `NetworkPolicy` entry in
  [holos/resources.cue](../../holos/resources.cue).
- [Ambient Mesh Enrollment](../../holos/docs/mesh-enrollment.md) — enrollment
  convention, `istioctl ztunnel-config workloads`, and the keycloak
  partial-enrollment nuance (CNPG pods opt out).
- Istio ambient + Kubernetes `NetworkPolicy` guidance: meshed pods must permit
  inbound TCP 15008 (HBONE); Istio's own ztunnel/cni/istiod charts ship optional
  policies that do exactly this.

# Ambient Mesh Enrollment

How platform namespaces enroll their workloads in the Istio ambient mesh, how
to verify enrollment, and which namespaces are deliberately not enrolled.
Written for component authors; the worked example is the
[`echo`](../components/echo/) component, the permanent Layer 0 smoke test.

## The convention

Platform namespaces carrying workloads **MUST** set the ambient dataplane
label on their Namespace resource:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: echo
  labels:
    istio.io/dataplane-mode: ambient
```

The label enrolls the namespace's pods in the ambient mesh: istio-cni
redirects the pods' traffic to the ztunnel node proxy, which carries it over
HBONE (HTTP-Based Overlay Network Environment) with mutual TLS. Workloads
need no sidecar and no restart-ordering relationship with the mesh — the
label is the entire enrollment. An individual pod can still opt back out with
the pod-level label `istio.io/dataplane-mode: none`, which takes precedence
over the namespace label (see the keycloak partial-enrollment note below,
where the CNPG `keycloak-db` pods use exactly this).

Namespaces are declared in the central namespaces registry
([`holos/namespaces.cue`](../namespaces.cue)), rendered by the
[`namespaces`](../components/namespaces/) component — register a namespace
and its enrollment label there, not inline in a component. Each registry
entry declares enrollment deliberately; the exceptions below document the
namespaces that are deliberately not enrolled. The registry is the only
place Namespace resources are rendered — no component emits an inline copy.

## Verifying enrollment

List the workloads ztunnel has captured; enrolled pods report protocol
`HBONE`:

```bash
istioctl ztunnel-config workloads
```

```text
NAMESPACE  POD NAME                    ADDRESS    NODE                PROTOCOL
echo       echo-86c9c66d8d-2hqxv       10.42.0.x  k3d-holos-server-0  HBONE
```

Pods in unenrolled namespaces appear with protocol `TCP` (ztunnel knows of
them but does not capture their traffic). Alternatively, check the ztunnel
logs for the pod's enrollment events:

```bash
kubectl logs -n istio-system -l app=ztunnel | grep <pod-name>
```

## Exceptions: workload namespaces that are not enrolled

The convention above applies to namespaces **carrying workloads**. Two such
namespaces are deliberately left out:

- **`istio-system`** — hosts the mesh dataplane and control plane themselves:
  istiod, istio-cni, and ztunnel. ztunnel is the node proxy that implements
  enrollment; redirecting its own traffic (or the control plane it
  synchronizes with) through itself is circular and unsupported. The mesh
  infrastructure secures its own control-plane connections natively.
- **`istio-gateways`** — the auto-provisioned gateway pods are Envoy proxies
  themselves and terminate mesh traffic natively, so redirecting them through
  ztunnel adds nothing. See the registry entry in
  [`holos/namespaces.cue`](../namespaces.cue).

Separately, a few **config/control-only** namespaces also carry
`_ambient: false` — `kargo-system-resources`, `kargo-shared-resources`,
`kargo-cluster-secrets`, and `kargo-echo`. These hold only configuration,
credential, or Kargo control objects (`Warehouse`/`Stage`/`Freight`/`Promotion`)
referenced by the enrolled `kargo` namespace, not pods, so there is no traffic
for ztunnel to capture. They are not mesh exceptions in the same sense as the
two above; see their rationale comments in
[`holos/namespaces.cue`](../namespaces.cue).

There is no `keycloak` exception. Keycloak **is** enrolled in the ambient mesh
like any other workload namespace (`keycloak: _ambient: true`); see the
partial-enrollment note below for the one CNPG nuance.

## Partial enrollment: keycloak (HOL-1362)

The `keycloak` namespace is enrolled in the ambient mesh, but its CNPG Postgres
pods are deliberately kept out — an exception scoped to a single workload, not
the whole namespace.

Keycloak used to be a full namespace-level exception: it terminated its own TLS
with a cert-manager `keycloak-tls` certificate, and a Gateway→Keycloak
`DestinationRule` re-encrypted the hop, because the reference platform's
root-cause analysis found ztunnel ambient interception broke Keycloak **when
Keycloak terminated its own TLS**. HOL-1362 removed that double TLS: Keycloak now
runs HTTP-only behind the shared Gateway, which terminates external TLS **once**
and forwards plaintext HTTP to `keycloak-service:8080`. ztunnel wraps that
Gateway→pod hop in HBONE mTLS, so the cross-namespace traffic is encrypted on the
wire without a per-pod TLS keystore — and with single TLS at the Gateway the RCA
failure mode is gone. The `keycloak-tls` `Certificate` and the Gateway→Keycloak
`DestinationRule` no longer exist. Keycloak resolves its own issuer
(`auth.holos.internal`) through CoreDNS in-cluster, so it needs **no** per-namespace
`ServiceEntry` for self-resolution.

The CNPG Postgres pods in this namespace (`keycloak-db`, see the
[`cnpg-clusters`](../components/cnpg-clusters/) component) stay **out** of the
mesh at the pod level: the `Cluster` sets `istio.io/dataplane-mode: none` via its
`spec.inheritedMetadata.labels`, so the plaintext Keycloak↔Postgres hop to
`keycloak-db-rw:5432` is not re-wrapped by ztunnel. Only those database pods are
excluded; the Keycloak server pods are enrolled. See the registry entry in
[`holos/namespaces.cue`](../namespaces.cue).

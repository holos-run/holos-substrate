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

The label enrolls every pod in the namespace in the ambient mesh: istio-cni
redirects the pods' traffic to the ztunnel node proxy, which carries it over
HBONE (HTTP-Based Overlay Network Environment) with mutual TLS. Workloads
need no sidecar and no restart-ordering relationship with the mesh — the
label is the entire enrollment.

Namespaces are declared in the central namespaces registry
([`holos/namespaces.cue`](../namespaces.cue)), rendered by the
[`namespaces`](../components/namespaces/) component — register a namespace
and its enrollment label there, not inline in a component. Each registry
entry declares enrollment deliberately; the exceptions below document the
namespaces that are deliberately not enrolled. Some components (`echo`,
`istio-base`, `istio-gateway`) still emit a transitional inline copy of
their Namespace; those copies converge under server-side apply and are
slated for removal (HOL-1162).

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

## Exceptions: namespaces that are not enrolled

- **`istio-system`** — hosts the mesh dataplane and control plane themselves:
  istiod, istio-cni, and ztunnel. ztunnel is the node proxy that implements
  enrollment; redirecting its own traffic (or the control plane it
  synchronizes with) through itself is circular and unsupported. The mesh
  infrastructure secures its own control-plane connections natively.
- **`istio-gateways`** — the auto-provisioned gateway pods are Envoy proxies
  themselves and terminate mesh traffic natively, so redirecting them through
  ztunnel adds nothing. See the registry entry in
  [`holos/namespaces.cue`](../namespaces.cue).

# holos-authenticator component

Deploys the **Holos Authenticator** ([ADR-23](../../../docs/adr/ADR-23.md)) into
the platform and wires it as an Istio external authorizer in the ambient mesh.

The authenticator is a controller-runtime manager that runs an **Envoy
`ext_authz` gRPC server** and reconciles `authenticator.holos.run` **Backend**
custom resources. Each `Backend` fronts one Kubernetes API server with OIDC
token validation and Kubernetes impersonation: on a valid token the authorizer
returns an OK response that sets `Impersonate-User` / `Impersonate-Group`
headers and replaces the caller's `Authorization` with the backend's privileged
credential, so Envoy forwards the request straight to the API server.

## What this component renders

`buildplan.cue` emits, into
`holos/deploy/clusters/<cluster>/components/holos-authenticator/`:

- the manager **Deployment** — image
  `quay.holos.internal/holos/holos-authenticator:dev`, `POD_NAMESPACE` via the
  downward API, gRPC `:9000`, metrics `:8080`, health `:8081` (the flag/port
  contract in `cmd/holos-authenticator/main.go`);
- a **ServiceAccount** (`holos-authenticator`);
- the authenticator **ClusterRole** + **ClusterRoleBinding**
  (`holos-authenticator-role`/`-rolebinding`, from the generated
  `config/authenticator/rbac/role.yaml`) and the namespaced **Role** +
  **RoleBinding** granting the manager `get` on Secrets in its own namespace
  (the per-Backend `credentialsSecretRef`);
- a **Service** exposing the gRPC and metrics ports;
- the generated **Backend CRD** (vendored from
  `config/crd/bases/authenticator.holos.run_backends.yaml`);
- an **AuthorizationPolicy** with `action: CUSTOM` and
  `provider.name: holos-authenticator`, matching the Istio extension provider;
- one example **Backend** CR.

No `Namespace` is emitted: the `holos-authenticator` namespace is owned by the
central registry (`holos/namespaces.cue`) and rendered by the `namespaces`
component.

## Istio extension provider

`holos/components/istio/istio.cue` declares the matching gRPC `ext_authz`
provider in `IstioValues.meshConfig.extensionProviders`:

```cue
meshConfig: extensionProviders: [{
  name: "holos-authenticator"
  envoyExtAuthzGrpc: {
    service: "holos-authenticator.holos-authenticator.svc.cluster.local"
    port:    9000
    timeout: "2s"
  }
}]
```

The `istiod` component passes `IstioValues` verbatim as Helm values, so the
provider lands in the mesh `MeshConfig`. The component's `AuthorizationPolicy`
references it by name (`provider.name: holos-authenticator`).

> **Ambient / L7 enforcement requires a waypoint.** ztunnel is L4-only; an
> `ext_authz` decision is an L7 concern, so a **waypoint** must front the
> protected workload for the `CUSTOM` AuthorizationPolicy to take effect. The
> example policy selects the authenticator's own pods as a harmless default; a
> real deployment retargets the selector at the protected workload behind a
> waypoint. The full waypoint / `ServiceEntry` topology for an **external** API
> server target is a deferred follow-up — see `holos/docs/placeholders.md`
> (finalized in the next phase).

## Impersonation RBAC (the impersonator credential)

The credential named by a `Backend`'s `spec.credentialsSecretRef` is the
**impersonator identity** the upstream API server authenticates Envoy as. It
**must** hold RBAC that grants `impersonate` on `users` and `groups` (and, if a
mapped group is a system group, on the relevant subresources). For example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: holos-authenticator-impersonator
rules:
  - apiGroups: [""]
    resources: ["users", "groups"]
    verbs: ["impersonate"]
```

- **In-cluster API server:** bind that ClusterRole to a ServiceAccount and store
  its token as the credential Secret.
- **External API server:** the credential is provisioned out-of-band (a
  kubeconfig/token for a principal that holds the impersonator ClusterRole on
  that cluster) and stored as the Secret named by `credentialsSecretRef`.

### Runtime Secret handling

Per the **Runtime Secret Handling** guardrail, the impersonator credential's
**material is never committed**. The component renders only the example
`Backend` CR (which *names* `holos-authenticator-backend-creds` via
`credentialsSecretRef`); the Secret itself is created at runtime in the
`holos-authenticator` namespace, out of band. The example `Backend` likewise
omits the `caBundle` fields so the committed manifest carries no per-cluster
trust material; an operator injects the local-ca PEM out of band (mirroring the
caBundle convention the project/application components use).

## Apply ordering

Registered in `holos/platform/platform.cue`, the `SYSTEM_COMPONENTS` list in
`holos/components/app-of-apps/buildplan.cue`, and the `COMPONENTS` array in
`scripts/apply` — all **after the Istio data-plane components**
(`istio-base`/`istiod`/`istio-cni`/`istio-ztunnel`/`istio-gateway`): the
`ext_authz` provider is part of `istiod`'s `MeshConfig`, and the namespace is
ambient-enrolled, so the mesh control plane and dataplane must be up first.

Because the component bundles the Backend CRD **and** an example Backend CR in
one directory, `scripts/apply` has a `pre_holos_authenticator` hook that applies
the CRD and waits for it to be `Established` before the full-directory apply (a
plain `kubectl apply -f dir/` would otherwise try the example CR before its CRD
exists), and a `wait_holos_authenticator` hook that waits for the manager
rollout.

## Render workflow

After editing any `.cue` file under `holos/components/holos-authenticator/` (or
the registries above), commit the CUE change, then run `scripts/render` from the
repo root and commit the regenerated `holos/deploy/` tree — the *CUE Component
Rendering* guardrail.

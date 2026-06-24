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
- two example **Backend** CRs — the discovery-based `example` (in-cluster
  Keycloak issuer) and the static-JWKS `remote-cluster-a` (KSA / offline mode,
  below).

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

## Static-JWKS / KSA backends (offline validation)

A `Backend` can validate tokens **offline** against a static JSON Web Key Set
instead of doing OIDC discovery. Set `spec.oidc.jwks` to the literal
`{"keys":[…]}` document and the authorizer:

- performs **no OIDC discovery and no JWKS HTTP fetch** — it verifies the token
  signature against the keys in `spec.oidc.jwks` directly;
- treats `spec.oidc.issuerURL` as the **expected `iss` claim value only** (it is
  not dialed), and ignores `spec.oidc.caBundle` (there is no issuer endpoint to
  trust);
- still enforces `iss` (== `issuerURL`), `aud` (== `clientID`), and `exp`/`nbf`,
  and runs the same CEL group mapping as the discovery path.

A malformed/unparseable JWKS, or one with zero usable keys, is rejected at
reconcile time as an **invalid spec** (`Accepted=False`/`Ready=False`, reason
`InvalidSpec`) — not a transient discovery failure. An empty `spec.oidc.jwks`
is unchanged: the authorizer performs OIDC discovery as before.

> Key selection / per-key algorithm enforcement currently matches the discovery
> path (the configured supported-algorithm set, no per-`kid` binding). Tightening
> both paths together (per-`kid` key selection, per-key alg enforcement) is the
> planned hardening tracked in HOL-1396.

This is the mechanism for a token issuer that is **unreachable** from this
cluster — most importantly a **remote cluster's Kubernetes API server signing
service-account (KSA) ID tokens**. The model is **1:1 host↔Backend**: each
remote cluster gets its own `Backend` with a unique `spec.host` (e.g.
`remote-cluster-a.holos.internal`), its own static JWKS, and its own
impersonator credential for the management cluster. The component renders
`remote-cluster-a` as the worked KSA example; its `jwks` is a **redacted
placeholder** an operator replaces with the remote cluster's real JWKS document
(`kubectl get --raw /openid/v1/jwks`). The JWKS is non-secret public-key
material and may live in the CR; the impersonator token in
`credentialsSecretRef` is still created at runtime and never committed.

The full operator procedure — capturing the remote JWKS/issuer, the SA-group
CEL expression, the SA-virtual-group impersonation RBAC, and end-to-end
External Secrets Operator (`SecretStore`/`ExternalSecret`) verification — is in
the runbook's [*KSA / static-JWKS
backends*](../../../docs/runbooks/holos-authenticator.md#ksa--static-jwks-backends)
section.

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

## Tenant isolation (Backend is a platform-owned object)

A `Backend` is **platform-owned**, never tenant-self-service:

- The manager's cache (and the Backend reconciler's watch) is **scoped to the
  `holos-authenticator` namespace** (`cmd/holos-authenticator/main.go`,
  `Cache.DefaultNamespaces`). A `Backend` created in a tenant namespace is never
  cached, reconciled, served by the ext_authz path, or used for controller-side
  OIDC discovery — closing both the controller-side SSRF (a tenant-chosen
  `issuerURL`) and the privileged-token-injection vectors at the wiring layer.
- The impersonator credential always resolves from the `holos-authenticator`
  namespace (`AuthorizerNamespace()` / `POD_NAMESPACE`), never the Backend's
  namespace, so a `Backend` cannot reference a Secret a tenant controls.
- The platform namespace registry adds `holos-authenticator` to
  `#ReservedNamespaceNames` (`holos/namespaces.cue`), so the `projects` Argo CD
  AppProject denies tenant Applications this namespace as a destination — a
  tenant cannot deploy a `Backend` (or a Secret) into it.

The example `AuthorizationPolicy` (`action: CUSTOM`, `provider.name:
holos-authenticator`) selects the authenticator's own pods. When retargeting it
at a protected workload behind a waypoint, keep the protected workload and its
`Backend`/policy in platform-owned namespaces; do not expose the provider to
tenant-controlled `AuthorizationPolicy` resources.

## Apply ordering (render-here / apply-out-of-band)

The component is **registered in `holos/platform/platform.cue`** (so it renders
to the deploy tree) but is **DELIBERATELY EXCLUDED from both the master
`scripts/apply` `COMPONENTS` floor and the system App-of-Apps**
(`holos/components/app-of-apps/buildplan.cue` `SYSTEM_COMPONENTS`).

The manager Deployment pulls its image from the in-cluster Quay registry
(`quay.holos.internal/holos/holos-authenticator:dev`), which **does not exist on
a freshly bootstrapped cluster** until an operator publishes it *after* the
imperative bootstrap floor (the manual Quay-org/image-push setup `scripts/apply`
stops before). A bootstrap rollout gate — or an Argo CD child Application — would
therefore hang in `ImagePullBackOff`. This is exactly why the `holos-controller`
(also image-from-Quay) is excluded from the bootstrap apply; the authenticator
follows the same precedent.

It is **applied out of band** once its image is published — the deploy step the
next phase wires, alongside the impersonator credential Secret and any waypoint
topology. The `ext_authz` provider (in `istiod`'s `MeshConfig`) and the
ambient-enrolled namespace are both established earlier in the Istio data-plane
phase, so nothing in the bootstrap floor depends on this component.

> **CRD-before-CR within the directory.** The component bundles the Backend CRD
> **and** an example Backend CR in one directory. The out-of-band apply step must
> apply `customresourcedefinition-*.yaml` first and wait for it to be
> `Established` before the example `backend-*.yaml` (a plain
> `kubectl apply -f dir/` applies files lexically, and `backend-*.yaml` sorts
> before `customresourcedefinition-*.yaml`).

## Render workflow

After editing any `.cue` file under `holos/components/holos-authenticator/` (or
the registries above), commit the CUE change, then run `scripts/render` from the
repo root and commit the regenerated `holos/deploy/` tree — the *CUE Component
Rendering* guardrail.

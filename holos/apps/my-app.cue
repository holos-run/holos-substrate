// my-app is the reference Application registration (ADR-21) — the one-line
// self-service entry the Application component (HOL-1356) renders into the full
// application-level resource set (the Deployment/Service/HTTPRoute, the app
// KeycloakClient defining the owner/editor/viewer roles the project role groups
// confer, the Quay Repository, and the Kargo Warehouse/Stage + Argo CD
// Application), all contained by its project my-project.  Registering it here
// (HOL-1357) is the second half of migrating the reference instance onto the
// templates: a project plus one app, each a single-file registration, replacing
// the bespoke holos/components/my-project component.
//
// The bespoke my-project scaffold had NO app (it was project-only), so the app's
// resources are a deliberate, documented SUPERSET of the pre-migration rendered
// tree (called out in the PR description) — they demonstrate the project↔app
// containment ADR-21 generalizes.
//
//   - project: my-project — the containment reference (a key in the `projects`
//     collection; a dangling reference fails at render).  The app's resources
//     land in my-project's bare control namespace, alongside the project's role
//     KeycloakGroups, so the role groups' clientRef to this app's KeycloakClient
//     resolves same-namespace (the Application component's namespace rationale).
//   - image: the upstream Kubernetes e2e agnhost image (multi-arch, the same one
//     the echo sample and the _AppImage default use) — its netexec server listens
//     on 8080 and answers the /healthz probe path the Application component's
//     Deployment configures.  Per-app images live on the registration (not the
//     single platform-wide _AppImage tag, which pins one image); a digest-pinned
//     reference is the publish-workflow posture, but a tag is accepted here for
//     the reference sample.
//   - port: 8080 — agnhost netexec's default listen port; the Service and
//     HTTPRoute target it.
//
// host is omitted, so the HTTPRoute defaults to my-app.holos.internal (the
// Application component's convention), which matches the *.holos.internal
// wildcard listener and resolves to 127.0.0.1 on the host (docs/local-cluster.md).
package apps

apps: "my-app": {
	project: "my-project"
	image:   "registry.k8s.io/e2e-test-images/agnhost:2.53"
	port:    8080
}

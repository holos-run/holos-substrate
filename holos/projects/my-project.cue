// my-project is the reference Project registration (ADR-21) — the one-line
// self-service entry the Project component (HOL-1355) will render into the full
// project-level resource set, and the template for a future self-service
// ProjectRequest.  Registering it here exercises the env-prefixed namespace
// derivation in holos/namespaces.cue (it derives ci-my-project, qa-my-project,
// and prod-my-project), while the hand-written static `my-project` registry
// entry is retained until the bespoke component migrates onto the templates
// (HOL-1357).
//
// owner bob@example.com matches the owner threaded through ADR-21's end-to-end
// worked example and the bespoke component's KeycloakUser (HOL-1348).
package projects

projects: "my-project": owners: "bob@example.com": _

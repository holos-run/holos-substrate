// my-project is the reference Project registration (ADR-21) — the one-line
// self-service entry the Project component (HOL-1355) renders into the full
// project-level resource set (the IAM owner/editor/viewer role + custodian
// Groups, the owner User, the project Client, the Quay
// Organization with its OIDC-synced teams, the Kargo control plane, the Argo CD
// AppProject/Application, and the owner-access RoleBinding), and the template
// for a future self-service ProjectRequest.
//
// As of HOL-1357 this single registration — plus the my-app application
// registration (holos/apps/my-app.cue) — is the ENTIRE source of the reference
// instance: the bespoke holos/components/my-project component was deleted and
// my-project is now produced wholly by the collection-driven Project +
// Application components.  Registering it here derives my-project's namespaces in
// holos/namespaces.cue (the bare my-project control namespace plus the
// env-prefixed ci-my-project, qa-my-project, prod-my-project) and authorizes its
// keycloak.holos.run CRs through the keycloak-instance ReferenceGrant.
//
// owner bob@example.com matches the owner threaded through ADR-21's end-to-end
// worked example.
package projects

projects: "my-project": owners: "bob@example.com": _

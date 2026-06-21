// Package apps is the well-known root CUE collection of platform Applications
// (ADR-21).  A product engineer stands up an Application with a single one-line
// registration:
//
//	// holos/apps/my-app.cue
//	package apps
//
//	apps: "my-app": {
//		project: "my-project"
//		image:   "quay.holos.internal/my-project/my-app@sha256:..."
//		port:    8080
//	}
//
// `project` names the Project the app belongs to (the GCP-model containment of
// ADR-21 *Unifying applications under their project*): it is a reference that
// must resolve to a key in the `projects` collection, so an app naming a
// non-existent project is a RENDER-time failure, not a runtime NotFound.  The
// Application component (HOL-1356) renders each apps.<name> entry into the full
// set of application-level resources; this package is only the data model +
// schema.
//
// This package IMPORTS the projects collection so apps.<name>.project unifies
// with projects.#RegisteredProject (the cross-collection reference is ordinary
// CUE unification).  Like the projects package, it is a SEPARATE importable
// package rather than `package holos` because a holos/ subdirectory's
// `package holos` files are not build-plan ancestors of a component instance;
// holos/collections.cue binds both collections into the root `holos` package
// scope.  See projects/projects.cue and collections.cue for the wiring rationale.
package apps

import proj "github.com/holos-run/holos-paas/holos/projects"

// #DNSLabel is the RFC 1123 DNS-label pattern (identical to the projects
// package's), validating each app name at render time.
#DNSLabel: =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"

// #App is the schema every apps.<name> entry unifies with.  Minimal but
// functional for the foundational phase: the containment field `project` plus
// the few fields the Application component (HOL-1356) needs to render a
// Deployment/Service/HTTPRoute.
#App: {
	// name is the app's name, set from the apps map key (apps: "<name>": …) and
	// validated against #DNSLabel — the registration does not author it.  The
	// Application component (HOL-1356) reads it as the resource name base.
	name: #DNSLabel

	// project names the Project (a key in the projects collection) this app is
	// contained by.  REQUIRED (the ! marker): an app omitting project fails at
	// render rather than rendering an unattached app.  Unified with
	// projects.#RegisteredProject so a reference to a non-existent project also
	// FAILS AT RENDER, mirroring the #RegisteredNamespace discipline
	// holos/namespaces.cue applies to namespace literals.
	project!: proj.#RegisteredProject

	// image is the container image reference the app's Deployment runs.  REQUIRED
	// (the ! marker): an app omitting image fails at render.  A digest-pinned
	// reference is preferred (the publish workflow's posture, see holos/tags.cue
	// _AppImage), but the schema only requires a non-empty string here — the
	// Application component validates the deployable shape.
	image!: string & !=""

	// port is the container port the app listens on; the Service/HTTPRoute the
	// Application component renders target it.  REQUIRED (the ! marker): an app
	// omitting port fails at render.  Constrained to the valid TCP port range so
	// a malformed port fails at render.
	port!: int & >0 & <=65535

	// host is the OPTIONAL external hostname the app is exposed at (the
	// HTTPRoute's hostname).  Omitted entries get no host field; when present it
	// must be a non-empty string.
	host?: string & !=""
}

// apps is the collection: an open map keyed by app name.  Each entry unifies
// with #App, and the key (NAME) is validated against #DNSLabel so a malformed
// app name fails at render.
//
// The pattern label [NAME=#DNSLabel] alone does NOT reject a non-matching key —
// CUE simply skips applying the constraint to a key the pattern does not match,
// rather than erroring.  So the key is ALSO captured into the value's `name`
// field and unified with #DNSLabel there, turning a malformed app name (e.g.
// "Bad_Name") into a render-time conflict on apps.<name>.name.  `name` is
// computed from the key, not authored in the one-line registration.
apps: [NAME=string]: #App & {
	name: NAME & #DNSLabel
}

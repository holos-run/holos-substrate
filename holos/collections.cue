package holos

import (
	proj "github.com/holos-run/holos-paas/holos/projects"
	appcoll "github.com/holos-run/holos-paas/holos/apps"
)

// collections.cue is the ANCESTOR/IMPORT WIRING for the two well-known CUE
// collections (ADR-21 *Unifying applications under their project*).
//
// The projects (holos/projects/) and apps (holos/apps/) collections live in
// holos/ SUBDIRECTORIES.  CUE loads only ROOT-level holos/*.cue `package holos`
// files as build-plan ancestors of a holos/components/<name>/ instance — a
// `package holos` file inside a subdirectory is NOT an ancestor (verified:
// `cue eval ./components/<x>` cannot resolve a field declared in
// holos/projects/*.cue under `package holos`).  So the collections cannot simply
// be `package holos` files in those subdirectories and be visible to namespaces.cue
// or to the Project/Application components.
//
// This file is the explicit wiring: it lives at the holos/ ROOT in `package holos`
// (an ancestor of every component, like namespaces.cue), imports the two
// collection packages, and BINDS them into the root `holos` package scope.  From
// here on:
//
//   - holos/namespaces.cue derives the env-prefixed namespace registry entries
//     from `projects` (this file makes `projects` visible at the root).
//   - The Project component (HOL-1355) reads `projects`; the Application
//     component (HOL-1356) reads `apps` — both as ordinary ancestor fields, with
//     no per-component import.
//   - The cross-collection reference apps.<name>.project → a projects key is
//     resolved as ordinary CUE unification (the apps package imports projects and
//     constrains project to projects.#RegisteredProject).
//
// Phases 2-3 follow THIS mechanism: reference the root `projects`/`apps` fields
// and the #Project/#App/#RegisteredProject definitions re-exported below; do not
// re-import the collection packages per component.

// projects is the platform Projects collection, bound from the projects package
// into the root `holos` scope.  Its #Project schema and #DNSLabel name
// validation live in projects/projects.cue.
projects: proj.projects

// #RegisteredProject is the disjunction of every registered project name
// (mirroring #RegisteredNamespace), re-exported here so root-level files and the
// components can constrain a project literal without importing the projects
// package.
#RegisteredProject: proj.#RegisteredProject

// #Project is the per-entry project schema, re-exported for the components.
#Project: proj.#Project

// apps is the platform Applications collection, bound from the apps package into
// the root `holos` scope.  Its #App schema lives in apps/apps.cue; apps.<name>.project
// is already unified with projects.#RegisteredProject inside that package, so the
// cross-collection validation holds wherever `apps` is read.
apps: appcoll.apps

// #App is the per-entry application schema, re-exported for the components.
#App: appcoll.#App

// --- Render-path validation -------------------------------------------------
//
// The collections' schema constraints (DNS-label names, the email-shaped owner
// keys, the at-least-one-owner minimum, and the apps.<name>.project →
// #RegisteredProject cross-reference) only fail at render if SOMETHING ON THE
// RENDER PATH actually evaluates the constrained fields.  Until the
// Project/Application components (HOL-1355/HOL-1356) consume the collections,
// nothing does: `holos render platform` would happily ignore a malformed
// holos/apps/*.cue or an ownerless project, and the validation would fire only
// under an explicit `cue vet ./apps`/`./projects`.
//
// collections.cue is a build-plan ANCESTOR of every component (a root-level
// `package holos` file), so the validation fields below are evaluated on EVERY
// component render — putting the collection contract on the scripts/render path
// now, before the consuming components exist.  Each field is hidden (underscore)
// so it never escapes into a manifest; CUE still evaluates a referenced hidden
// field, and these are referenced by being concrete struct fields of the
// ancestor.

// _validateProjects forces every project's owner constraints onto the render
// path: len(owners) > 0 (no ownerless project) per concrete entry.  The
// per-entry name/owner-key/email constraints ride the bound `projects` data
// itself; this comprehension adds the cross-cutting minimum the projects
// package could only express as a package-private field (#Project cannot carry
// it without making the abstract schema unsatisfiable — see projects.cue).
_validateProjects: {
	for NAME, P in projects {
		(NAME): len(P.owners) & >0
	}
}

// _validateApps forces every app's fields — including the apps.<name>.project →
// #RegisteredProject cross-reference — onto the render path by referencing them.
// Reading project/image/port/name makes a malformed or dangling app entry a
// render failure even with no Application component yet.  (host is optional, so
// it is not forced.)
_validateApps: {
	for NAME, A in apps {
		(NAME): {
			project: A.project
			image:   A.image
			port:    A.port
			name:    A.name
		}
	}
}

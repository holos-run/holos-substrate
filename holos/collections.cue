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
// #RegisteredProject cross-reference) only fail at render if SOMETHING THE
// RENDERED OUTPUT DEPENDS ON evaluates the constrained fields.  `holos render
// platform` evaluates the `holos:` BuildPlan of each component, NOT arbitrary
// hidden ancestor fields — a hidden field that no rendered output references is
// never forced.  So until the Project/Application components (HOL-1355/HOL-1356)
// consume the collections, the contract must be tied to an output something
// already renders.  The always-present `namespaces` component is that anchor:
// holos/namespaces.cue derives the project namespaces and the namespaces
// buildplan UNIFIES the rendered Namespace resources with #CollectionsValidated
// below, so evaluating any namespace manifest forces the whole validation.
//
// #CollectionsValidated exposes two things the always-rendered namespaces
// component folds into an EXPORTED manifest (a ConfigMap), which is what puts the
// whole collection contract on the render path:
//
//   - `ownersOk`: a per-project bool that is `true` only when the project names
//     at least one owner; an ownerless project makes it `false & true` (= _|_),
//     failing render.
//   - `tokens`: a per-app string folding each app's required fields via
//     INTERPOLATION.  Interpolation forces CONCRETENESS on export, so when the
//     namespaces component exports these strings, a MISSING required field
//     (project!/image!/port! omitted) fails with `required field missing`, a
//     dangling project fails the #RegisteredProject cross-reference (`conflicting
//     values`), and a malformed name / empty image / bad port fails the value
//     constraint (`out of bound`).  This is the lever that makes even a missing
//     required field a render error — a hidden field merely READING the value
//     left it incomplete (tolerated); an EXPORTED interpolation does not.
//
// Both maps are keyed by name (project/app) so distinct entries never unify into
// a cross-entry conflict (the multi-app-collision trap).  The namespaces buildplan
// is the anchor because it already derives the project namespaces from the same
// `projects` collection; see components/namespaces/buildplan.cue.
#CollectionsValidated: {
	// ownersOk: per-project, true iff the project has >0 owners.  The namespaces
	// component exports these into the validation ConfigMap, forcing the check.
	ownersOk: {
		for NAME, P in projects {
			(NAME): true & (len(P.owners) > 0)
		}
	}

	// tokens: per-app interpolation of name|project|image|port.  Exporting these
	// forces every required app field to be present and concrete and the
	// project cross-reference to resolve.  (host is optional, so it is omitted.)
	tokens: {
		for NAME, A in apps {
			(NAME): "\(A.name)|\(A.project)|\(A.image)|\(A.port)"
		}
	}
}

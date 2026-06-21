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
// RENDER PATH evaluates the constrained fields.  `holos render platform`
// evaluates each component's value, including a referenced hidden field — and a
// hidden field that evaluates to _|_ (bottom) DOES surface as a render error.
// So until the Project/Application components (HOL-1355/HOL-1356) consume the
// collections, the contract is anchored to the always-rendered `namespaces`
// component, which references #CollectionsValidated from a hidden field
// (_collectionsValidated — see components/namespaces/buildplan.cue).  That
// component is the natural anchor: it already derives the project namespaces
// from the same `projects` collection.  The reference is a HIDDEN field, not a
// rendered resource — the namespaces component is the bootstrap, apply-first
// component and must emit ONLY Namespace manifests (a namespaced validation
// resource would sort/apply before its own namespace and break a fresh apply),
// so the validation rides evaluation, not an emitted object.
//
// #CollectionsValidated is well-defined ONLY when every project and app entry
// satisfies its constraints; any violation that produces _|_ propagates the
// failure to the namespaces component's _collectionsValidated reference and
// fails the render.  The cases that produce _|_ — and therefore fail at render
// now — are: an ownerless project, a dangling apps.<name>.project, and a
// malformed app/project name or empty image or out-of-range port (the present-
// value constraints).  The one case that does NOT (a required app field omitted
// ENTIRELY leaves the value incomplete, not _|_, which a hidden field tolerates)
// is enforced when the Application component (HOL-1356) consumes/exports the
// field — the ! markers on #App declare the contract; see the _apps note below.
#CollectionsValidated: {
	// Every project must name at least one owner (len(owners) > 0).  The
	// per-entry name/owner-key/email constraints ride the bound `projects` data
	// itself; this adds the cross-cutting minimum the projects package could not
	// express on #Project without making the abstract schema unsatisfiable (see
	// projects.cue).  Unified into the empty struct via a guard: the for-body
	// asserts but contributes no fields.
	for _, P in projects {
		if len(P.owners) < 1 {
			// A project with no owners forces this branch, unifying the marker
			// with an impossible constraint and failing the render.
			_ownerless: false & true
		}
	}

	// Per-app validation, keyed by app NAME.  Keying by name (not a single shared
	// field) is essential: a shared field would unify across apps and make two
	// apps with different images/ports conflict — breaking multi-app support.
	//
	// What this forces on the render path NOW (a violation is a render error
	// because it produces _|_, which holos surfaces even from a hidden field):
	//   - the apps.<name>.project → #RegisteredProject CROSS-REFERENCE (a dangling
	//     project is `conflicting values`),
	//   - the name/image/port VALUE constraints when the field is present (a
	//     malformed app name or an empty image is `out of bound`).
	//
	// What it does NOT force here, and why: a MISSING required field
	// (project!/image!/port! omitted entirely) leaves the app value INCOMPLETE,
	// not _|_, and holos render tolerates an incomplete HIDDEN field that no
	// rendered manifest exports.  Concreteness of a required field is therefore
	// enforced when an app is actually CONSUMED — i.e. when the Application
	// component (HOL-1356) renders the app's Deployment/Service from these fields,
	// the export forces project/image/port to be concrete.  The ! markers on #App
	// declare the contract; the consuming component is where a render exports it.
	// (host is optional, so it is not read.)  This is an honest boundary of the
	// foundation phase: the cross-reference and present-value validation the AC
	// mandates hold now; required-field presence rides the consumer.
	_apps: {
		for NAME, A in apps {
			(NAME): {
				_p: A.project
				_i: A.image
				_n: A.port
				_m: A.name
			}
		}
	}
}

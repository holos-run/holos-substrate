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

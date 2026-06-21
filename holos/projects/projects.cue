// Package projects is the well-known root CUE collection of platform Projects
// (ADR-21).  A product engineer stands up a Project with a single one-line
// registration:
//
//	// holos/projects/my-project.cue
//	package projects
//
//	projects: "my-project": owners: "bob@example.com": _
//
// `owners` is a map keyed by the owner's email, so a Project may name one or
// several owners (projects.<name>.owners.<email>).  The Project component
// (HOL-1355) renders each projects.<name> entry into the full set of
// project-level resources; this package is only the data model + schema.
//
// Why a SEPARATE package (not `package holos`): the two collections live in
// holos/ SUBDIRECTORIES, and a subdirectory's `package holos` files are NOT
// loaded as build-plan ancestors of a holos/components/<name>/ instance — only
// root-level holos/*.cue files are (verified: cue eval ./components/<x> cannot
// see a holos/projects/*.cue field declared in `package holos`).  Making this an
// importable package lets the root-level holos/collections.cue bind it into the
// `holos` package scope (where namespaces.cue and every component ancestor can
// reach it) via an ordinary import.  That is the explicit ancestor/import wiring
// ADR-21 *Unifying applications under their project* calls for; collections.cue
// documents the binding.
package projects

// #DNSLabel is the RFC 1123 DNS-label pattern the platform validates names
// against everywhere (the same regex holos/namespaces.cue enforces on Namespace
// names).  A project name flows into the derived <env>-<name> namespace names
// (holos/namespaces.cue), which must themselves be valid DNS labels — so the
// "ci-"/"qa-"/"prod-" prefix (3-5 chars incl. the hyphen) bounds the project
// name length implicitly: a 63-char project name would overflow the 63-char
// label limit once prefixed and fail render there.
#DNSLabel: =~"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"

// #Email is the shape an owner-map key must take — a deliberately simple
// "<local>@<domain>.<tld>" pattern (a non-empty local part, an "@", a dotted
// domain).  It is not a full RFC 5322 grammar (CUE regexps cannot express one,
// and the platform does not need it); it exists to reject the obvious mistakes
// the owners map would otherwise admit — an empty key "" or a bare token like
// "notanemail" — at RENDER time, before that malformed identity flows into the
// owner's KeycloakUser (ADR-20) or an access grant.
#Email: =~"^[^@[:space:]]+@[^@[:space:]]+\\.[^@[:space:]]+$"

// #Owner is a single project owner.  Keyed by #Email in the owners map; the
// value is open (the one-line registration writes `owners: "<email>": _`) so a
// later phase may attach owner attributes without breaking existing entries.
// `email` is set from the map key (not authored in the registration) and
// validated against #Email by the owners constraint.
#Owner: {
	email: #Email
	...
}

// #Project is the schema every projects.<name> entry unifies with.  It is
// intentionally minimal for this foundational phase: the one required field is
// the owners map.  name is bound to the map key by the projects constraint
// below and validated against #DNSLabel there.
#Project: {
	// name is the project's name, set from the projects map key
	// (projects: "<name>": …) and validated against #DNSLabel — the registration
	// does not author it.  The Project component (HOL-1355) reads it as the
	// resource name base, and holos/namespaces.cue derives the <env>-<name>
	// namespace names from it.
	name: #DNSLabel

	// owners maps an owner's email address to that owner's (open) record.  The
	// key must be #Email-shaped; an empty or malformed key fails at render.  As
	// with the name fields, the pattern label [#Email] alone does not reject a
	// non-matching key (CUE skips the constraint rather than erroring), so each
	// key is captured into the value's `email` field and unified with #Email
	// there — turning a bad owner key (e.g. "" or "notanemail") into a
	// render-time conflict on owners.<key>.email.
	//
	// The "at least one owner" minimum is NOT enforced here on #Project: a
	// len(owners) & >0 constraint inside the definition makes the abstract
	// schema itself unsatisfiable (its default owners is the empty struct), so
	// the minimum is enforced on the CONCRETE collection entries instead — see
	// the projects map constraint below.
	owners: [EMAIL=string]: #Owner & {
		email: EMAIL & #Email
	}
}

// projects is the collection: an open map keyed by project name.  Each entry
// unifies with #Project, and the key (NAME) is captured into name and validated
// against #DNSLabel so a malformed project name fails at RENDER time, before it
// can produce an invalid derived namespace name or escape into a manifest.  The
// pattern label alone does not reject a non-matching key (CUE skips the
// constraint), so the key is unified into name to force the failure.
projects: [NAME=string]: #Project & {
	name: NAME & #DNSLabel
}

// A registered project MUST name at least one owner — an ownerless project is
// rejected at render, not silently admitted (it would have no KeycloakUser to
// pre-provision and no one to bind project access to).  Enforced as a separate
// validation comprehension over the CONCRETE entries (not as a constraint on
// #Project or on the projects pattern label, either of which evaluates against
// the empty default owners struct and is unsatisfiable / cannot resolve the
// field): for each entry, len(owners) is unified with >0, so a project with an
// empty owners map fails here at render.
_projectsHaveOwners: {
	for NAME, P in projects {
		(NAME): len(P.owners) & >0
	}
}

// #RegisteredProject is the disjunction of every registered project name —
// mirroring holos/namespaces.cue's #RegisteredNamespace.  The apps collection
// unifies apps.<name>.project with this definition so an app naming a
// non-existent project is a RENDER-time failure, not a runtime NotFound.  It is
// re-exported through the root `holos` package by holos/collections.cue.
#RegisteredProject: or([for NAME, _ in projects {NAME}])

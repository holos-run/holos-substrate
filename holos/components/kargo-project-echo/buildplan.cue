package holos

import (
	kargoproject "kargo.akuity.io/project/v1alpha1"
)

// kargo-project-echo defines the Kargo Project for the echo sample app's
// delivery pipeline (HOL-1240).  A Kargo Project reconciles to a same-named
// Kubernetes namespace and is the boundary that owns the Warehouse, Stage,
// Freight, and Promotion resources for one application's pipeline.
//
// This mirrors the reference platform's minimal Project component
// (holos-reference/holos/components/kargo-project-braintrust): the Project is a
// single resource carrying only its name and an (empty here) promotion policy.
// The companion Warehouse, Stage, and Argo CD Application target live in the
// sibling kargo-echo component so this component stays a pure Project, matching
// the reference's one-Project-per-component shape.
//
// Sample app: echo (components/echo), the permanent Layer 3 smoke-test
// workload the client-side publish workflow targets
// (holos/docs/oci-publish-workflow.md).  Its two repositories are:
//   - app image:           quay.holos.localhost/holos/echo            (the
//     container the echo Deployment runs, injected via the _AppImage tag)
//   - rendered manifests:  quay.holos.localhost/holos/holos-paas-manifests
//     (the OCI artifact scripts/publish pushes; the Warehouse watches this)
//
// PROJECT is the Kargo Project namespace.  It is DELIBERATELY a dedicated
// namespace (kargo-echo), not the echo workload namespace: a Project adopts a
// same-named namespace and adds its own kargo.akuity.io/finalizer, so reusing
// the echo namespace (server-side-applied by the namespaces component) would
// risk finalizer/label contention between the two reconcilers.  The namespace
// is registered centrally (holos/namespaces.cue) carrying the
// kargo.akuity.io/project: "true" adoption label and the
// kargo.akuity.io/keep-namespace annotation so Kargo adopts the pre-created
// namespace rather than refusing it and never deletes it.  Unifying the literal
// with #RegisteredNamespace turns drift between this name and the registry
// entry into a render failure.
let PROJECT = "kargo-echo" & #RegisteredNamespace

// The auto-promotion policy lets new Freight flow into the Stage as soon as the
// Warehouse discovers a newly published rendered-manifests artifact, without a
// manual promotion — the spike's "publish → Freight → promotion → Argo CD sync"
// loop must close automatically (the issue's verification AC).  Stages that
// subscribe to a Warehouse (rather than an upstream Stage) are the documented
// case for autoPromotionEnabled: true.  The stage name MUST match the Stage in
// the kargo-echo component.
let STAGE = "test"

let PROJECT_RESOURCE = kargoproject.#Project & {
	metadata: name: PROJECT
	spec: promotionPolicies: [{
		stage:                STAGE
		autoPromotionEnabled: true
	}]
}

userDefinedBuildPlan: {
	metadata: name: "kargo-project-echo"
	spec: artifacts: manifests: {
		// One resource per artifact, CUE-natively (component guidelines): the
		// Resources generator's output IS the artifact file, so the file holds
		// exactly one Project resource with no bundle to slice.
		"clusters/\(clusterName)/components/\(metadata.name)/project-\(PROJECT).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: Project: (PROJECT): PROJECT_RESOURCE
			}]
		}
	}
}

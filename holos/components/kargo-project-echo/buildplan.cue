package holos

// kargo-project-echo defines the Kargo Project for the echo sample app's
// delivery pipeline (HOL-1240).  A Kargo Project reconciles to a same-named
// Kubernetes namespace and is the boundary that owns the Warehouse, Stage,
// Freight, and Promotion resources for one application's pipeline.
//
// This mirrors the reference platform's minimal Project component
// (holos-reference/holos/components/kargo-project-braintrust): the Project is a
// single resource carrying only its name.  The companion Warehouse, Stage, and
// Argo CD Application target live in the sibling kargo-echo component so this
// component stays Project-scoped, matching the reference's
// one-Project-per-component shape.
//
// Authored as plain CUE structs rather than via the vendored kargo bindings:
// the vendored #Project binding
// (holos/cue.mod/gen/kargo.akuity.io/project/v1alpha1) is stale — it carries a
// required spec! with promotionPolicies from an OLDER Kargo, but the Kargo
// 1.10.3 Project CRD this platform installs (components/kargo-crds, the same
// chart version) is CLUSTER-SCOPED and has NO spec at all (only metadata and
// status).  In Kargo 1.10 the promotion policy moved off Project onto the
// namespaced ProjectConfig CRD (see PROJECT_CONFIG below).  Using the stale
// binding would force a spec the server prunes or rejects.  The CRD shapes are
// the source of truth here:
//   - components/kargo/vendor/1.10.3/kargo/resources/crds/kargo.akuity.io_projects.yaml
//   - components/kargo/vendor/1.10.3/kargo/resources/crds/kargo.akuity.io_projectconfigs.yaml
//
// Sample app: echo (components/echo), the permanent Layer 3 smoke-test
// workload the client-side publish workflow targets
// (holos/docs/oci-publish-workflow.md).  Its two repositories are:
//   - app image:           quay.holos.internal/holos/echo            (the
//     container the echo Deployment runs, injected via the _AppImage tag)
//   - rendered manifests:  quay.holos.internal/holos/holos-paas-manifests
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

// The Kargo 1.10 Project is cluster-scoped with no spec: it only marks the
// kargo-echo namespace as a Project (the controller reconciles it to the
// adopted namespace).  No metadata.namespace — the resource is cluster-scoped
// and the Project NAME is what the controller maps to the same-named namespace.
let PROJECT_RESOURCE = {
	apiVersion: "kargo.akuity.io/v1alpha1"
	kind:       "Project"
	metadata: name: PROJECT
}

// The promotion policy lives on a namespaced ProjectConfig in the Project's
// namespace (Kargo 1.10).  Stage.status.autoPromotionEnabled is derived from
// this, so the test Stage auto-promotes newly created Freight.  The
// ProjectConfig is conventionally named after its Project's namespace.
let PROJECT_CONFIG = {
	apiVersion: "kargo.akuity.io/v1alpha1"
	kind:       "ProjectConfig"
	metadata: {
		name:      PROJECT
		namespace: PROJECT
	}
	spec: promotionPolicies: [{
		stage:                STAGE
		autoPromotionEnabled: true
	}]
}

userDefinedBuildPlan: {
	metadata: name: "kargo-project-echo"
	spec: artifacts: manifests: {
		// One resource per artifact, CUE-natively (component guidelines): each
		// Resources generator's output IS its artifact file, so each file holds
		// exactly one resource with no bundle to slice.
		"clusters/\(clusterName)/components/\(metadata.name)/project-\(PROJECT).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: Project: (PROJECT): PROJECT_RESOURCE
			}]
		}
		"clusters/\(clusterName)/components/\(metadata.name)/projectconfig-\(PROJECT).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				resources: ProjectConfig: (PROJECT): PROJECT_CONFIG
			}]
		}
	}
}

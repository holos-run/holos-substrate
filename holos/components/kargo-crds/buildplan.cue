package holos

// kargo-crds renders ONLY the Kargo CustomResourceDefinitions
// (warehouses, stages, freights, promotions, promotiontasks,
// clusterpromotiontasks, projects, projectconfigs, clusterconfigs in group
// kargo.akuity.io) from the upstream Kargo Helm chart, filtered to
// CustomResourceDefinition.  CRDs are isolated from the controller per the
// component guidelines (holos/docs/component-guidelines.md) and labeled
// crds: "true" at the registration site (platform/platform.cue) so they
// apply before the kargo controller that depends on them.
//
// This mirrors the reference platform's kargo-crds component
// (holos-reference/holos/components/kargo-crds): a Helm render of the same
// chart with crds.install: true, sliced to CustomResourceDefinition only with
// `holos kubectl-slice --include-kind=CustomResourceDefinition`.  The chart
// itself owns the CRD content (vendor/<version>/kargo/resources/crds), so this
// component never hand-authors a CRD bundle.
//
// The version pin MUST match KargoChartVersion in
// components/kargo/buildplan.cue: the CRDs applied ahead of the controller
// must be exactly the versions the chart's workloads expect.  They are two
// sibling components (the issue's flat layout), so there is no shared CUE
// ancestor to hoist the pin into; a bump touches both files plus both vendored
// charts and both regenerated deploy trees, and a mismatch is visible in the
// diff.

// KargoChartVersion pins the upstream Kargo Helm chart
// (oci://ghcr.io/akuity/kargo-charts/kargo).  Chart 1.10.3 installs Kargo app
// version v1.10.3 (the chart's appVersion — chart and app versions track
// together in this chart), matching the reference platform's pin
// (holos-reference/holos/config/kargo/version/*.yaml).  The vendored chart at
// vendor/1.10.3/kargo carries the CRDs this component slices.  Before bumping,
// re-check the chart's appVersion and keep this in sync with
// components/kargo/buildplan.cue.
let KargoChartVersion = "1.10.3"

// KargoRepository is the upstream Kargo Helm chart OCI repository.  The chart
// name is the full OCI reference (the holos Helm generator treats an
// oci:// chart name as a direct pull), so no separate repository field is set.
let KargoChartName = "oci://ghcr.io/akuity/kargo-charts/kargo"

// The CRDs install cluster-scoped, so the Helm render namespace is cosmetic
// for this component (CRDs carry no namespace); reuse the kargo namespace the
// controller component uses so the Helm release metadata is consistent.  The
// #RegisteredNamespace constraint (holos/namespaces.cue) turns silent drift
// between this literal and the registry entry into a render failure.
let NAMESPACE = "kargo" & #RegisteredNamespace

userDefinedBuildPlan: {
	metadata: name: "kargo-crds"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Helm"
				output: "helm-output.yaml"
				helm: {
					namespace: NAMESPACE
					chart: {
						name:    KargoChartName
						version: KargoChartVersion
						release: metadata.name
					}
					values: {
						// Render the CRDs (this component owns them); the
						// kargo controller component sets crds.install: false.
						crds: install: true
						// The chart `fail`s at template time when the admin
						// account is enabled (its default) without a bcrypt
						// passwordHash and token signing key.  This component
						// slices the output down to CustomResourceDefinition
						// only, so the API workloads it would emit are discarded
						// anyway — disable the admin account purely so the chart
						// templates successfully.  Keep this in sync with the
						// kargo controller component's no-auth posture
						// (components/kargo/buildplan.cue).
						api: adminAccount: enabled: false
						// Helm derives version-gated template output from the
						// helm binary's compiled-in default Kubernetes version
						// unless overridden; pin it to the local cluster's k3s
						// version — v1.31.5, the k3d v5.8.3 default image, per
						// the CertManagerVersion pin comment in
						// components/cert-manager/cert-manager.cue — so
						// rendering is deterministic across helm versions on
						// contributor machines and CI.
						kubeVersionOverride: "1.31.5"
					}
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: resources: inputs
				},
				{
					kind: "Command"
					inputs: [transformers[0].output]
					// this output is the artifact holos writes to the deploy
					// directory, one file per CustomResourceDefinition.  The
					// --include-kind filter discards everything the chart
					// renders that is not a CRD (the reference platform's
					// kargo-crds pattern).
					output: artifact
					command: args: [
						"holos",
						"kubectl-slice",
						"--include-kind=CustomResourceDefinition",
						"-f", "\(BuildContext.tempDir)/\(inputs[0])",
						"-o", "\(BuildContext.tempDir)/\(artifact)",
					]
				},
			]
		}
	}
}

package holos

import (
	core "github.com/holos-run/holos/api/core/v1alpha6:core"
	application "argoproj.io/application/v1alpha1"
)

// userDefinedBuildPlan represents the user-defined interface platform
// engineers use to integrate software into the platform with the holos cli.
//
// The holos cli processes the holos: core.#BuildPlan value, therefore we use
// the userDefinedBuildPlan field to define our own schema and an adapter at
// the bottom of this file to transform it into the holos field for use with
// holos render platform.
//
// This file lives in the components/ directory so CUE includes it in every
// component instance loaded from components/<name> without imports.
userDefinedBuildPlan: {
	metadata: core.#Metadata & {
		name: _Tags.component.name
		labels: "app.holos.run/component.name": name
	}

	spec: {
		// artifacts represents two distinct sets of kubernetes resources:
		// manifest files, and gitops files.  Conceptually gitops resources
		// reconcile manifest files with the cluster, therefore they are
		// separated for effective operational management.
		artifacts: {
			// manifests represents the set of kubernetes manifest files each
			// containing a bundle of kubernetes resources produced by the
			// component.
			manifests: [ARTIFACT=core.#FileOrDirectoryPath]: core.#Artifact & {
				artifact: ARTIFACT
			}
			// gitops represents the gitops resources associated with the
			// component (e.g. an ArgoCD Application), projected into all
			// components in a consistent way.
			gitops: [ARTIFACT=core.#FileOrDirectoryPath]: core.#Artifact & {
				artifact: ARTIFACT
			}
		}

		// disabled causes the holos render platform command to skip the
		// BuildPlan.
		disabled?: bool

		// argoAppDisabled causes the gitops Application artifact to be
		// omitted.  Defaults to true as a placeholder until ArgoCD is
		// deployed to the platform.
		argoAppDisabled: bool | *true

		// argoApp represents the ArgoCD Application associated with this
		// build plan.
		argoApp: application.#Application
	}

	// Project an ArgoCD Application through the gitops field when enabled.
	if spec.argoAppDisabled == false {
		spec: artifacts: gitops: {
			"clusters/\(clusterName)/gitops/application-\(metadata.name).yaml": {
				artifact: _
				generators: [{
					kind:   "Resources"
					output: artifact
					resources: Application: (metadata.name): spec.argoApp
				}]
			}
		}
	}

	// Components may override the configuration of the Application using
	// this struct.
	spec: argoApp: {
		metadata: {
			name: userDefinedBuildPlan.metadata.name
			// Placeholder namespace until ArgoCD is deployed to the platform.
			namespace: string | *"argocd"
		}
		spec: {
			destination: server: string | *"https://kubernetes.default.svc"
			project: string | *"default"
			source: {
				path:           string | *"holos/deploy/clusters/\(clusterName)/components/\(metadata.name)"
				repoURL:        string | *"https://github.com/holos-run/holos-paas"
				targetRevision: string | *"main"
			}
		}
	}
}

// Adapt the user-defined BuildPlan into a core v1alpha6 BuildPlan for holos
// to execute.
holos: core.#BuildPlan & {
	metadata: userDefinedBuildPlan.metadata
	spec: {
		// Convert the artifacts struct to a list for v1alpha6.
		artifacts: [
			// manifest files (e.g. resources.gen.yaml)
			for M in userDefinedBuildPlan.spec.artifacts.manifests {M},
			// gitops files (e.g. argocd Application referring to the manifest files)
			for G in userDefinedBuildPlan.spec.artifacts.gitops {G},
		]
		if userDefinedBuildPlan.spec.disabled != _|_ {
			disabled: userDefinedBuildPlan.spec.disabled
		}
	}
}

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
//
// IMPORTANT: because this file unconditionally defines holos: as a BuildPlan
// whose spec.artifacts list is computed from userDefinedBuildPlan, every
// component under components/ must integrate through userDefinedBuildPlan.
// The author-style wrappers in schema.cue (#Kubernetes, #Kustomize, #Helm)
// produce their own BuildPlan and conflict with this adapter; they remain
// usable only for components registered under a non-default
// #ComponentTemplate inputs.prefix where this file is not an ancestor.
userDefinedBuildPlan: {
	metadata: core.#Metadata & {
		name: _Tags.component.name
		labels: "app.holos.run/component.name": name

		// labels is an optional field, guard references to it.
		if _Tags.component.labels != _|_ {
			labels: _Tags.component.labels
		}

		// annotations is an optional field, guard references to it.
		if _Tags.component.annotations != _|_ {
			annotations: _Tags.component.annotations
		}
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
		// omitted.  Defaults to true: ArgoCD is installed on the platform,
		// but gitops delivery of platform components has not yet replaced
		// the direct apply — see docs/placeholders.md (ArgoCD gitops
		// delivery) for the deferred flip to false.
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
			// ArgoCD runs in the argocd namespace — keep in sync with
			// ArgoCDNamespace in components/argocd/argocd.cue.
			namespace: string | *"argocd"
		}
		spec: {
			destination: server: string | *"https://kubernetes.default.svc"
			project: string | *"default"
			source: {
				path:           string | *"holos/deploy/clusters/\(clusterName)/components/\(userDefinedBuildPlan.metadata.name)"
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

package holos

import "github.com/holos-run/holos/api/author/v1alpha6:author"

// IMPORTANT: components under components/ must integrate through the
// userDefinedBuildPlan adapter defined in components/user-defined-build-plan.cue,
// which unconditionally defines holos: as a BuildPlan.  The author-style
// wrappers below (#Kubernetes, #Kustomize, #Helm) produce their own BuildPlan
// and conflict with that adapter; they remain usable only for components
// registered under a non-default #ComponentTemplate inputs.prefix where the
// adapter file is not an ancestor.

#ComponentConfig: author.#ComponentConfig & {
	Name:      _Tags.component.name
	Path:      _Tags.component.path
	Resources: #Resources

	// labels is an optional field, guard references to it.
	if _Tags.component.labels != _|_ {
		Labels: _Tags.component.labels
	}

	// annotations is an optional field, guard references to it.
	if _Tags.component.annotations != _|_ {
		Annotations: _Tags.component.annotations
	}
}

// https://holos.run/docs/api/author/v1alpha6/#Kubernetes
#Kubernetes: close({
	#ComponentConfig
	author.#Kubernetes
})

// https://holos.run/docs/api/author/v1alpha6/#Kustomize
#Kustomize: close({
	#ComponentConfig
	author.#Kustomize
})

// https://holos.run/docs/api/author/v1alpha6/#Helm
#Helm: close({
	#ComponentConfig
	author.#Helm
})

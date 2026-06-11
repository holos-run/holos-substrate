package holos

// clusters represents the clusters the platform manages, keyed by name.  Each
// registered cluster gets every component registered in the platform:
// components: struct below, parameterized by the clusterName tag.
clusters: [NAME=string]: name: NAME

// k3d-holos is the local development cluster.  See docs/local-cluster.md and
// the k3d/ directory for how the cluster is created.
clusters: "k3d-holos": _

// Register production clusters here when a production deployment area is
// established.  For example:
//
//  clusters: "prod-us-east-1": _

// All components managed on all clusters get merged into one big platform
// structure.  Use holos show platform to inspect the structure holos render
// platform iterates over, rendering each component concurrently.
//
// See https://holos.run/docs/api/author/v1alpha6/#Platform
platform: {
	name: "holos-paas"

	for CLUSTER in clusters {
		components: {
			// Register components for every cluster here.  For example:
			//
			//  (#ComponentTemplate & {inputs: {
			//  	component: "gateway-api"
			//  	cluster:   CLUSTER.name
			//  }}).output
		}
	}
}

// #ComponentTemplate registers one component for one cluster.  The output
// field unifies into the platform: components: struct keyed so the same
// component may be registered for multiple clusters without collisions.
//
// holos render platform injects each entry's name and path as the
// holos_component_name and holos_component_path tags, parameters as additional
// tags (clusterName), and copies labels and annotations to the BuildPlan.
#ComponentTemplate: {
	inputs: {
		// component represents the directory name of the component under prefix.
		component: string
		// name represents the BuildPlan metadata.name, defaults to component.
		name: string | *component
		// cluster represents the name of the cluster the component renders for.
		cluster: string
		// prefix represents the directory containing the component directory.
		prefix: string | *"components"
		// parameters are injected into the component as CUE @tag variables.
		parameters: {}
		labels: {[string]: string}
		annotations: {[string]: string}
	}
	key: "cluster:\(inputs.cluster):component:\(inputs.name)"
	output: (key): {
		name: inputs.name
		path: "\(inputs.prefix)/\(inputs.component)"
		parameters: inputs.parameters & {
			clusterName: inputs.cluster
		}
		// labels are useful for inspecting BuildPlans and rendering a subset of
		// the platform.  For example:
		//  holos show buildplans --selector cluster==k3d-holos
		//  holos render platform --selector cluster==k3d-holos
		labels: {
			inputs.labels
			"path":    path
			cluster:   inputs.cluster
			component: inputs.name
		}
		annotations: {
			inputs.annotations
			"app.holos.run/description": "\(inputs.name) for \(inputs.cluster)"
		}
	}
}

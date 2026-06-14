package holos

import (
	"encoding/json"

	"github.com/holos-run/holos/api/core/v1alpha6:core"
)

// _AppImage is the container image reference for the deployable sample
// application (the echo component, the Layer 3 workload the client-side
// build-and-publish workflow targets — see holos/docs/oci-publish-workflow.md).
//
// The default preserves the agnhost smoke-test image so an unparameterized
// `holos render platform` stays diff-clean.  The publish workflow overrides it
// with a digest-pinned reference:
//
//	holos render platform --inject app_image=registry/app@sha256:<digest>
//
// Injecting the immutable digest (not a mutable tag) makes the rendered output
// exact and reproducible for the same input (research §4.4).
_AppImage: string | *"registry.k8s.io/e2e-test-images/agnhost:2.53" @tag(app_image, type=string)

// Note: tags should have a reasonable default value for cue export.
_Tags: {
	// Standardized parameters
	component: core.#Component & {
		name: string | *"no-name" @tag(holos_component_name, type=string)
		path: string | *"no-path" @tag(holos_component_path, type=string)

		_labels_json: string | *"" @tag(holos_component_labels, type=string)
		_labels: {}
		if _labels_json != "" {
			_labels: json.Unmarshal(_labels_json)
		}
		for k, v in _labels {
			labels: (k): v
		}

		_annotations_json: string | *"" @tag(holos_component_annotations, type=string)
		_annotations: {}
		if _annotations_json != "" {
			_annotations: json.Unmarshal(_annotations_json)
		}
		for k, v in _annotations {
			annotations: (k): v
		}
	}
}

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

// _CABundlePEM is the per-cluster local-ca certificate (PEM) injected at apply
// time so a quay.holos.run resource (the my-project Organization) can carry the
// trust anchor the holos-controller needs to verify the in-cluster Quay
// registry's mkcert-signed serving certificate (HOL-1319/HOL-1320).
//
// The default is the EMPTY string so an unparameterized `holos render platform`
// (and scripts/render's clean-tree gate) stays diff-clean: when the tag is
// empty the consuming component omits spec.caBundle entirely, so the committed
// holos/deploy/ tree carries no per-cluster CA material.  scripts/apply-my-project
// injects the cluster's local-ca PEM:
//
//	holos render platform --inject ca_bundle_pem="$PEM" --write-to <tmp>
//
// mirroring the app_image inject path (scripts/publish).  The consuming
// component base64-encodes the PEM with encoding/base64 to satisfy the
// caBundle field's []byte/base64 serialization (api/quay/v1alpha1).
_CABundlePEM: string | *"" @tag(ca_bundle_pem, type=string)

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

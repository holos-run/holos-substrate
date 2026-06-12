package holos

// This file is a CUE ancestor of the two argocd leaf components (crds,
// controller), so each component instance includes it without imports.  It
// holds the single Argo CD version pin and the names shared by both.

// ArgoCDChartVersion pins the argo-cd Helm chart for the controller
// component.  Chart 9.5.15 installs Argo CD app version 3.4.2 (the chart's
// appVersion, mirrored in ArgoCDAppVersion below) — matching the reference
// platform's pin.  Argo CD >= 3.1 is required: it introduced first-class
// OCI repository support, which the rendered-manifests source pattern
// (HOL-1185) depends on.  The official quay.io/argoproj/argocd:v3.4.2
// image is a multi-arch manifest list including linux/arm64 — the standard
// architecture here, because the cluster is k3d on OrbStack/Apple silicon.
// Before bumping, re-check the chart's appVersion is still >= 3.1 and that
// the app image tag still publishes linux/arm64.
ArgoCDChartVersion: "9.5.15"

// ArgoCDAppVersion pins the Argo CD application version: the CRDs the crds
// component fetches via crds/read-thru-cache derive from it (upstream git
// tags carry a leading v).  This MUST match the appVersion declared in the
// vendored chart's Chart.yaml
// (controller/vendor/9.5.15/argo-cd/Chart.yaml — verified 2026-06-12) so
// the CRDs applied ahead of the controllers are exactly the versions the
// chart's workloads expect.  Re-check Chart.yaml's appVersion whenever
// ArgoCDChartVersion is bumped.  The invariant is deliberately NOT a
// render-time assertion: embedding the vendored Chart.yaml has a bootstrap
// problem (vendor/<version>/ does not exist until the first render after a
// bump), and drift is reviewable anyway — a bump regenerates the committed
// CRD bundle and deploy tree, so a mismatched pair is visible in the diff.
ArgoCDAppVersion: "3.4.2"

// ArgoCDNamespace is the namespace Argo CD runs in.  Keep the conventional
// upstream default (argocd); this platform has no environment dimension to
// encode in the namespace name.
//
// Keep this value in sync with the "argocd" entry in the central
// namespaces registry (holos/namespaces.cue): this file is an ancestor only
// of the argocd leaf components, so the registry at the holos root cannot
// reference it — the two literal values must match.  The
// #RegisteredNamespace constraint (holos/namespaces.cue) turns silent drift
// between the two literals into a render failure.
ArgoCDNamespace: "argocd"
ArgoCDNamespace: #RegisteredNamespace

// ArgoCDRepository is the upstream Helm chart repository.
ArgoCDRepository: {
	name: "argo"
	url:  "https://argoproj.github.io/argo-helm"
}

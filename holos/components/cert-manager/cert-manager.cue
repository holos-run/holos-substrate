package holos

// This file is a CUE ancestor of the two cert-manager leaf components (crds,
// controller), so each component instance includes it without imports.  It
// holds the single cert-manager version pin and the names shared by both.

// CertManagerVersion pins cert-manager for both components: the controller
// Helm chart version and the CRDs release manifest fetched by
// crds/read-thru-cache derive from it (upstream tags carry a leading v, so
// both interpolate "v" + this value).  cert-manager 1.19 supports Kubernetes
// 1.31 through 1.35 per https://cert-manager.io/docs/releases/ (checked
// 2026-06-11); the local k3d cluster runs k3s v1.31.5 (the k3d v5.8.3
// default image), which cert-manager 1.20 drops (1.20 supports 1.32+).
// Re-check the supported-releases page against the cluster's k3s version
// before bumping.
CertManagerVersion: "1.19.5"

// CertManagerNamespace is the namespace cert-manager runs in.  Keep the
// upstream default (cert-manager): scripts/local-ca pre-creates this
// namespace and stages the local-ca Secret in it before cert-manager
// installs, and cert-manager resolves ClusterIssuer secret references in
// the namespace it runs in (the cluster resource namespace).
//
// Keep this value in sync with the "cert-manager" entry in the central
// namespaces registry (holos/namespaces.cue): this file is an ancestor only
// of the cert-manager leaf components, so the registry at the holos root
// cannot reference it — the two literal values must match.  The registry IS
// an ancestor of these leaf instances, so the constraint below checks
// membership in this direction: CertManagerNamespace must unify with one of
// the registered namespace names, turning silent drift between the two
// literals into a render failure.
CertManagerNamespace: "cert-manager"
CertManagerNamespace: or([for NAME, _ in namespaces {NAME}])

// CertManagerRepository is the upstream Helm chart repository.
CertManagerRepository: {
	name: "jetstack"
	url:  "https://charts.jetstack.io"
}

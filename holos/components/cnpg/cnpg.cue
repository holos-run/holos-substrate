package holos

// This file is a CUE ancestor of the two CloudNativePG leaf components
// (crds, operator), so each component instance includes it without imports.
// It holds the single CNPG version pin and the namespace shared by both.
//
// Upstream publishes one release manifest carrying both the CRDs and the
// operator — unlike cert-manager there is no separate CRDs-only asset — so
// the shared read-thru-cache script in this directory caches the bundle
// exactly once (manifests/bundle.<VERSION>.yaml, committed) and each leaf
// component filters it by kind with the sibling filter-kinds script before
// slicing.  The upstream YAML manifest is CNPG's primary install method and
// avoids mapping Helm chart versions to operator versions, which is why this
// shape was chosen over vendoring the cloudnative-pg chart.

// CnpgVersion pins CloudNativePG for both components: the release manifest
// URL and cache filename used by read-thru-cache derive from it.  CNPG 1.29
// officially supports Kubernetes 1.33 through 1.35 and lists 1.29 through
// 1.32 as tested-but-not-supported per
// https://cloudnative-pg.io/documentation/current/supported_releases/
// (checked 2026-06-11); the local k3d cluster runs k3s v1.31.5 (the k3d
// v5.8.3 default image), which falls in 1.29's tested tier — no in-support
// CNPG minor still lists 1.31 as officially supported (1.28.x, EOL
// 2026-06-30, starts at 1.32).  The pinned image
// ghcr.io/cloudnative-pg/cloudnative-pg:1.29.1 publishes a multi-arch
// manifest including linux/arm64 (verified against the ghcr.io image index
// 2026-06-11), matching the Apple Silicon MVP target (ADR-7).  Re-check the
// supported-releases page against the cluster's k3s version before bumping.
CnpgVersion: "1.29.1"

// CnpgNamespace is the namespace the CNPG operator runs in.  Keep the
// upstream default (cnpg-system): the release manifest hardcodes it in every
// namespaced resource and in the webhook clientConfig service references, so
// changing it would mean rewriting the bundle rather than filtering it.  The
// operator component strips the Namespace resource from the bundle; the
// namespace itself is registered centrally per the component guidelines.
//
// Keep this value in sync with the "cnpg-system" entry in the central
// namespaces registry (holos/namespaces.cue): this file is an ancestor only
// of the cnpg leaf components, so the registry at the holos root cannot
// reference it — the two literal values must match.  The
// #RegisteredNamespace constraint (holos/namespaces.cue) turns silent drift
// between the two literals into a render failure.
CnpgNamespace: "cnpg-system"
CnpgNamespace: #RegisteredNamespace

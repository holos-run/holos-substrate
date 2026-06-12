package holos

// This file is a CUE ancestor of the two Keycloak operator leaf components
// (operator-crds, operator), so each component instance includes it without
// imports.  It holds the single Keycloak version pin and the namespace
// shared by both.
//
// Deployment-method decision: the Keycloak Operator was chosen over a plain
// StatefulSet/Deployment of the quay.io/keycloak/keycloak image.  The
// operator provides the Keycloak CR — declarative external-DB
// configuration, TLS, hostname, and resource sizing — and the
// KeycloakRealmImport CR, the mechanism that satisfies "the holos realm
// exists after a clean bootstrap with no manual clicks" declaratively.  It
// also auto-generates the initial admin Secret.  A plain StatefulSet would
// require hand-rolling realm import jobs and server configuration; the
// reference platform (holos-reference) runs the operator in production, so
// this repo follows it.
//
// Upstream publishes the operator as separate raw manifests per version
// under https://raw.githubusercontent.com/keycloak/keycloak-k8s-resources/
// <VERSION>/kubernetes/: two single-CRD files (keycloaks.k8s.keycloak.org,
// keycloakrealmimports.k8s.keycloak.org) and kubernetes.yml (the operator
// Deployment, ServiceAccount, and RBAC).  Unlike CNPG there is no combined
// bundle to split by kind: each leaf component fetches exactly its own
// asset with its own read-thru-cache script, caching it as
// manifests/bundle.<VERSION>.yaml, committed.

// KeycloakVersion pins Keycloak for both components: the CRD and operator
// manifest URLs and cache filenames used by each leaf's read-thru-cache
// derive from it, and the operator Deployment deploys the same version of
// the server (its RELATED_IMAGE_KEYCLOAK env var pins
// quay.io/keycloak/keycloak:<VERSION>), so the operator and the Keycloak
// server image stay on the same version line by construction.  26.6.3 is
// the current 26.6.x patch release (checked against
// https://github.com/keycloak/keycloak/releases 2026-06-11; the reference
// platform pins 26.6.2 on the same minor).  Both
// quay.io/keycloak/keycloak:26.6.3 and
// quay.io/keycloak/keycloak-operator:26.6.3 publish multi-arch manifest
// lists including linux/arm64 (verified against the quay.io image indexes
// 2026-06-11), matching the Apple Silicon MVP target (ADR-7).  Re-check the
// keycloak-k8s-resources tags and both image indexes before bumping.
KeycloakVersion: "26.6.3"

// KeycloakNamespace is the namespace the Keycloak operator and the Keycloak
// server run in.  The upstream kubernetes.yml resources carry no namespace
// (upstream docs apply them with -n keycloak), so each operator component
// sets it via its Kustomize transformer's kustomization.namespace; the
// upstream ClusterRoleBinding subject already references the keycloak
// namespace, so this matches upstream's expectation.  The namespace itself
// is registered centrally (holos/namespaces.cue) with _ambient: false —
// see the registry entry and holos/docs/mesh-enrollment.md for the
// rationale — never emitted by components.
//
// Keep this value in sync with the "keycloak" entry in the central
// namespaces registry (holos/namespaces.cue): this file is an ancestor only
// of the keycloak leaf components, so the registry at the holos root cannot
// reference it — the two literal values must match.  The
// #RegisteredNamespace constraint (holos/namespaces.cue) turns silent drift
// between the two literals into a render failure.
KeycloakNamespace: "keycloak"
KeycloakNamespace: #RegisteredNamespace

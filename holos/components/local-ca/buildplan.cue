package holos

// local-ca emits the CA ClusterIssuer that signs every platform certificate.
// It references the local-ca Secret in the cert-manager namespace: the mkcert
// root CA staged by scripts/local-ca before cert-manager installs (see
// docs/local-cluster.md).  mkcert --install puts that same root CA in the
// host trust store, so certificates the issuer signs are trusted by browsers
// and other host clients without extra configuration.
//
// The secret reference names no namespace: cert-manager resolves
// ClusterIssuer secrets in its cluster resource namespace, which defaults to
// the namespace cert-manager runs in (cert-manager — see CertManagerNamespace
// in components/cert-manager/cert-manager.cue).
//
// This is a separate component, not part of the cert-manager component, so
// the ordered apply can wait for the cert-manager webhook to be ready to
// admit cert-manager.io resources before the ClusterIssuer applies.
let ISSUER = {
	apiVersion: "cert-manager.io/v1"
	kind:       "ClusterIssuer"
	metadata: name: "local-ca"
	spec: ca: secretName: "local-ca"
}

// The one-file-per-resource guardrail is satisfied CUE-natively: the single
// artifact is produced by a single Resources generator holding exactly one
// resource, so no Kustomize bundle or kubectl-slice transformer is needed.
userDefinedBuildPlan: {
	metadata: name: "local-ca"
	spec: artifacts: manifests: {
		// clusterissuer-<name>.yaml matches the kubectl-slice naming
		// convention used everywhere else in the deploy tree.
		"clusters/\(clusterName)/components/\(metadata.name)/clusterissuer-\(ISSUER.metadata.name).yaml": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: artifact
				// Unify with #Resources (holos/resources.cue) so the issuer
				// validates against the vendored cert-manager schemas at
				// render time.
				resources: #Resources & {
					ClusterIssuer: (ISSUER.metadata.name): ISSUER
				}
			}]
		}
	}
}

package holos

// cnpg-clusters emits one CloudNativePG Cluster per platform service that
// needs Postgres: keycloak-db and quay-db.  Each Cluster lives in its
// consuming service's namespace so the CNPG-generated credentials Secret
// lands next to its consumer.  CNPG is the platform's single Postgres
// operator (components/cnpg); this component carries no version pin of its
// own — the Postgres image is the operator default for the CnpgVersion
// pinned in components/cnpg/cnpg.cue, so a CNPG bump is the one line that
// moves both.
//
// Generated names (CNPG conventions) — the contract the keycloak
// (components/keycloak/instance) and quay (components/quay) components
// consume; also documented in holos/README.md:
//
//   - Credentials Secret: <cluster>-app — keycloak-db-app in the keycloak
//     namespace, quay-db-app in the quay namespace; keys: username,
//     password, dbname, host, port, uri, jdbc-uri.
//   - Read-write Service: <cluster>-rw — keycloak-db-rw.keycloak.svc and
//     quay-db-rw.quay.svc, port 5432.
//
// The CNPG operator's admission webhooks for postgresql.cnpg.io resources
// fail closed, so scripts/apply orders this component after the operator,
// waits for the controller-manager rollout, and retries through the brief
// window before the webhook serves.

// DATABASES declares the per-service Postgres clusters, keyed by Cluster
// name.  The #RegisteredNamespace constraint (holos/namespaces.cue) turns
// silent drift between a namespace literal here and the central registry
// into a render failure instead of an apply-time NotFound error.
let DATABASES = {
	"keycloak-db": {
		namespace: "keycloak" & #RegisteredNamespace
		database:  "keycloak"
		// Keep the keycloak-db Postgres pods OUT of the Istio ambient mesh,
		// even though the keycloak namespace is now ambient-enrolled (HOL-1362):
		// inheritedMetadata propagates istio.io/dataplane-mode: none onto the
		// Cluster's pods so the plaintext Keycloak↔Postgres hop
		// (keycloak-db-rw:5432, no sslmode) is not re-wrapped by ztunnel HBONE.
		// Scoped to THIS database only; quay-db stays in the mesh.
		inheritedMetadata: labels: "istio.io/dataplane-mode": "none"
	}
	"quay-db": {
		namespace: "quay" & #RegisteredNamespace
		database:  "quay"
	}
}

// The one-file-per-resource guardrail is satisfied CUE-natively: each
// artifact is produced by a single Resources generator holding exactly one
// Cluster, so no Kustomize bundle or kubectl-slice transformer is needed.
userDefinedBuildPlan: {
	metadata: name: "cnpg-clusters"
	spec: artifacts: manifests: {
		for NAME, DB in DATABASES {
			// cluster-<name>.yaml matches the kubectl-slice naming
			// convention used everywhere else in the deploy tree.
			"clusters/\(clusterName)/components/\(metadata.name)/cluster-\(NAME).yaml": {
				artifact: _
				generators: [{
					kind:   "Resources"
					output: artifact
					// Unify with #Resources (holos/resources.cue) so the
					// Cluster validates against the vendored CNPG schema at
					// render time.
					resources: #Resources & {
						Cluster: (NAME): {
							apiVersion: "postgresql.cnpg.io/v1"
							kind:       "Cluster"
							metadata: {
								name:      NAME
								namespace: DB.namespace
							}
							spec: {
								// Per-database pod metadata (e.g. keycloak-db's
								// istio.io/dataplane-mode: none); only set when the
								// DATABASES entry declares it.
								if DB.inheritedMetadata != _|_ {
									inheritedMetadata: DB.inheritedMetadata
								}
								// One laptop-sized instance per service: the
								// MVP demo target is a single Apple Silicon
								// Mac (ADR-7) — no HA replicas.
								instances: 1
								// storageClass is deliberately omitted: the
								// PVC binds to the k3s default local-path
								// StorageClass on the local cluster.
								storage: size: "2Gi"
								resources: {
									requests: {
										memory: "256Mi"
										cpu:    "100m"
									}
									limits: memory: "512Mi"
								}
								// initdb bootstraps the service database and
								// its owner role; CNPG generates the owner's
								// credentials in the <cluster>-app Secret
								// documented above.
								bootstrap: initdb: {
									database: DB.database
									owner:    DB.database
								}
							}
						}
					}
				}]
			}
		}
	}
}

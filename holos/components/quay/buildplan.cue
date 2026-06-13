package holos

// quay renders the Quay registry as a plain Deployment backed by the
// quay-db CNPG Postgres Cluster (components/cnpg-clusters) and a minimal
// single-pod Redis, with registry blob storage on a local-path PVC, exposed
// at https://quay.holos.localhost through the shared Gateway
// (components/istio-gateway).  This component brings up the UI and the v2
// registry API; users and credentials are bootstrapped by scripts/quay-init
// (HOL-1177), which uses the /api/v1/user/initialize endpoint enabled by
// FEATURE_USER_INITIALIZE below.
//
// Quay reads /conf/stack/config.yaml and performs no environment-variable
// substitution, and the database credentials are CNPG-generated (never
// committed), so the committed ConfigMap holds a config *template* and an
// initContainer renders the real config at pod start: it substitutes the
// DB URI (from the quay-db-app Secret — the contract in holos/README.md
// "Postgres credentials and connection contract") and the two secret keys
// (from the quay-secret-keys Secret the bootstrap Job below creates) into
// the template and writes the result to an emptyDir shared with the main
// container at /conf/stack.
//
// The quay Namespace — including its ambient mesh enrollment label — is
// registered in the central namespaces registry (holos/namespaces.cue) and
// rendered by the namespaces component.

// VERSION pins the Quay registry image tag.  quay.io/projectquay/quay:3.17.3
// is an OCI index whose platforms are linux/{amd64,arm64,ppc64le,s390x} —
// linux/arm64 is required because the cluster is k3d on OrbStack/Apple
// silicon — verified 2026-06-12 via the quay.io registry API (the manifest
// list for the tag).  The image config declares USER 1001 (non-root) and
// exposes 8080, the plain-HTTP port the Service and probes below use.
// Before bumping, re-check the new tag's manifest list still includes
// linux/arm64 and that the image still serves HTTP on 8080 with the
// /health/instance endpoint.
let VERSION = "3.17.3"

// REDIS_VERSION pins the Redis image for Quay's ephemeral state.  The
// official docker.io/library/redis 8.6.4-alpine tag is a manifest list
// including linux/arm64 (checked 2026-06-12 via the Docker Hub registry
// API); 8.6 is the current mature stable line.  Quay uses Redis only for
// build logs and user events — both ephemeral — so no persistence is
// configured and any stable multi-arch tag works.  Re-check the tag's
// manifest list includes linux/arm64 before bumping.
let REDIS_VERSION = "8.6.4-alpine"

// KUBECTL_IMAGE pins the image the secret-keys bootstrap Job runs kubectl
// from.  docker.io/alpine/kubectl:1.33.3 is a manifest list including
// linux/arm64 (checked 2026-06-12 via the Docker Hub registry API) and is
// alpine-based, providing the /bin/sh the Job script needs (the
// version-matched rancher/kubectl image is scratch-based — no shell).
// 1.33.3 is the oldest tag the repository publishes; it exceeds the
// kubectl +/-1 minor skew recommendation against the live server
// (v1.31.5+k3s1) but the Job performs only core/v1 Secret get/create,
// which are version-stable.  Re-check available tags against the cluster
// version before bumping.
let KUBECTL_IMAGE = "docker.io/alpine/kubectl:1.33.3"

// The #RegisteredNamespace constraint (holos/namespaces.cue) turns silent
// drift between this literal and the registry entry into a render failure:
// if "quay" is ever removed or renamed in holos/namespaces.cue, rendering
// fails here instead of at apply time with a NotFound namespace error.
let NAMESPACE = "quay" & #RegisteredNamespace
let NAME = "quay"
let PORT = 8080

// quay.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md, and the Keycloak realm's placeholder quay client
// already lists https://quay.holos.localhost/* as a redirect URI.
// k3d-registry.holos.localhost is deliberately NOT used: that name belongs to
// the k3d bootstrap registry on port 5100 (scripts/local-k3d).
let HOSTNAME = "quay.holos.localhost"

// The shared Gateway's namespace and name (components/istio-gateway).
// GATEWAY_NAME feeds both the HTTPRoute parentRefs and the ServiceEntry
// endpoint below, keeping this component's references to the Gateway
// mutually consistent.  Nothing ties the literal to the istio-gateway
// component at render time, so a Gateway rename still surfaces only at
// runtime — update both components together.
let GATEWAY_NAMESPACE = "istio-gateways"
let GATEWAY_NAME = "default"

let REDIS_NAME = "quay-redis"
let REDIS_PORT = 6379

// SECRET_KEYS is the Secret the bootstrap Job creates and the Quay pod's
// initContainer reads; PVC_NAME is the registry blob storage claim.
let SECRET_KEYS = "quay-secret-keys"
let PVC_NAME = "quay-datastorage"
let CONFIG_TEMPLATE = "quay-config-template"
let BOOTSTRAP = "quay-secret-keys-bootstrap"

let METADATA = {
	name:      NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": NAME
}

let REDIS_METADATA = {
	name:      REDIS_NAME
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": REDIS_NAME
}

// The config template the initContainer renders into /conf/stack/config.yaml.
// The __TOKEN__ placeholders are replaced with JSON-encoded values (JSON
// strings are valid YAML scalars), so credential values never need to be
// YAML-escaped by hand and never appear in this repository.
//
// Field notes:
//   - EXTERNAL_TLS_TERMINATION: the shared Gateway terminates TLS; Quay
//     itself serves plain HTTP on 8080.
//   - BUILDLOGS_REDIS and USER_EVENTS_REDIS are both mandatory even though
//     the build feature is unused.
//   - FEATURE_USER_INITIALIZE enables the one-shot /api/v1/user/initialize
//     endpoint scripts/quay-init (HOL-1177) uses to create the admin user.
//   - SETUP_COMPLETE skips the interactive setup flow.
let CONFIG_YAML = """
	SERVER_HOSTNAME: \(HOSTNAME)
	PREFERRED_URL_SCHEME: https
	EXTERNAL_TLS_TERMINATION: true
	SETUP_COMPLETE: true
	DB_URI: __DB_URI__
	SECRET_KEY: __SECRET_KEY__
	DATABASE_SECRET_KEY: __DATABASE_SECRET_KEY__
	BUILDLOGS_REDIS:
	  host: \(REDIS_NAME)
	  port: \(REDIS_PORT)
	USER_EVENTS_REDIS:
	  host: \(REDIS_NAME)
	  port: \(REDIS_PORT)
	DISTRIBUTED_STORAGE_CONFIG:
	  default:
	    - LocalStorage
	    - storage_path: /datastorage/registry
	DISTRIBUTED_STORAGE_PREFERENCE:
	  - default
	FEATURE_USER_INITIALIZE: true
	SUPER_USERS:
	  - admin
	FEATURE_MAILING: false
	"""

// The initContainer script: render the config, then prepare the database.
// json.dumps the env values so any character CNPG or the bootstrap Job
// might generate stays a valid YAML scalar; python3 and psycopg2 ship in
// the Quay image, which the initContainer reuses to avoid a second pin.
//
// Quay's config validator refuses to start without the pg_trgm extension
// in its database.  pg_trgm is a *trusted* extension (PostgreSQL 13+), so
// the CNPG-generated app user — owner of the quay database via initdb
// (components/cnpg-clusters) — can CREATE it without superuser.  Creating
// it here keeps the dependency level-triggered: it converges on an
// already-bootstrapped Cluster exactly like a fresh one, with no manual
// step and no bootstrap-only hook.
let INIT_SCRIPT = """
	import json, os

	import psycopg2

	template = open("/conf/template/config.yaml").read()
	for key in ("DB_URI", "SECRET_KEY", "DATABASE_SECRET_KEY"):
	    template = template.replace("__%s__" % key, json.dumps(os.environ[key]))
	open("/conf/stack/config.yaml", "w").write(template)

	conn = psycopg2.connect(os.environ["DB_URI"])
	conn.autocommit = True
	conn.cursor().execute("CREATE EXTENSION IF NOT EXISTS pg_trgm")
	conn.close()
	"""

// The create-if-absent bootstrap script for the quay-secret-keys Secret.
// DATABASE_SECRET_KEY encrypts fields inside Postgres, so it MUST be stable
// across restarts and MUST NOT be committed: the Job generates it once and
// never touches an existing Secret.  Keys are alphanumeric (base64 with
// +/=/newlines stripped) so they are safe in any downstream context.  The
// length checks guard against an improbable pipeline failure under set -eu
// (no pipefail in busybox sh) silently creating empty keys — empty keys
// would otherwise become permanent by the create-if-absent design.  The
// Secret is piped as a manifest on stdin so the key material never appears
// in the container's argv (/proc-visible).
let BOOTSTRAP_SCRIPT = """
	set -eu
	if kubectl -n \(NAMESPACE) get secret \(SECRET_KEYS) >/dev/null 2>&1; then
	  echo "Secret \(SECRET_KEYS) already exists; leaving it untouched."
	  exit 0
	fi
	random_key() {
	  head -c 256 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c 1-48
	}
	SECRET_KEY="$(random_key)"
	DATABASE_SECRET_KEY="$(random_key)"
	[ "${#SECRET_KEY}" -eq 48 ]
	[ "${#DATABASE_SECRET_KEY}" -eq 48 ]
	kubectl -n \(NAMESPACE) create -f - <<EOF
	apiVersion: v1
	kind: Secret
	metadata:
	  name: \(SECRET_KEYS)
	  namespace: \(NAMESPACE)
	stringData:
	  SECRET_KEY: "${SECRET_KEY}"
	  DATABASE_SECRET_KEY: "${DATABASE_SECRET_KEY}"
	EOF
	echo "Secret \(SECRET_KEYS) created."
	"""

let CONFIG_TEMPLATE_CM = {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: {
		name:      CONFIG_TEMPLATE
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	data: "config.yaml": CONFIG_YAML
}

// The bootstrap resources carry their own app.kubernetes.io/name — NOT the
// Quay Deployment's — because the quay Service selects on that label: a
// probe-less bootstrap pod labeled like the Quay pod would become a dead
// Service endpoint for the seconds it runs whenever the Job re-runs after
// TTL garbage collection.
let BOOTSTRAP_METADATA = {
	name:      BOOTSTRAP
	namespace: NAMESPACE
	labels: "app.kubernetes.io/name": BOOTSTRAP
}

let BOOTSTRAP_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   BOOTSTRAP_METADATA
}

// Scoped to the one Secret the Job manages: get is restricted to the
// quay-secret-keys resourceName; create cannot be restricted by
// resourceName (the API server does not evaluate resourceNames for create
// requests), so the create grant is namespace-wide on secrets — acceptable
// in a namespace whose Secrets all belong to this service.
let BOOTSTRAP_ROLE = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "Role"
	metadata:   BOOTSTRAP_METADATA
	rules: [
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["get"]
			resourceNames: [SECRET_KEYS]
		},
		{
			apiGroups: [""]
			resources: ["secrets"]
			verbs: ["create"]
		},
	]
}

let BOOTSTRAP_ROLE_BINDING = {
	apiVersion: "rbac.authorization.k8s.io/v1"
	kind:       "RoleBinding"
	metadata:   BOOTSTRAP_METADATA
	roleRef: {
		apiGroup: "rbac.authorization.k8s.io"
		kind:     "Role"
		name:     BOOTSTRAP
	}
	subjects: [{
		kind:      "ServiceAccount"
		name:      BOOTSTRAP
		namespace: NAMESPACE
	}]
}

// CAVEAT: a completed Job's pod template is immutable.  Server-side
// re-apply of this unchanged spec is a no-op while the Job exists, and
// ttlSecondsAfterFinished garbage-collects it a day after completion —
// after that a re-apply recreates the Job, which exits 0 against the
// existing Secret.  Only a pod-template change within the TTL window
// requires deleting the old Job first
// (kubectl -n quay delete job quay-secret-keys-bootstrap) — the Secret it
// created survives, and the new Job exits 0 without touching it.
let BOOTSTRAP_JOB = {
	apiVersion: "batch/v1"
	kind:       "Job"
	metadata:   BOOTSTRAP_METADATA
	spec: {
		backoffLimit: 3
		// A day keeps the Job's logs around for debugging a fresh
		// bootstrap while still dissolving the immutable-pod-template
		// caveat above for routine re-applies.
		ttlSecondsAfterFinished: 86400
		template: {
			// The distinct label matters here most of all: the quay
			// Service must never select this pod (see BOOTSTRAP_METADATA).
			metadata: labels: BOOTSTRAP_METADATA.labels
			spec: {
				serviceAccountName: BOOTSTRAP
				restartPolicy:      "Never"
				securityContext: {
					runAsNonRoot: true
					// The alpine/kubectl image declares no non-root USER;
					// pick the conventional "nobody" uid.
					runAsUser:  65534
					runAsGroup: 65534
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "bootstrap"
					image: KUBECTL_IMAGE
					command: ["/bin/sh", "-c", BOOTSTRAP_SCRIPT]
					// kubectl writes its discovery cache under $HOME; point
					// it at the writable emptyDir since the root filesystem
					// is read-only.
					env: [{
						name:  "HOME"
						value: "/tmp"
					}]
					resources: {
						requests: {
							cpu:    "10m"
							memory: "32Mi"
						}
						limits: memory: "64Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					volumeMounts: [{
						name:      "tmp"
						mountPath: "/tmp"
					}]
				}]
				volumes: [{
					name: "tmp"
					emptyDir: {}
				}]
			}
		}
	}
}

let REDIS_DEPLOYMENT = {
	apiVersion: "apps/v1"
	kind:       "Deployment"
	metadata:   REDIS_METADATA
	spec: {
		replicas: 1
		selector: matchLabels: REDIS_METADATA.labels
		template: {
			metadata: labels: REDIS_METADATA.labels
			spec: {
				// Redis never talks to the Kubernetes API; don't mount a
				// ServiceAccount token it has no use for.
				automountServiceAccountToken: false
				securityContext: {
					runAsNonRoot: true
					// The official redis alpine image creates the redis user
					// as uid 999 in group 1000.
					runAsUser:  999
					runAsGroup: 1000
					seccompProfile: type: "RuntimeDefault"
				}
				containers: [{
					name:  "redis"
					image: "docker.io/library/redis:\(REDIS_VERSION)"
					// Quay uses Redis only for build logs and user events —
					// both ephemeral — so snapshotting and AOF are disabled
					// explicitly; nothing needs to survive a restart.  No
					// auth: the Service is in-cluster only and nothing
					// sensitive transits it.
					args: ["redis-server", "--save", "", "--appendonly", "no"]
					ports: [{
						name:          "redis"
						containerPort: REDIS_PORT
						protocol:      "TCP"
					}]
					// TCP probes suffice for a single-purpose cache: redis
					// accepts connections only once it is serving.
					readinessProbe: tcpSocket: port: REDIS_PORT
					livenessProbe: tcpSocket: port:  REDIS_PORT
					// Laptop sizing: an ephemeral cache for one Quay
					// instance idles far below these.
					resources: {
						requests: {
							cpu:    "25m"
							memory: "32Mi"
						}
						limits: memory: "128Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					// /data is the image's working directory; with
					// persistence disabled redis writes nothing there, but
					// an emptyDir keeps the read-only root filesystem viable
					// if a future flag change re-enables snapshots.
					volumeMounts: [{
						name:      "data"
						mountPath: "/data"
					}]
				}]
				volumes: [{
					name: "data"
					emptyDir: {}
				}]
			}
		}
	}
}

let REDIS_SERVICE = {
	apiVersion: "v1"
	kind:       "Service"
	metadata:   REDIS_METADATA
	spec: {
		selector: REDIS_METADATA.labels
		ports: [{
			name:       "redis"
			port:       REDIS_PORT
			targetPort: REDIS_PORT
			protocol:   "TCP"
		}]
	}
}

// The Quay Deployment's dedicated identity.  It exists for precision, not
// permissions: it carries no role bindings (the mounted token satisfies
// Quay's KubernetesConfigProvider, which only needs the token file to
// exist), and the Redis AuthorizationPolicy below pins to this principal,
// so a future pod that omits serviceAccountName never silently gains
// Redis access via the namespace default ServiceAccount.
let QUAY_SERVICE_ACCOUNT = {
	apiVersion: "v1"
	kind:       "ServiceAccount"
	metadata:   METADATA
}

// Redis runs without auth, so make the in-cluster-only claim true by
// construction: the quay namespace is ambient-enrolled
// (holos/namespaces.cue), ztunnel enforces L4 authorization, and this
// policy allows only the Quay pod's identity — the dedicated quay
// ServiceAccount above — to connect.  Kubelet health probes are exempt
// from ambient capture, so the TCP probes above keep working.
let REDIS_AUTHZ = {
	apiVersion: "security.istio.io/v1"
	kind:       "AuthorizationPolicy"
	metadata: {
		name:      REDIS_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": REDIS_NAME
	}
	spec: {
		selector: matchLabels: REDIS_METADATA.labels
		action: "ALLOW"
		rules: [{
			from: [{source: principals: ["cluster.local/ns/\(NAMESPACE)/sa/\(QUAY_SERVICE_ACCOUNT.metadata.name)"]}]
		}]
	}
}

// Registry blob storage.  storageClassName is deliberately omitted: the
// claim binds to the k3s default local-path StorageClass on the local
// cluster (the same pattern as the cnpg-clusters component's Cluster
// storage).  The local-path provisioner creates the backing directory
// world-writable, so the Quay container's uid 1001 can write without an
// fsGroup.
let PVC = {
	apiVersion: "v1"
	kind:       "PersistentVolumeClaim"
	metadata: {
		name:      PVC_NAME
		namespace: NAMESPACE
		labels: "app.kubernetes.io/name": NAME
	}
	spec: {
		accessModes: ["ReadWriteOnce"]
		resources: requests: storage: "5Gi"
	}
}

let DEPLOYMENT = {
	apiVersion: "apps/v1"
	kind:       "Deployment"
	metadata:   METADATA
	spec: {
		replicas: 1
		// The RWO local-path volume cannot be shared by the old and new pod
		// of a rolling update; Recreate stops the old pod before the new
		// one claims the volume.
		strategy: type:        "Recreate"
		selector: matchLabels: METADATA.labels
		template: {
			metadata: labels: METADATA.labels
			spec: {
				// The dedicated ServiceAccount pins the pod's mesh identity
				// for the Redis AuthorizationPolicy.  Its token IS mounted
				// (the default), unlike the other pods in this component:
				// Quay selects its KubernetesConfigProvider whenever
				// KUBERNETES_SERVICE_HOST is set — which the kubelet always
				// injects — and that provider refuses to start without a
				// token file (verified on the live cluster: "Cannot load
				// Kubernetes service account token").  The token is inert:
				// the ServiceAccount has no role bindings, so it grants no
				// API access.
				serviceAccountName: QUAY_SERVICE_ACCOUNT.metadata.name
				// The Quay image config declares USER 1001 (verified with
				// the VERSION pin above), so runAsNonRoot validates without
				// forcing a uid the image layout doesn't expect.
				securityContext: {
					runAsNonRoot: true
					seccompProfile: type: "RuntimeDefault"
				}
				initContainers: [{
					name: "init"
					// Reuse the Quay image: it ships the python3 and
					// psycopg2 the init script needs, avoiding a second pin.
					image: "quay.io/projectquay/quay:\(VERSION)"
					command: ["python3", "-c", INIT_SCRIPT]
					env: [
						{
							name: "DB_URI"
							// The CNPG-generated connection URI:
							// postgresql://user:pass@quay-db-rw.quay:5432/quay
							// per the contract in holos/README.md "Postgres
							// credentials and connection contract".
							valueFrom: secretKeyRef: {
								name: "quay-db-app"
								key:  "uri"
							}
						},
						{
							name: "SECRET_KEY"
							valueFrom: secretKeyRef: {
								name: SECRET_KEYS
								key:  "SECRET_KEY"
							}
						},
						{
							name: "DATABASE_SECRET_KEY"
							valueFrom: secretKeyRef: {
								name: SECRET_KEYS
								key:  "DATABASE_SECRET_KEY"
							}
						},
					]
					resources: {
						requests: {
							cpu:    "10m"
							memory: "32Mi"
						}
						limits: memory: "128Mi"
					}
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
						readOnlyRootFilesystem: true
					}
					volumeMounts: [
						{
							name:      "config-template"
							mountPath: "/conf/template"
							readOnly:  true
						},
						{
							name:      "config"
							mountPath: "/conf/stack"
						},
					]
				}]
				containers: [{
					name:  NAME
					image: "quay.io/projectquay/quay:\(VERSION)"
					// Quay sizes its gunicorn worker pools from the node's
					// CPU count by default, which on a many-core dev box
					// multiplies its already large per-process footprint.
					// Pin the pools to laptop sizing (ADR-7: a single local
					// instance) — the env vars are the upstream
					// quay-entrypoint contract, the same knobs the Red Hat
					// operator sets.
					env: [
						{
							name:  "WORKER_COUNT_WEB"
							value: "2"
						},
						{
							name:  "WORKER_COUNT_REGISTRY"
							value: "2"
						},
						{
							name:  "WORKER_COUNT_SECSCAN"
							value: "1"
						},
						{
							// The WORKER_COUNT_* pins above are clamped to
							// per-pool minimums in Quay's util/workers.py —
							// the registry pool's minimum is 8, so without
							// this knob WORKER_COUNT_REGISTRY=2 silently runs
							// 8 gunicorn registry workers (~140Mi anon each),
							// which pushed the container to ~4.1Gi anon and
							// repeated OOMKills against the 4Gi limit
							// (observed on the live cluster during the
							// HOL-1178 webhook/restart verification).
							// Lowering the floor to 1 makes the pins above
							// authoritative; "UNSUPPORTED" is upstream's
							// naming for sub-minimum sizing, acceptable for a
							// single-user laptop registry (ADR-7).
							name:  "WORKER_COUNT_UNSUPPORTED_MINIMUM"
							value: "1"
						},
					]
					ports: [{
						name:          "http"
						containerPort: PORT
						protocol:      "TCP"
					}]
					// The first start runs the database schema migrations
					// before the HTTP endpoints serve; the startupProbe
					// gives that up to 10 minutes before the liveness probe
					// takes over.  /health/instance checks the database,
					// Redis, and storage from a heavyweight Python service,
					// so every probe gets an explicit generous timeout
					// instead of the 1s default.
					startupProbe: {
						httpGet: {
							path: "/health/instance"
							port: PORT
						}
						periodSeconds:    10
						failureThreshold: 60
						timeoutSeconds:   5
					}
					readinessProbe: {
						httpGet: {
							path: "/health/instance"
							port: PORT
						}
						timeoutSeconds: 5
					}
					livenessProbe: {
						httpGet: {
							path: "/health/instance"
							port: PORT
						}
						timeoutSeconds: 5
					}
					// Laptop sizing: Quay is heavy — a Python monolith whose
					// supervisord runs ~20 worker processes — so the memory
					// limit is generous relative to the other platform
					// services.  2Gi was not enough: with the default
					// CPU-scaled worker pools the first start was OOMKilled
					// before serving (observed on the live cluster), hence
					// the pinned pools above.  4Gi was still not enough:
					// before the WORKER_COUNT_UNSUPPORTED_MINIMUM floor fix
					// above the container OOMKilled roughly every 10
					// minutes at ~4.1Gi anonymous memory, and even with the
					// pools genuinely pinned it idles at ~3.6Gi — Quay's
					// ~20 single-purpose workers each carry the full Python
					// codebase — leaving under 500Mi of headroom for push
					// load (all observed on the live cluster during the
					// HOL-1178 verification).  6Gi gives real headroom; a
					// limit reserves nothing, so the only cost is on a box
					// that cannot spare it.
					resources: {
						requests: {
							cpu:    "250m"
							memory: "512Mi"
						}
						limits: memory: "6Gi"
					}
					// No readOnlyRootFilesystem: the Quay entrypoint writes
					// runtime state (supervisord configuration and logs)
					// throughout its filesystem.
					securityContext: {
						allowPrivilegeEscalation: false
						capabilities: drop: ["ALL"]
					}
					volumeMounts: [
						{
							name:      "config"
							mountPath: "/conf/stack"
						},
						{
							name:      "datastorage"
							mountPath: "/datastorage"
						},
					]
				}]
				volumes: [
					{
						name: "config-template"
						configMap: name: CONFIG_TEMPLATE
					},
					{
						name: "config"
						emptyDir: {}
					},
					{
						name: "datastorage"
						persistentVolumeClaim: claimName: PVC_NAME
					},
				]
			}
		}
	}
}

let SERVICE = {
	apiVersion: "v1"
	kind:       "Service"
	metadata:   METADATA
	spec: {
		selector: METADATA.labels
		ports: [{
			name:       "http"
			port:       PORT
			targetPort: PORT
			protocol:   "TCP"
		}]
	}
}

// Cross-namespace attachment to the shared Gateway is allowed because its
// listeners set allowedRoutes.namespaces.from: All (istio-gateway
// component).  sectionName binds this route to the https listener only:
// the registry carries credentials, so it must never be served over the
// plaintext http listener — the companion route below redirects port 80 to
// HTTPS instead.  The backend is plain HTTP on 8080 (the Gateway
// terminates TLS — EXTERNAL_TLS_TERMINATION above), so no DestinationRule
// is needed: this is the echo pattern, not the Keycloak TLS-origination
// pattern.
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
	}
	spec: {
		parentRefs: [{
			name:        GATEWAY_NAME
			namespace:   GATEWAY_NAMESPACE
			sectionName: "https"
		}]
		hostnames: [HOSTNAME]
		rules: [{
			matches: [{path: {type: "PathPrefix", value: "/"}}]
			backendRefs: [{
				name: NAME
				port: PORT
			}]
		}]
	}
}

// Companion to HTTPROUTE above: bound to the http listener only, it
// permanently redirects every plaintext request for the Quay hostname to
// HTTPS, so no registry credentials can transit port 80.  A
// RequestRedirect filter terminates the request at the Gateway; no
// backendRefs.
let HTTPROUTE_REDIRECT = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      "\(NAME)-redirect-http"
		namespace: NAMESPACE
	}
	spec: {
		parentRefs: [{
			name:        GATEWAY_NAME
			namespace:   GATEWAY_NAMESPACE
			sectionName: "http"
		}]
		hostnames: [HOSTNAME]
		rules: [{
			filters: [{
				type: "RequestRedirect"
				requestRedirect: {
					scheme:     "https"
					statusCode: 301
				}
			}]
		}]
	}
}

// In-cluster clients must reach the registry by its public hostname:
// Quay pins its OCI token-auth realm to
// https://quay.holos.localhost/v2/auth (PREFERRED_URL_SCHEME +
// SERVER_HOSTNAME above), so a client that connects via the in-cluster
// Service DNS name is still redirected to the public hostname to fetch a
// token — every v2 client needs quay.holos.localhost to resolve and route
// inside the cluster.  Plain DNS cannot provide that: *.localhost names
// resolve to loopback both upstream of CoreDNS (the host resolver
// implements RFC 6761) and inside ztunnel's DNS proxy
// (AMBIENT_DNS_CAPTURE is enabled, and ztunnel's resolver special-cases
// *.localhost before forwarding), so a CoreDNS rewrite never sees queries
// from ambient-enrolled pods.  This ServiceEntry fixes both layers at
// once: it makes quay.holos.localhost a service the mesh knows, so ztunnel
// answers enrolled pods' queries with the auto-allocated VIP and routes
// connections to that VIP to the shared Gateway, which terminates TLS for
// *.holos.localhost and routes by SNI/Host to the HTTPRoute above —
// in-cluster clients traverse the exact host path, credentials and all.
// protocol TLS keeps ztunnel at L4 (the Gateway terminates TLS);
// resolution DNS tracks the Gateway Service by name so the entry survives
// ClusterIP changes — the "<gateway>-istio" Service name is Istio's
// gateway auto-deployment convention, coupled to GATEWAY_NAME above.
// exportTo is deliberately left at its mesh-wide default: every enrolled
// namespace should resolve the registry's public hostname exactly as the
// host does.  Verified live in HOL-1188; the consumption contract is
// documented in holos/docs/argocd-application-source.md.
let SERVICE_ENTRY = {
	apiVersion: "networking.istio.io/v1"
	kind:       "ServiceEntry"
	metadata: {
		name:      "quay-holos-localhost"
		namespace: NAMESPACE
	}
	spec: {
		hosts: [HOSTNAME]
		ports: [{
			number:   443
			name:     "tls"
			protocol: "TLS"
		}]
		resolution: "DNS"
		endpoints: [{
			address: "\(GATEWAY_NAME)-istio.\(GATEWAY_NAMESPACE).svc.cluster.local"
		}]
	}
}

userDefinedBuildPlan: {
	metadata: name: "quay"
	spec: artifacts: manifests: {
		// The artifact is a directory: kubectl-slice writes one file per
		// resource so changes diff cleanly and apply tools can prune.
		"clusters/\(clusterName)/components/\(metadata.name)": {
			artifact: _
			generators: [{
				kind:   "Resources"
				output: "resources.gen.yaml"
				// Unify with #Resources (holos/resources.cue) so the
				// hand-authored resources validate against the vendored
				// Kubernetes and Gateway API schemas at render time.
				resources: #Resources & {
					ConfigMap: (CONFIG_TEMPLATE_CM.metadata.name): CONFIG_TEMPLATE_CM
					ServiceAccount: {
						(BOOTSTRAP_SERVICE_ACCOUNT.metadata.name): BOOTSTRAP_SERVICE_ACCOUNT
						(QUAY_SERVICE_ACCOUNT.metadata.name):      QUAY_SERVICE_ACCOUNT
					}
					Role: (BOOTSTRAP_ROLE.metadata.name):                BOOTSTRAP_ROLE
					RoleBinding: (BOOTSTRAP_ROLE_BINDING.metadata.name): BOOTSTRAP_ROLE_BINDING
					Job: (BOOTSTRAP_JOB.metadata.name):                  BOOTSTRAP_JOB
					Deployment: {
						(DEPLOYMENT.metadata.name):       DEPLOYMENT
						(REDIS_DEPLOYMENT.metadata.name): REDIS_DEPLOYMENT
					}
					Service: {
						(SERVICE.metadata.name):       SERVICE
						(REDIS_SERVICE.metadata.name): REDIS_SERVICE
					}
					AuthorizationPolicy: (REDIS_AUTHZ.metadata.name): REDIS_AUTHZ
					PersistentVolumeClaim: (PVC.metadata.name):       PVC
					ServiceEntry: (SERVICE_ENTRY.metadata.name):      SERVICE_ENTRY
					HTTPRoute: {
						(HTTPROUTE.metadata.name):          HTTPROUTE
						(HTTPROUTE_REDIRECT.metadata.name): HTTPROUTE_REDIRECT
					}
				}
			}]
			transformers: [
				{
					kind: "Kustomize"
					inputs: [for G in generators {G.output}]
					output: "kustomize-output-bundle.yaml"
					kustomize: kustomization: resources: inputs
				},
				{
					kind: "Command"
					inputs: [transformers[0].output]
					// this output is the artifact holos writes to the deploy
					// directory, one file per resource.
					output: artifact
					command: args: ["holos", "kubectl-slice", "-f", "\(BuildContext.tempDir)/\(inputs[0])", "-o", "\(BuildContext.tempDir)/\(artifact)"]
				},
			]
		}
	}
}

package holos

// keycloak renders the Keycloak server instance reconciled by the operator
// (the sibling operator component): the Keycloak CR backed by the CNPG
// keycloak-db Postgres Cluster (components/cnpg-clusters), running HTTP-only
// behind the shared Gateway, the declarative holos realm import, and the
// HTTPRoute attaching it to the shared Gateway (components/istio-gateway).
// The Gateway terminates external TLS once with the wildcard cert and ztunnel
// HBONE mTLS secures the Gateway→pod hop (HOL-1362), so Keycloak needs no
// per-pod TLS cert and no Gateway-side re-encryption DestinationRule.  The
// version pin and the
// deployment-method decision live in ../keycloak.cue, shared with the
// operator components: the operator's RELATED_IMAGE_KEYCLOAK env var pins
// the server image to KeycloakVersion, so the CR sets no image field and
// the operator and server stay on the same version line by construction.
//
// Admin bootstrap: the operator generates the initial admin credentials in
// the keycloak-initial-admin Secret (keys username/password) on first
// reconcile — no plaintext credentials are committed to this repository.
// Retrieve them with:
//
//	kubectl -n keycloak get secret keycloak-initial-admin -o json \
//	  | jq '.data | map_values(@base64d)'
//
// The keycloak Namespace is registered in the central namespaces registry
// (holos/namespaces.cue) with _ambient: true — see the registry entry for
// the rationale — never emitted by components.

let NAME = "keycloak"
let NAMESPACE = KeycloakNamespace

// The operator names the Service it creates "<cr-name>-service"; with CR
// name "keycloak" that is "keycloak-service".  The http.serviceName field
// could override it, but following the operator default keeps the CR
// minimal and matches the reference platform.
let SERVICE = "\(NAME)-service"

// auth.holos.internal matches the shared Gateway's *.holos.internal
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "auth.holos.internal"

// Keycloak serves plaintext HTTP on this port behind the shared Gateway; the
// operator-created keycloak-service exposes it as the port named "http", which
// the HTTPRoute backendRef targets and Istio treats as an HTTP backend.
let HTTP_PORT = 8080

// The shared Gateway's namespace (components/istio-gateway).  The Keycloak
// HTTPRoute attaches to the shared Gateway in this namespace.
let GATEWAY_NAMESPACE = "istio-gateways"

// The Keycloak CR uses v2beta1, the storage version of the pinned
// KeycloakVersion CRDs — see the #Resources comment in holos/resources.cue.
// With a real database configured the operator runs the server in start
// (production) mode, never start-dev; verify the rendered StatefulSet args on
// the live cluster after changing this CR.
let KEYCLOAK = {
	apiVersion: "k8s.keycloak.org/v2beta1"
	kind:       "Keycloak"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
	}
	spec: {
		// Laptop sizing: a single instance for the local MVP (ADR-7).
		instances: 1

		// The operator creates a classless Ingress for the hostname by
		// default — confirmed on the live cluster — outside the rendered
		// Gateway API path, which would bypass the HTTPRoute redirect and
		// the verified Gateway→Keycloak TLS hop if any default Ingress
		// controller were present.  Disable it: the shared Gateway is the
		// only ingress path (the reference platform disables it too).
		ingress: enabled: false

		// The CNPG Secret/Service contract from components/cnpg-clusters
		// (documented in holos/README.md): the keycloak-db-rw Service and
		// the keycloak-db-app credentials Secret, both in this namespace,
		// so the short Service name resolves.
		db: {
			vendor:   "postgres"
			host:     "keycloak-db-rw"
			port:     5432
			database: "keycloak"
			usernameSecret: {
				name: "keycloak-db-app"
				key:  "username"
			}
			passwordSecret: {
				name: "keycloak-db-app"
				key:  "password"
			}
		}

		// HTTP-only behind the shared Gateway: the Gateway terminates external
		// TLS once and forwards plaintext HTTP to keycloak-service:8080, and
		// ztunnel HBONE mTLS secures the Gateway→pod wire hop (the keycloak
		// namespace is ambient-enrolled, HOL-1362).  No per-pod tlsSecret.
		http: {
			httpEnabled: true
			httpPort:    HTTP_PORT
		}

		// One issuer URL resolved everywhere: browsers AND in-cluster consumers
		// (quay/argocd/kargo, and Keycloak itself) reach Keycloak through the
		// Gateway at https://auth.holos.internal, which CoreDNS resolves
		// in-cluster (HOL-1364).  backchannelDynamic is deliberately NOT set so
		// Keycloak does not advertise plaintext Service-DNS backchannel URLs
		// that consumers would reject.  hostname-strict is ignored once a full
		// hostname is configured; strict: false is kept explicit so the intended
		// posture survives if the hostname field is ever removed or made
		// relative.  See https://www.keycloak.org/server/hostname.
		hostname: {
			hostname: "https://\(HOSTNAME)"
			strict:   false
		}

		// The shared Gateway sets the X-Forwarded-* headers.
		proxy: headers: "xforwarded"

		// JVM sizing per the laptop target: ~512Mi requested for the
		// Keycloak JVM, bursting to 1Gi.
		resources: {
			requests: {
				cpu:    "250m"
				memory: "512Mi"
			}
			limits: memory: "1Gi"
		}
	}
}

// Declarative realm creation: the operator's import Job creates the holos
// realm on first reconcile, satisfying "the holos realm exists after a
// clean bootstrap with no manual clicks".
//
// CAVEAT: realm import is bootstrap-only.  The operator's import Job skips
// when the realm already exists — it does NOT reconcile changes in this CR
// into an existing realm.  Post-bootstrap realm changes need another
// mechanism (or a realm deletion and re-import, which destroys realm
// state).
let REALM_IMPORT = {
	apiVersion: "k8s.keycloak.org/v2beta1"
	kind:       "KeycloakRealmImport"
	metadata: {
		name:      "holos"
		namespace: NAMESPACE
	}
	spec: {
		keycloakCRName: KEYCLOAK.metadata.name
		realm: {
			realm:   "holos"
			enabled: true
			// No clients are declared here.  This bootstrap import creates
			// only the realm shell (realm holos, enabled: true); the live
			// "quay" OIDC client — enabled, confidential (client-secret auth,
			// no PKCE — HOL-1257), with roles, mappers, and a provisioned
			// secret — is owned and reconciled on every apply by
			// the realm-config component's keycloak-config-cli Job
			// (components/keycloak/realm-config, HOL-1218/HOL-1219).  A
			// disabled placeholder client used to live here, but it could
			// only ever disagree with the managed client (different enabled
			// state, no committed secret), so it was removed (HOL-1221).
			// realm-config converges the realm on every apply regardless of
			// bootstrap ordering, so a fresh cluster still gets a working
			// quay client without a placeholder seeded here.
		}
	}
}

// Cross-namespace attachment to the shared Gateway is allowed because its
// listeners set allowedRoutes.namespaces.from: All (istio-gateway
// component).  sectionName binds this route to the https listener only:
// Keycloak carries credentials, so it must never be served over the
// plaintext http listener — the companion route below redirects port 80 to
// HTTPS instead.
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
	}
	spec: {
		parentRefs: [{
			name:        "default"
			namespace:   GATEWAY_NAMESPACE
			sectionName: "https"
		}]
		hostnames: [HOSTNAME]
		rules: [{
			matches: [{path: {type: "PathPrefix", value: "/"}}]
			backendRefs: [{
				name: SERVICE
				port: HTTP_PORT
			}]
		}]
	}
}

// Companion to HTTPROUTE above: bound to the http listener only, it
// permanently redirects every plaintext request for the Keycloak hostname
// to HTTPS, so no login or admin traffic can transit port 80.  A
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
			name:        "default"
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

userDefinedBuildPlan: {
	metadata: name: "keycloak"
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
				// Keycloak and Gateway API schemas at render time.
				resources: #Resources & {
					Keycloak: (KEYCLOAK.metadata.name):                KEYCLOAK
					KeycloakRealmImport: (REALM_IMPORT.metadata.name): REALM_IMPORT
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

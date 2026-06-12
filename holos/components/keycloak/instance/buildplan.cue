package holos

// keycloak renders the Keycloak server instance reconciled by the operator
// (the sibling operator component): the Keycloak CR backed by the CNPG
// keycloak-db Postgres Cluster (components/cnpg-clusters), the cert-manager
// Certificate Keycloak terminates its own TLS with, the declarative holos
// realm import, the HTTPRoute attaching it to the shared Gateway
// (components/istio-gateway), and the DestinationRule that makes the
// Gateway re-encrypt to the HTTPS backend.  The version pin and the
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
// (holos/namespaces.cue) with _ambient: false — see the registry entry for
// the rationale — never emitted by components.

let NAME = "keycloak"
let NAMESPACE = KeycloakNamespace

// The operator names the Service it creates "<cr-name>-service"; with CR
// name "keycloak" that is "keycloak-service".  The http.serviceName field
// could override it, but following the operator default keeps the CR
// minimal and matches the reference platform.
let SERVICE = "\(NAME)-service"

// auth.holos.localhost matches the shared Gateway's *.holos.localhost
// listener hostname and resolves to 127.0.0.1 on the host per
// docs/local-cluster.md.
let HOSTNAME = "auth.holos.localhost"

let HTTPS_PORT = 8443
let TLS_SECRET = "\(NAME)-tls"

// The shared Gateway's namespace (components/istio-gateway).  The
// DestinationRule below lives there so the gateway proxy applies it on the
// Gateway→Keycloak hop.
let GATEWAY_NAMESPACE = "istio-gateways"

// Keycloak terminates its own TLS (http.tlsSecret below), so the
// certificate covers the operator-created Service DNS names — the names
// in-cluster backchannel consumers (Quay later) and the Gateway connect to
// — plus the public hostname.  Issued by the local-ca ClusterIssuer
// (components/local-ca) backed by the mkcert root CA the host trusts.
let CERTIFICATE = {
	apiVersion: "cert-manager.io/v1"
	kind:       "Certificate"
	metadata: {
		name:      TLS_SECRET
		namespace: NAMESPACE
	}
	spec: {
		secretName: TLS_SECRET
		dnsNames: [
			"\(SERVICE).\(NAMESPACE).svc.cluster.local",
			"\(SERVICE).\(NAMESPACE).svc",
			SERVICE,
			HOSTNAME,
		]
		issuerRef: {
			group: "cert-manager.io"
			kind:  "ClusterIssuer"
			name:  "local-ca"
		}
	}
}

// The Keycloak CR uses v2beta1, the storage version of the pinned
// KeycloakVersion CRDs — see the #Resources comment in holos/resources.cue.
// With a real database and a TLS secret configured the operator runs the
// server in start (production) mode, never start-dev; verify the rendered
// StatefulSet args on the live cluster after changing this CR.
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

		// Keycloak terminates its own TLS with the cert-manager Certificate
		// above; the shared Gateway re-encrypts to this port via the
		// DestinationRule below.
		http: {
			httpsPort: HTTPS_PORT
			tlsSecret: TLS_SECRET
		}

		// Browsers use the public hostname through the Gateway; strict:
		// false plus backchannelDynamic lets in-cluster consumers (Quay
		// later) reach Keycloak via the Service DNS names over the
		// backchannel.  See https://www.keycloak.org/server/hostname.
		hostname: {
			hostname:           "https://\(HOSTNAME)"
			strict:             false
			backchannelDynamic: true
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

		// Explicitly opt the pod out of Istio ambient mode, mirroring the
		// reference platform's defense in depth: the keycloak namespace
		// already carries _ambient: false (holos/namespaces.cue), and the
		// pod label guards against an accidental namespace-label change
		// re-enrolling Keycloak.
		unsupported: podTemplate: metadata: labels: "istio.io/dataplane-mode": "none"
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
			// OIDC client placeholder for Quay: disabled, with no secret
			// committed.  The Quay issue enables it and provisions real
			// credentials.
			clients: [{
				clientId: "quay"
				name:     "Quay (placeholder — wired in the Quay issue)"
				enabled:  false
				protocol: "openid-connect"
				redirectUris: ["https://quay.holos.localhost/*"]
			}]
		}
	}
}

// Cross-namespace attachment to the shared Gateway is allowed because its
// listener sets allowedRoutes.namespaces.from: All (istio-gateway
// component).
let HTTPROUTE = {
	apiVersion: "gateway.networking.k8s.io/v1"
	kind:       "HTTPRoute"
	metadata: {
		name:      NAME
		namespace: NAMESPACE
	}
	spec: {
		parentRefs: [{
			name:      "default"
			namespace: GATEWAY_NAMESPACE
		}]
		hostnames: [HOSTNAME]
		rules: [{
			matches: [{path: {type: "PathPrefix", value: "/"}}]
			backendRefs: [{
				name: SERVICE
				port: HTTPS_PORT
			}]
		}]
	}
}

// The backend serves HTTPS on 8443 but the Gateway API standard channel
// bundle this repo ships (components/gateway-api) has no BackendTLSPolicy
// CRD, so an Istio DestinationRule makes the gateway proxy originate TLS
// on the Gateway→Keycloak hop, as the reference platform does.  It lives
// in the Gateway's namespace and is exported only there ("."): only the
// gateway proxy needs it, and scoping it prevents surprising other mesh
// clients of the Service.
//
// insecureSkipVerify: the gateway proxy cannot verify the backend
// certificate against the local CA without the CA bundle in a Secret in
// istio-gateways readable via SDS, and the mkcert root is host-local —
// staged into the cert-manager namespace only, by scripts/local-ca, never
// committed — so there is no declarative way to place it here today.  The
// reference platform accepts the same trade-off in production; for the
// local MVP the hop stays encrypted but unverified.  Tighten by verifying
// against the CA (credentialName) when a CA-distribution mechanism (e.g.
// trust-manager) or the experimental-channel BackendTLSPolicy lands.
let DESTINATION_RULE = {
	apiVersion: "networking.istio.io/v1"
	kind:       "DestinationRule"
	metadata: {
		name:      SERVICE
		namespace: GATEWAY_NAMESPACE
	}
	spec: {
		host: "\(SERVICE).\(NAMESPACE).svc.cluster.local"
		exportTo: ["."]
		trafficPolicy: tls: {
			mode:               "SIMPLE"
			insecureSkipVerify: true
		}
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
				// Keycloak, cert-manager, Gateway API, and Istio schemas at
				// render time.
				resources: #Resources & {
					Certificate: (CERTIFICATE.metadata.name):          CERTIFICATE
					Keycloak: (KEYCLOAK.metadata.name):                KEYCLOAK
					KeycloakRealmImport: (REALM_IMPORT.metadata.name): REALM_IMPORT
					HTTPRoute: (HTTPROUTE.metadata.name):              HTTPROUTE
					DestinationRule: (DESTINATION_RULE.metadata.name): DESTINATION_RULE
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

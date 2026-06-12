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

		// Keycloak terminates its own TLS with the cert-manager Certificate
		// above; the shared Gateway re-encrypts to this port via the
		// DestinationRule below.
		http: {
			httpsPort: HTTPS_PORT
			tlsSecret: TLS_SECRET
		}

		// Browsers use the public hostname through the Gateway, and
		// backchannelDynamic lets in-cluster consumers (Quay later) reach
		// Keycloak via the Service DNS names over the backchannel.
		// hostname-strict is ignored once a full hostname is configured;
		// strict: false is kept explicit so the intended posture survives
		// if the hostname field is ever removed or made relative.  See
		// https://www.keycloak.org/server/hostname.
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
				port: HTTPS_PORT
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

// The backend serves HTTPS on 8443 but the Gateway API standard channel
// bundle this repo ships (components/gateway-api) has no BackendTLSPolicy
// CRD, so an Istio DestinationRule makes the gateway proxy originate TLS
// on the Gateway→Keycloak hop, as the reference platform does.  It lives
// in the Gateway's namespace and is exported only there ("."): only the
// gateway proxy needs it, and scoping it prevents surprising other mesh
// clients of the Service.
//
// Unlike the reference platform (which sets insecureSkipVerify), the
// gateway verifies the backend certificate against the local CA:
// credentialName references the wildcard-holos-localhost Secret the
// istio-gateway component's Certificate writes in this same namespace —
// cert-manager CA issuers include the signing CA in the issued Secret's
// ca.crt key, and Istio's SDS reads ca.crt from a TLS-type Secret as the
// trust anchor for SIMPLE-mode origination (the Secret must live in the
// gateway proxy's namespace, which is why the wildcard Secret is usable
// and the keycloak-tls Secret is not).  subjectAltNames pins the expected
// backend identity to the Service FQDN, present in the keycloak-tls
// certificate's dnsNames; sni makes the gateway present that same name so
// verification stays deterministic.  Replace with the standard-channel
// BackendTLSPolicy when the pinned Gateway API version ships it.
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
			mode:           "SIMPLE"
			credentialName: "wildcard-holos-localhost"
			sni:            "\(SERVICE).\(NAMESPACE).svc.cluster.local"
			subjectAltNames: ["\(SERVICE).\(NAMESPACE).svc.cluster.local"]
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
					HTTPRoute: {
						(HTTPROUTE.metadata.name):          HTTPROUTE
						(HTTPROUTE_REDIRECT.metadata.name): HTTPROUTE_REDIRECT
					}
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

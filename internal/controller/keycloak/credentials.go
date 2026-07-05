package keycloak

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	ctrlshared "github.com/holos-run/holos-paas/internal/controller/shared"
)

// Credential Secret keys. The Keycloak admin credential Secret carries the
// OAuth2 client_credentials grant material the internal/keycloak client
// authenticates the Admin API with: a confidential service-account client's
// clientId and secret in the instance's realm (ADR-20, "Admin credential"). The
// optional tokenUrl overrides the derived token endpoint for an out-of-cluster
// target whose token path differs from the conventional derivation.
//
// The KeycloakInstance spec already carries the url and realm, so unlike quay's
// credential (which holds url + token) this Secret holds only the auth material;
// the reconciler combines it with the instance's url/realm/caBundle when building
// the client.
const (
	// credentialKeyClientID is the Secret key holding the confidential client's
	// clientId.
	credentialKeyClientID = "clientId"
	// credentialKeyClientSecret is the Secret key holding that client's secret.
	credentialKeyClientSecret = "clientSecret"
	// credentialKeyTokenURL is the optional Secret key holding an explicit OAuth2
	// token endpoint, overriding the conventional
	// {url}/realms/{realm}/protocol/openid-connect/token derivation. Informational
	// for an in-cluster target; useful for a remote target with a non-standard
	// token path.
	credentialKeyTokenURL = "tokenUrl"
)

// DefaultControllerNamespace is the namespace the controller resolves credential
// Secrets from when POD_NAMESPACE is unset (ADR-18). It matches the namespace the
// kustomize deployment installs into and mirrors the quay reconciler's default.
const DefaultControllerNamespace = ctrlshared.DefaultControllerNamespace

// keycloakCredential is the resolved Keycloak admin credential: the OAuth2
// client_credentials grant material the internal/keycloak client is built from.
type keycloakCredential struct {
	// clientID is the confidential client's clientId (the Secret's clientId key).
	clientID string
	// clientSecret is that client's secret (the Secret's clientSecret key).
	clientSecret string
	// tokenURL is the optional explicit token endpoint (the Secret's tokenUrl
	// key); empty means derive it from the instance url + realm.
	tokenURL string
}

// missingCredentialError reports that the credential Secret, or a required key
// within it, could not be resolved. The reconciler maps it to a False condition
// with reason CredentialsNotFound and requeues rather than crashing — the
// credential is created at runtime and may not yet exist.
// isMissingCredential reports whether err is a missingCredentialError.
func isMissingCredential(err error) bool {
	return ctrlshared.IsMissingCredential(err)
}

// controllerNamespace returns the namespace the controller resolves credential
// Secrets from: the POD_NAMESPACE env (set via the downward API in the
// deployment) when present, otherwise DefaultControllerNamespace. The credential
// Secret always lives in the controller's own namespace, never the resource's
// namespace — the KeycloakInstance's credentialsSecretRef names only the Secret,
// not a namespace (ADR-20).
func controllerNamespace() string {
	return ctrlshared.ControllerNamespace()
}

// resolveCredential reads the Keycloak admin credential Secret named by ref
// (defaulting to holos-controller-keycloak-creds) from the controller's namespace
// and returns the clientId + clientSecret (+ optional tokenUrl) it carries.
//
// reader is the manager's APIReader (a non-caching reader), used so the
// controller can read the credential Secret without a cluster-wide Secret
// informer cache — the controller holds only get on Secrets, not list/watch (see
// the RBAC markers). namespace is the controller's own namespace.
//
// A missing Secret, or a missing/empty clientId or clientSecret key, returns a
// *missingCredentialError so the reconciler sets a False condition and requeues
// rather than crashing.
func resolveCredential(ctx context.Context, reader client.Reader, namespace string, ref keycloakv1alpha1.SecretReference) (*keycloakCredential, error) {
	name := ref.Name
	if name == "" {
		name = keycloakv1alpha1.DefaultCredentialsSecretName
	}

	secret, err := ctrlshared.ResolveCredentialSecret(ctx, reader, namespace, name)
	if err != nil {
		return nil, err
	}

	// The client-secret key is the conventional "clientSecret" unless the ref
	// narrows it with an explicit Key — the SecretReference.Key field selects which
	// Secret entry holds the client secret. Honoring it keeps the documented CRD
	// field functional rather than silently ignored. clientId and tokenUrl always
	// use their conventional keys.
	secretKey := credentialKeyClientSecret
	if ref.Key != "" {
		secretKey = ref.Key
	}

	clientID, err := ctrlshared.RequiredSecretValue(secret, namespace, name, credentialKeyClientID)
	if err != nil {
		return nil, err
	}
	clientSecret, err := ctrlshared.RequiredSecretValue(secret, namespace, name, secretKey)
	if err != nil {
		return nil, err
	}

	return &keycloakCredential{
		clientID:     clientID,
		clientSecret: clientSecret,
		tokenURL:     string(secret.Data[credentialKeyTokenURL]),
	}, nil
}

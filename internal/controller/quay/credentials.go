package quay

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	ctrlshared "github.com/holos-run/holos-paas/internal/controller/shared"
)

// Credential Secret keys. The Quay superuser OAuth-Application credential Secret
// carries the API URL and Bearer token, plus an optional username for diagnostic
// logging. The reconciler authenticates to Quay solely through this credential
// (ADR-19, AC #7).
//
// This shape — Secret holos-controller-quay-creds in the holos-controller
// namespace, keys url + token (+ optional username) — is the contract this
// reconciler's phase (HOL-1311) mandates and the deployment provisions (HOL-1313).
// It supersedes the older single-key quay/quay-resource-controller token Secret
// the credentials runbook documents for manual API calls; reconciling that
// runbook to this controller contract is HOL-1314's scope. The token may also be
// pointed at a non-default key via SecretReference.Key (honored below).
const (
	// credentialKeyURL is the Secret key holding the Quay API URL.
	credentialKeyURL = "url"
	// credentialKeyToken is the Secret key holding the superuser OAuth token.
	credentialKeyToken = "token"
	// credentialKeyUsername is the optional Secret key holding the username the
	// token acts as (e.g. svc-quay-resource-controller). Informational only.
	credentialKeyUsername = "username"
)

// DefaultControllerNamespace is the namespace the controller resolves credential
// Secrets from when POD_NAMESPACE is unset (ADR-18). It matches the namespace the
// kustomize deployment installs into (HOL-1313).
const DefaultControllerNamespace = ctrlshared.DefaultControllerNamespace

// quayCredential is the resolved Quay API credential: the base URL and Bearer
// token the internal/quay client is built from, plus the optional username for
// logging.
type quayCredential struct {
	// url is the Quay API base URL (the Secret's url key).
	url string
	// token is the superuser OAuth-Application Bearer token (the Secret's token key).
	token string
	// username is the optional identity the token acts as; informational.
	username string
}

// missingCredentialError reports that the credential Secret, or a required key
// within it, could not be resolved. The reconciler maps it to a False condition
// with reason CredentialsNotFound and requeues rather than crashing (AC #3).
// isMissingCredential reports whether err is a missingCredentialError.
func isMissingCredential(err error) bool {
	return ctrlshared.IsMissingCredential(err)
}

// controllerNamespace returns the namespace the controller resolves credential
// Secrets from: the POD_NAMESPACE env (set via the downward API in the
// deployment, HOL-1313) when present, otherwise DefaultControllerNamespace. The
// credential Secret always lives in the controller's own namespace, never the
// resource's namespace — the resource's credentialsSecretRef names only the
// Secret, not a namespace (ADR-19).
func controllerNamespace() string {
	return ctrlshared.ControllerNamespace()
}

// resolveCredential reads the Quay credential Secret named by ref (defaulting to
// holos-controller-quay-creds) from the controller's namespace and returns the
// url + token (+ optional username) it carries.
//
// reader is the manager's APIReader (a non-caching reader), used so the
// controller can read the credential Secret without a cluster-wide Secret
// informer cache — the controller holds only get on Secrets, not list/watch
// (see the RBAC markers). namespace is the controller's own namespace.
//
// A missing Secret, or a missing/empty url or token key, returns a
// *missingCredentialError so the reconciler sets a False condition and requeues
// rather than crashing.
func resolveCredential(ctx context.Context, reader client.Reader, namespace string, ref *quayv1alpha1.SecretReference) (*quayCredential, error) {
	name := ""
	if ref != nil {
		name = ref.Name
	}
	if name == "" {
		name = quayv1alpha1.DefaultCredentialsSecretName
	}

	secret, err := ctrlshared.ResolveCredentialSecret(ctx, reader, namespace, name)
	if err != nil {
		return nil, err
	}

	// The token key is the conventional "token" unless the ref narrows it with
	// an explicit Key — the SecretReference.Key field selects which Secret entry
	// holds the token. Honoring it keeps the documented CRD field functional
	// rather than silently ignored. url and username always use their
	// conventional keys.
	tokenKey := credentialKeyToken
	if ref != nil && ref.Key != "" {
		tokenKey = ref.Key
	}

	url, err := ctrlshared.RequiredSecretValue(secret, namespace, name, credentialKeyURL)
	if err != nil {
		return nil, err
	}
	token, err := ctrlshared.RequiredSecretValue(secret, namespace, name, tokenKey)
	if err != nil {
		return nil, err
	}

	return &quayCredential{
		url:      url,
		token:    token,
		username: string(secret.Data[credentialKeyUsername]),
	}, nil
}

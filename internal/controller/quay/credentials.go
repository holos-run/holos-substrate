package quay

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
)

// Credential Secret keys. The Quay superuser OAuth-Application credential Secret
// carries the API URL and Bearer token, plus an optional username for diagnostic
// logging. The reconciler authenticates to Quay solely through this credential
// (ADR-19, AC #7).
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
const DefaultControllerNamespace = "holos-controller"

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
type missingCredentialError struct {
	// msg is the human-readable explanation surfaced on the status condition.
	msg string
}

func (e *missingCredentialError) Error() string { return e.msg }

// isMissingCredential reports whether err is a missingCredentialError.
func isMissingCredential(err error) bool {
	_, ok := err.(*missingCredentialError)
	return ok
}

// controllerNamespace returns the namespace the controller resolves credential
// Secrets from: the POD_NAMESPACE env (set via the downward API in the
// deployment, HOL-1313) when present, otherwise DefaultControllerNamespace. The
// credential Secret always lives in the controller's own namespace, never the
// resource's namespace — the resource's credentialsSecretRef names only the
// Secret, not a namespace (ADR-19).
func controllerNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultControllerNamespace
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
func resolveCredential(ctx context.Context, reader client.Reader, namespace string, ref quayv1alpha1.SecretReference) (*quayCredential, error) {
	name := ref.Name
	if name == "" {
		name = quayv1alpha1.DefaultCredentialsSecretName
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &missingCredentialError{
				msg: fmt.Sprintf("credential Secret %s/%s not found", namespace, name),
			}
		}
		// A transient API error (e.g. timeout) is not a missing-credential
		// condition — return it as-is so the reconciler requeues with backoff
		// and does not stamp a misleading CredentialsNotFound reason.
		return nil, fmt.Errorf("reading credential Secret %s/%s: %w", namespace, name, err)
	}

	url := string(secret.Data[credentialKeyURL])
	token := string(secret.Data[credentialKeyToken])
	if url == "" {
		return nil, &missingCredentialError{
			msg: fmt.Sprintf("credential Secret %s/%s is missing the %q key", namespace, name, credentialKeyURL),
		}
	}
	if token == "" {
		return nil, &missingCredentialError{
			msg: fmt.Sprintf("credential Secret %s/%s is missing the %q key", namespace, name, credentialKeyToken),
		}
	}

	return &quayCredential{
		url:      url,
		token:    token,
		username: string(secret.Data[credentialKeyUsername]),
	}, nil
}

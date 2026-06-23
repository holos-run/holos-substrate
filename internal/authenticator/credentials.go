package authenticator

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
)

// credentialKeyToken is the conventional Secret key holding the backend's
// privileged Kubernetes API server bearer token — the impersonator identity the
// authorizer injects as the upstream Authorization header. A Backend's
// spec.credentialsSecretRef.Key overrides it.
const credentialKeyToken = "token"

// DefaultAuthorizerNamespace is the namespace the authorizer resolves credential
// Secrets from when POD_NAMESPACE is unset. The credential Secret always lives in
// the authorizer's own namespace, never the Backend's namespace — the Backend's
// spec.credentialsSecretRef names only the Secret, not a namespace (ADR-23,
// mirroring ADR-19). This matches the namespace the authenticator deploys into
// (HOL-1389).
const DefaultAuthorizerNamespace = "holos-authenticator"

// missingCredentialError reports that the credential Secret, or the required key
// within it, could not be resolved. The Check path maps it to a fail-closed
// Denied response rather than crashing, and distinguishes it from a transient API
// error so logging can name the cause.
type missingCredentialError struct {
	// msg is the human-readable explanation logged on the denied request.
	msg string
}

func (e *missingCredentialError) Error() string { return e.msg }

// AuthorizerNamespace returns the namespace the authorizer resolves credential
// Secrets from: the POD_NAMESPACE env (set via the downward API in the
// deployment, HOL-1389) when present, otherwise DefaultAuthorizerNamespace. It is
// exported so main can pass it into NewCheckServer, keeping the resolution rule in
// one place alongside the Secret reader that consumes it.
func AuthorizerNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultAuthorizerNamespace
}

// resolveImpersonatorToken reads the backend's privileged credential Secret named
// by ref (defaulting to holos-authenticator-backend-creds) from the authorizer's
// own namespace and returns the bearer token it carries — the identity the
// authorizer impersonates as on the upstream API server.
//
// reader is the manager's APIReader (a non-caching reader), used so the Check
// path reads the credential Secret without a cluster-wide Secret informer cache
// — the authorizer holds only get on Secrets in its own namespace, not
// list/watch (HOL-1389 RBAC). namespace is the authorizer's own namespace.
//
// A missing Secret, or a missing/empty token key, returns a
// *missingCredentialError so the Check path denies fail-closed and names the
// cause; a transient API error is returned as-is so the cause is not misreported.
func resolveImpersonatorToken(ctx context.Context, reader client.Reader, namespace string, ref authenticatorv1alpha1.SecretReference) (string, error) {
	name := ref.Name
	if name == "" {
		name = authenticatorv1alpha1.DefaultCredentialsSecretName
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &missingCredentialError{
				msg: fmt.Sprintf("credential Secret %s/%s not found", namespace, name),
			}
		}
		// A transient API error (e.g. timeout) is not a missing-credential
		// condition — return it as-is so the cause is not misreported as a
		// missing Secret. The Check path denies fail-closed either way.
		return "", fmt.Errorf("reading credential Secret %s/%s: %w", namespace, name, err)
	}

	// The token key is the conventional "token" unless the ref narrows it with an
	// explicit Key — the SecretReference.Key field selects which Secret entry
	// holds the token, keeping the documented CRD field functional rather than
	// silently ignored.
	tokenKey := credentialKeyToken
	if ref.Key != "" {
		tokenKey = ref.Key
	}

	token := string(secret.Data[tokenKey])
	if token == "" {
		return "", &missingCredentialError{
			msg: fmt.Sprintf("credential Secret %s/%s is missing the %q key", namespace, name, tokenKey),
		}
	}
	return token, nil
}

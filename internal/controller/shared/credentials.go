package shared

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultControllerNamespace is the namespace the controller resolves runtime
// credential Secrets from when POD_NAMESPACE is unset.
const DefaultControllerNamespace = "holos-controller"

// MissingCredentialError reports that a runtime credential Secret, or a required
// key within it, is not present yet.
type MissingCredentialError struct {
	Message string
}

func (e *MissingCredentialError) Error() string { return e.Message }

func IsMissingCredential(err error) bool {
	var missing *MissingCredentialError
	return As(err, &missing)
}

// ControllerNamespace returns the namespace the controller resolves credential
// Secrets from.
func ControllerNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultControllerNamespace
}

// ResolveCredentialSecret reads a runtime credential Secret from the controller
// namespace and reports absent Secrets as MissingCredentialError so reconcilers
// can distinguish expected bootstrap waits from transient API failures.
func ResolveCredentialSecret(ctx context.Context, reader client.Reader, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &MissingCredentialError{
				Message: fmt.Sprintf("credential Secret %s/%s not found", namespace, name),
			}
		}
		return nil, fmt.Errorf("reading credential Secret %s/%s: %w", namespace, name, err)
	}
	return secret, nil
}

func RequiredSecretValue(secret *corev1.Secret, namespace, name, key string) (string, error) {
	value := string(secret.Data[key])
	if value == "" {
		return "", &MissingCredentialError{
			Message: fmt.Sprintf("credential Secret %s/%s is missing the %q key", namespace, name, key),
		}
	}
	return value, nil
}

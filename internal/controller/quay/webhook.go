package quay

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	quayv1alpha1 "github.com/holos-run/holos-substrate/api/quay/v1alpha1"
)

const maxWebhookURLLength = 2048

// webhookURLNotFoundError reports that the webhook urlSecretRef Secret, or the
// key within it, could not be resolved into a non-empty URL. It is a recoverable
// condition: the reconciler maps it to a False WebhookConfigured condition with
// reason WebhookURLNotFound and requeues, so a later-created Secret takes effect
// rather than failing permanently.
type webhookURLNotFoundError struct {
	// msg is the human-readable explanation surfaced on the status condition.
	msg string
}

func (e *webhookURLNotFoundError) Error() string { return e.msg }

// isWebhookURLNotFound reports whether err is a webhookURLNotFoundError.
func isWebhookURLNotFound(err error) bool {
	var notFound *webhookURLNotFoundError
	return errors.As(err, &notFound)
}

// invalidWebhookError reports that spec.webhook violated the mutual-exclusion
// rule at runtime — neither or both of url/urlSecretRef were set. The CRD's
// XValidation should reject this at admission, so this is a defense-in-depth
// guard; the reconciler maps it to a False condition with reason InvalidWebhook
// and does not requeue (a spec change re-triggers).
type invalidWebhookError struct {
	// msg is the human-readable explanation surfaced on the status condition.
	msg string
}

func (e *invalidWebhookError) Error() string { return e.msg }

// isInvalidWebhook reports whether err is an invalidWebhookError.
func isInvalidWebhook(err error) bool {
	var invalid *invalidWebhookError
	return errors.As(err, &invalid)
}

// resolveWebhookURL resolves a Repository's repo_push webhook target URL from
// spec.webhook. The URL comes from exactly one of two sources:
//
//   - webhook.url — the inline URL, returned verbatim.
//   - webhook.urlSecretRef — read Secret[name].Data[key] in the Repository's own
//     namespace, where Kargo's hard-to-guess receiver URL lives. This is distinct
//     from spec.credentialsSecretRef (the Quay API credential, resolved in the
//     controller's namespace) and must never be conflated with it.
//
// reader is the manager's APIReader (non-caching) so the controller reads the
// referenced Secret without a cluster-wide Secret cache. namespace is the
// Repository's own namespace.
//
// Errors are typed so the reconciler can branch:
//   - *invalidWebhookError when neither or both sources are set (a runtime guard
//     behind the CRD XValidation).
//   - *webhookURLNotFoundError when the urlSecretRef Secret, or its key, is
//     missing or empty (recoverable — the reconciler requeues).
//
// A transient API error reading the Secret is returned as-is so the reconciler
// requeues with backoff without stamping a misleading reason.
func resolveWebhookURL(ctx context.Context, reader client.Reader, namespace string, webhook *quayv1alpha1.RepositoryWebhook) (string, error) {
	hasInline := webhook.URL != nil && *webhook.URL != ""
	hasRef := webhook.URLSecretRef != nil

	switch {
	case hasInline && hasRef:
		return "", &invalidWebhookError{
			msg: "spec.webhook sets both url and urlSecretRef; exactly one must be set",
		}
	case !hasInline && !hasRef:
		return "", &invalidWebhookError{
			msg: "spec.webhook sets neither url nor urlSecretRef; exactly one must be set",
		}
	case hasInline:
		return *webhook.URL, nil
	}

	// urlSecretRef path: read the named key from the Secret in the Repository's
	// namespace.
	ref := webhook.URLSecretRef
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &webhookURLNotFoundError{
				msg: fmt.Sprintf("webhook URL Secret %s/%s not found", namespace, ref.Name),
			}
		}
		return "", fmt.Errorf("reading webhook URL Secret %s/%s: %w", namespace, ref.Name, err)
	}

	url := string(secret.Data[ref.Key])
	if url == "" {
		return "", &webhookURLNotFoundError{
			msg: fmt.Sprintf("webhook URL Secret %s/%s is missing the %q key", namespace, ref.Name, ref.Key),
		}
	}
	if len(url) > maxWebhookURLLength {
		return "", &webhookURLNotFoundError{
			msg: fmt.Sprintf("webhook URL Secret %s/%s key %q exceeds %d bytes", namespace, ref.Name, ref.Key, maxWebhookURLLength),
		}
	}
	return url, nil
}

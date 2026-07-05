package quay

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
)

// TestResolveWebhookURL is a fast unit test for the inline-vs-secretRef webhook
// resolution (AC #8). It uses the controller-runtime fake client as the reader,
// so it runs without envtest — unlike the reconciler tests, it does not need a
// real API server.
func TestResolveWebhookURL(t *testing.T) {
	const ns = "tenant"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kargo-receiver"},
		Data:       map[string][]byte{"url": []byte("https://kargo.example.test/webhook/zzz")},
	}
	emptyKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "empty-key"},
		Data:       map[string][]byte{"url": []byte("")},
	}
	reader := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(secret, emptyKeySecret).
		Build()

	inline := "https://inline.example.test/webhook/abc"

	tests := []struct {
		name    string
		webhook *quayv1alpha1.RepositoryWebhook
		wantURL string
		wantErr func(error) bool
	}{
		{
			name:    "inline url",
			webhook: &quayv1alpha1.RepositoryWebhook{URL: &inline},
			wantURL: inline,
		},
		{
			name: "secretRef resolves",
			webhook: &quayv1alpha1.RepositoryWebhook{
				URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
			},
			wantURL: "https://kargo.example.test/webhook/zzz",
		},
		{
			name: "secretRef missing secret",
			webhook: &quayv1alpha1.RepositoryWebhook{
				URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "absent", Key: "url"},
			},
			wantErr: isWebhookURLNotFound,
		},
		{
			name: "secretRef missing key",
			webhook: &quayv1alpha1.RepositoryWebhook{
				URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "absent"},
			},
			wantErr: isWebhookURLNotFound,
		},
		{
			name: "secretRef empty value",
			webhook: &quayv1alpha1.RepositoryWebhook{
				URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "empty-key", Key: "url"},
			},
			wantErr: isWebhookURLNotFound,
		},
		{
			name: "both set is invalid",
			webhook: &quayv1alpha1.RepositoryWebhook{
				URL:          &inline,
				URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
			},
			wantErr: isInvalidWebhook,
		},
		{
			name:    "neither set is invalid",
			webhook: &quayv1alpha1.RepositoryWebhook{},
			wantErr: isInvalidWebhook,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, err := resolveWebhookURL(context.Background(), reader, ns, tc.webhook)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("expected an error, got url %q", url)
				}
				if !tc.wantErr(err) {
					t.Fatalf("error %v did not match the expected classification", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if url != tc.wantURL {
				t.Errorf("url = %q, want %q", url, tc.wantURL)
			}
		})
	}
}

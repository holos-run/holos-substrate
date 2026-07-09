package authenticator

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	authenticatorv1alpha1 "github.com/holos-run/holos-substrate/api/authenticator/v1alpha1"
)

// countingSubResourceClient wraps a client.Client and counts Create calls on the
// "token" sub-resource, so a test can assert the TokenManager minted (or did not
// mint) without depending on the opaque JWT bytes — two mints for the same SA at
// the same instant could otherwise be byte-identical. All other methods delegate.
type countingSubResourceClient struct {
	client.Client
	mu        sync.Mutex
	tokenMint int
}

func (c *countingSubResourceClient) SubResource(subResource string) client.SubResourceClient {
	inner := c.Client.SubResource(subResource)
	if subResource != "token" {
		return inner
	}
	return &countingSubResourceWriter{SubResourceClient: inner, parent: c}
}

func (c *countingSubResourceClient) mintCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenMint
}

type countingSubResourceWriter struct {
	client.SubResourceClient
	parent *countingSubResourceClient
}

func (w *countingSubResourceWriter) Create(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption) error {
	w.parent.mu.Lock()
	w.parent.tokenMint++
	w.parent.mu.Unlock()
	return w.SubResourceClient.Create(ctx, obj, sub, opts...)
}

// requireEnvtest skips the test cleanly when the envtest control plane was not
// provisioned (KUBEBUILDER_ASSETS unset), so `go test ./...` stays green and the
// minting tests run only under `make authenticator-test`.
func requireEnvtest(t *testing.T) *sharedEnv {
	t.Helper()
	if envtestShared == nil {
		t.Skip("envtest control plane not provisioned (KUBEBUILDER_ASSETS unset); run via `make authenticator-test`")
	}
	return envtestShared
}

// createServiceAccount creates a ServiceAccount of the given name in the test
// namespace (created first if absent) and registers cleanup.
func createServiceAccount(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %q: %v", namespace, err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := c.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ServiceAccount %s/%s: %v", namespace, name, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), sa) })
}

// withFixedNow swaps the package time source to return a fixed instant and
// restores it on cleanup, so rotation can be driven deterministically.
func withFixedNow(t *testing.T, fixed time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = prev })
}

// TestTokenManagerMintsRealToken asserts the TokenManager mints a non-empty bearer
// token with a future expiry for a real ServiceAccount via the envtest API server's
// TokenRequest endpoint.
func TestTokenManagerMintsRealToken(t *testing.T) {
	env := requireEnvtest(t)
	const namespace = "holos-authenticator-tm-mint"
	const saName = "impersonator"
	createServiceAccount(t, env.k8sClient, namespace, saName)

	tm := NewTokenManager(env.k8sClient, namespace)
	tok, err := tm.Token(context.Background(), saName, "", DefaultTokenExpirationSeconds)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok == "" {
		t.Fatal("minted token is empty")
	}

	// The cache should hold one entry with a future expiry.
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if got := len(tm.cache); got != 1 {
		t.Fatalf("cache size = %d, want 1", got)
	}
	for _, cached := range tm.cache {
		if cached.token != tok {
			t.Errorf("cached token != returned token")
		}
		if !cached.expiresAt.After(time.Now()) {
			t.Errorf("cached expiresAt %v is not in the future", cached.expiresAt)
		}
	}
}

// TestTokenManagerCachesWithinWindow asserts a second Token call within the
// rotation window returns the cached token without a second TokenRequest mint.
func TestTokenManagerCachesWithinWindow(t *testing.T) {
	env := requireEnvtest(t)
	const namespace = "holos-authenticator-tm-cache"
	const saName = "impersonator"
	createServiceAccount(t, env.k8sClient, namespace, saName)

	counter := &countingSubResourceClient{Client: env.k8sClient}
	tm := NewTokenManager(counter, namespace)

	// Pin time well before expiry so the first mint's token stays outside the
	// rotation window for the second call.
	withFixedNow(t, time.Now())

	first, err := tm.Token(context.Background(), saName, "", DefaultTokenExpirationSeconds)
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	second, err := tm.Token(context.Background(), saName, "", DefaultTokenExpirationSeconds)
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}

	if first != second {
		t.Error("second Token returned a different token; expected the cached one")
	}
	if got := counter.mintCount(); got != 1 {
		t.Errorf("mint count = %d, want 1 (second call should hit the cache)", got)
	}
}

// TestTokenManagerReMintsPastMargin asserts a Token call once the cached token is
// within the rotation margin of expiry mints a fresh token (a second
// TokenRequest), and that distinct (name, audience, expiration) tuples are cached
// independently.
func TestTokenManagerReMintsPastMargin(t *testing.T) {
	env := requireEnvtest(t)
	const namespace = "holos-authenticator-tm-remint"
	const saName = "impersonator"
	createServiceAccount(t, env.k8sClient, namespace, saName)

	counter := &countingSubResourceClient{Client: env.k8sClient}
	tm := NewTokenManager(counter, namespace)

	base := time.Now()
	withFixedNow(t, base)
	if _, err := tm.Token(context.Background(), saName, "", DefaultTokenExpirationSeconds); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if got := counter.mintCount(); got != 1 {
		t.Fatalf("mint count after first call = %d, want 1", got)
	}

	// Advance the clock to within the rotation margin of the (1h) token's expiry.
	// The cached entry must be considered stale and re-minted.
	withFixedNow(t, base.Add(time.Duration(DefaultTokenExpirationSeconds)*time.Second).Add(-tokenRotationMargin+time.Second))
	if _, err := tm.Token(context.Background(), saName, "", DefaultTokenExpirationSeconds); err != nil {
		t.Fatalf("re-mint Token: %v", err)
	}
	if got := counter.mintCount(); got != 2 {
		t.Errorf("mint count after re-mint = %d, want 2", got)
	}

	// A different expirationSeconds is a distinct cache key — it mints separately.
	if _, err := tm.Token(context.Background(), saName, "", 1800); err != nil {
		t.Fatalf("distinct-key Token: %v", err)
	}
	if got := counter.mintCount(); got != 3 {
		t.Errorf("mint count after distinct key = %d, want 3", got)
	}
	if got := len(tm.cache); got != 2 {
		t.Errorf("cache size = %d, want 2 distinct keys", got)
	}
}

// TestTokenManagerDefaultsName asserts an empty ServiceAccount name defaults to the
// shipped impersonator SA name (so a Backend omitting spec.serviceAccountRef.name
// still mints against the default SA).
func TestTokenManagerDefaultsName(t *testing.T) {
	env := requireEnvtest(t)
	const namespace = "holos-authenticator-tm-default"
	createServiceAccount(t, env.k8sClient, namespace, authenticatorv1alpha1.DefaultImpersonatorServiceAccountName)

	tm := NewTokenManager(env.k8sClient, namespace)
	if _, err := tm.Token(context.Background(), "", "", DefaultTokenExpirationSeconds); err != nil {
		t.Fatalf("Token with empty name: %v", err)
	}
}

// TestTokenManagerMintErrorPropagates asserts a mint against a nonexistent
// ServiceAccount returns an error (which the Check path maps to a fail-closed
// Denied response).
func TestTokenManagerMintErrorPropagates(t *testing.T) {
	env := requireEnvtest(t)
	const namespace = "holos-authenticator-tm-err"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := env.k8sClient.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	tm := NewTokenManager(env.k8sClient, namespace)
	if _, err := tm.Token(context.Background(), "does-not-exist", "", DefaultTokenExpirationSeconds); err == nil {
		t.Fatal("expected an error minting for a nonexistent ServiceAccount, got nil")
	}
}

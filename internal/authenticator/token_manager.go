package authenticator

import (
	"context"
	"fmt"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	authenticatorv1alpha1 "github.com/holos-run/holos-substrate/api/authenticator/v1alpha1"
)

// DefaultTokenExpirationSeconds is the requested lifetime of a minted
// ServiceAccount token when a Backend's spec.serviceAccountRef.expirationSeconds
// is not populated (it matches the CRD default). The kube-apiserver may clamp the
// effective expiry to its own configured bounds; the cache keys off the requested
// value but rotates against the actual status.expirationTimestamp the API server
// returns.
const DefaultTokenExpirationSeconds int64 = 3600

// tokenRotationMargin is how long before a minted token's
// status.expirationTimestamp the TokenManager re-mints it. Refreshing ahead of
// the actual expiry keeps a valid token in hand for the Check path even under
// clock skew or a slow mint, and bounds the window in which an in-flight request
// could carry a token that expires before the upstream API server validates it.
// Five minutes comfortably covers both for the 3600s default and the 600s
// minimum (for which the effective refresh point is governed by
// tokenRotationFraction below).
const tokenRotationMargin = 5 * time.Minute

// tokenRotationFraction caps the rotation margin at a fraction of the token's
// total lifetime so a short-lived token is not refreshed almost immediately. For
// the 600s minimum lifetime the fixed 5-minute margin would otherwise demand a
// re-mint when more than half the lifetime remains; clamping the margin to 20% of
// the lifetime keeps the cache useful for short tokens while still rotating well
// ahead of expiry.
const tokenRotationFraction = 0.2

// now is the time source, overridable in tests to drive rotation deterministically.
// It defaults to time.Now.
var now = time.Now

// cachedToken is a minted token and the metadata the TokenManager rotates it by.
type cachedToken struct {
	// token is the bearer token string the Check path forwards as the upstream
	// Authorization credential.
	token string
	// expiresAt is the API server's reported status.expirationTimestamp for the
	// minted token — the actual expiry, which the API server may have clamped below
	// the requested expirationSeconds.
	expiresAt time.Time
	// lifetime is expiresAt minus the mint time, used to clamp the rotation margin
	// to a fraction of the token's actual lifetime (tokenRotationFraction).
	lifetime time.Duration
}

// needsRefresh reports whether the cached token is within the rotation window of
// its expiry (or already expired) and must be re-minted before use. The window is
// the smaller of tokenRotationMargin and tokenRotationFraction of the token's
// lifetime, so a short-lived token is not refreshed prematurely.
func (c cachedToken) needsRefresh(t time.Time) bool {
	margin := tokenRotationMargin
	if frac := time.Duration(float64(c.lifetime) * tokenRotationFraction); frac < margin {
		margin = frac
	}
	return !t.Before(c.expiresAt.Add(-margin))
}

// tokenKey identifies a cached token by the inputs that determine its value: the
// ServiceAccount name, the requested audience, and the requested expiration. Two
// Backends naming the same SA with the same audience and expiration share one
// cache entry (and one minted token); differing in any field mints separately.
type tokenKey struct {
	name              string
	audience          string
	expirationSeconds int64
}

// TokenManager mints, caches, and rotates ServiceAccount bearer tokens via the
// Kubernetes TokenRequest API for the Check path's serviceAccountRef credential
// source. It is the ServiceAccount analogue of resolveImpersonatorToken's Secret
// path: where that reads a long-lived credential Secret, this mints a short-lived
// token for a ServiceAccount in the authorizer's own namespace and re-mints it
// before expiry.
//
// TokenRequest is a create (write) sub-resource, so the TokenManager holds the
// manager's writable client (not the non-caching APIReader the Secret path uses).
// It is minted WITHOUT a BoundObjectRef — matching `kubectl create token` — so the
// authorizer's RBAC need only `create` on serviceaccounts/token, never `get` on
// serviceaccounts. When the requested audience is empty the TokenRequest omits
// spec.audiences entirely, so the kube-apiserver mints a token bound to its own
// default audience — correct for impersonating against the local API server
// (spec.server.url: https://kubernetes.default.svc).
//
// All exported methods are safe for concurrent use: the ext_authz Check path runs
// each request on its own goroutine and several may resolve the same SA credential
// at once.
type TokenManager struct {
	// client is the manager's writable client. TokenRequest minting is a create on
	// the serviceaccounts/token sub-resource, which the non-caching APIReader cannot
	// perform.
	client client.Client
	// namespace is the authorizer's own namespace, where the impersonator
	// ServiceAccount lives. Like the credential Secret, the ServiceAccount is always
	// resolved here, never the Backend's namespace.
	namespace string

	mu    sync.Mutex
	cache map[tokenKey]cachedToken
}

// NewTokenManager returns a TokenManager that mints tokens for ServiceAccounts in
// namespace using c (the manager's writable client). namespace is the authorizer's
// own namespace.
func NewTokenManager(c client.Client, namespace string) *TokenManager {
	return &TokenManager{
		client:    c,
		namespace: namespace,
		cache:     make(map[tokenKey]cachedToken),
	}
}

// Token returns a valid bearer token for the named ServiceAccount, minting one via
// the TokenRequest API on the first call and on every subsequent call once the
// cached token is within the rotation window of its expiry. A cached token still
// outside that window is returned without contacting the API server, so steady
// state mints at most once per rotation interval per (name, audience,
// expirationSeconds) tuple.
//
// name is the ServiceAccount name in the authorizer's namespace; an empty name
// defaults to DefaultImpersonatorServiceAccountName. audience is the requested
// token audience; empty omits spec.audiences so the API server uses its own
// default audience. expirationSeconds is the requested lifetime; a non-positive
// value defaults to DefaultTokenExpirationSeconds.
//
// A mint failure is returned to the caller, which the Check path maps to a
// fail-closed Denied response exactly as a missing credential Secret is — the
// authorizer never serves a request it cannot mint a credential for.
func (m *TokenManager) Token(ctx context.Context, name, audience string, expirationSeconds int64) (string, error) {
	if name == "" {
		name = authenticatorv1alpha1.DefaultImpersonatorServiceAccountName
	}
	if expirationSeconds <= 0 {
		expirationSeconds = DefaultTokenExpirationSeconds
	}
	key := tokenKey{name: name, audience: audience, expirationSeconds: expirationSeconds}

	m.mu.Lock()
	defer m.mu.Unlock()

	if cached, ok := m.cache[key]; ok && !cached.needsRefresh(now()) {
		return cached.token, nil
	}

	minted, err := m.mint(ctx, key)
	if err != nil {
		return "", err
	}
	m.cache[key] = minted
	return minted.token, nil
}

// mint performs the TokenRequest create for key and returns the resulting cached
// token. It populates spec.audiences only when key.audience is non-empty and never
// sets spec.boundObjectRef, keeping the required RBAC to create-only on
// serviceaccounts/token (no get on serviceaccounts), matching `kubectl create
// token`.
func (m *TokenManager) mint(ctx context.Context, key tokenKey) (cachedToken, error) {
	expirationSeconds := key.expirationSeconds
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
		},
	}
	// Omit Audiences when no audience is requested so the API server mints a token
	// bound to its own default audience (the common case for the local API server).
	if key.audience != "" {
		tr.Spec.Audiences = []string{key.audience}
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.name,
			Namespace: m.namespace,
		},
	}

	mintedAt := now()
	if err := m.client.SubResource("token").Create(ctx, sa, tr); err != nil {
		return cachedToken{}, fmt.Errorf("minting token for ServiceAccount %s/%s: %w", m.namespace, key.name, err)
	}
	if tr.Status.Token == "" {
		return cachedToken{}, fmt.Errorf("TokenRequest for ServiceAccount %s/%s returned an empty token", m.namespace, key.name)
	}

	expiresAt := tr.Status.ExpirationTimestamp.Time
	lifetime := expiresAt.Sub(mintedAt)
	return cachedToken{
		token:     tr.Status.Token,
		expiresAt: expiresAt,
		lifetime:  lifetime,
	}, nil
}

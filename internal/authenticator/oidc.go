package authenticator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"
)

// VerifiedToken is the result of validating a raw bearer token: the decoded
// claim set as a generic map, which the GroupMapper evaluates over and from
// which the username claim is read.
type VerifiedToken struct {
	// Claims is the validated token's claim set, unmarshalled into a generic map
	// (the shape go-oidc's IDToken.Claims produces). It is the input to the
	// GroupMapper and the source of the username claim.
	Claims map[string]any
}

// TokenVerifier validates a raw OIDC identity token and returns its claims. It
// is the seam the Authenticator validates tokens through, so tests inject a fake
// verifier (signing test JWTs with a local key) without a live issuer or JWKS
// endpoint. The production implementation is oidcVerifier, backed by go-oidc's
// *oidc.IDTokenVerifier (signature against the discovered JWKS, plus iss, aud,
// exp/nbf checks).
type TokenVerifier interface {
	// Verify validates rawToken and returns its claims, or an error when the
	// signature, issuer, audience, or expiry/not-before checks fail.
	Verify(ctx context.Context, rawToken string) (*VerifiedToken, error)
}

// oidcVerifier is the production TokenVerifier: it wraps go-oidc's
// *oidc.IDTokenVerifier, which checks the token signature against the issuer's
// discovered JWKS and validates the iss, aud (== clientID), and exp/nbf claims.
type oidcVerifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewOIDCVerifier wraps a pre-built *oidc.IDTokenVerifier as a TokenVerifier.
// Production code reaches a verifier through DiscoverVerifier (which discovers
// the issuer and builds the verifier from its JWKS); this constructor exists so
// tests can build a verifier from a StaticKeySet (oidc.NewVerifier) — exercising
// the same Verify code path that validates signature, iss, aud, and exp/nbf —
// without standing up a live issuer or JWKS HTTP endpoint.
func NewOIDCVerifier(verifier *oidc.IDTokenVerifier) TokenVerifier {
	return &oidcVerifier{verifier: verifier}
}

// Verify validates rawToken with the wrapped go-oidc verifier and extracts its
// claims into a generic map.
func (v *oidcVerifier) Verify(ctx context.Context, rawToken string) (*VerifiedToken, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("verifying OIDC token: %w", err)
	}
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decoding OIDC token claims: %w", err)
	}
	return &VerifiedToken{Claims: claims}, nil
}

// DiscoverVerifier performs OIDC discovery against issuerURL and returns a
// TokenVerifier bound to clientID (the expected audience), trusting caBundle in
// addition to the system store when it is non-empty. It is the production
// DiscoverFunc the reconciler calls; tests substitute a DiscoverFunc that returns
// a fake verifier so no live issuer is contacted.
//
// Discovery fetches issuerURL/.well-known/openid-configuration and the JWKS it
// advertises, so a failure here means the issuer is unreachable or
// misconfigured — the reconciler maps it to Programmed=False (DiscoveryFailed).
func DiscoverVerifier(ctx context.Context, issuerURL, clientID string, caBundle []byte) (TokenVerifier, error) {
	httpClient, err := oidcHTTPClient(caBundle)
	if err != nil {
		return nil, err
	}

	// go-oidc reads the *http.Client from the context for both discovery and the
	// subsequent JWKS fetch the remote KeySet performs, so the caBundle-trusting
	// client is honored end to end.
	ctx = oidc.ClientContext(ctx, httpClient)

	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: %w", issuerURL, err)
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})
	return &oidcVerifier{verifier: verifier}, nil
}

// DiscoverFunc is the discovery seam: given an issuer, client ID, and optional CA
// bundle it returns a TokenVerifier. DiscoverVerifier is the production
// implementation; the reconciler holds a DiscoverFunc field so tests inject a
// fake that returns a stub verifier without reaching a live issuer.
type DiscoverFunc func(ctx context.Context, issuerURL, clientID string, caBundle []byte) (TokenVerifier, error)

// oidcHTTPClient builds an *http.Client whose TLS transport trusts caBundle in
// addition to the system root store. An empty caBundle yields the default client
// (system trust only). A caBundle that parses to no certificate is an error so a
// misconfigured bundle surfaces at discovery rather than silently falling back to
// system trust and reporting Ready for an unhonored spec.
func oidcHTTPClient(caBundle []byte) (*http.Client, error) {
	if len(caBundle) == 0 {
		return http.DefaultClient, nil
	}

	systemPool, err := x509.SystemCertPool()
	if err != nil || systemPool == nil {
		// A nil/failed system pool is not fatal: start from an empty pool so the
		// explicit caBundle is still honored (the issuer may be signed solely by
		// the supplied CA, e.g. the in-cluster local CA).
		systemPool = x509.NewCertPool()
	}
	if !systemPool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("oidc caBundle contains no valid PEM certificate")
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    systemPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}

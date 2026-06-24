package authenticator

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// maxJWKSBody bounds the JWKS response body read during the discovery
// reachability probe so a hostile or misconfigured issuer cannot stream an
// unbounded body into the reconciler. 1 MiB is far larger than any real JWKS.
const maxJWKSBody = 1 << 20

// discoveryTimeout bounds the whole OIDC discovery + JWKS-probe round trip so a
// hung or slow issuer cannot pin a reconcile worker indefinitely. It applies as
// both an http.Client.Timeout (each request) and a context deadline (the overall
// operation), so neither a stalled connection nor a slow-drip body can hang the
// reconciler.
const discoveryTimeout = 30 * time.Second

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

// StaticVerifier builds a TokenVerifier from a static JWKS document, performing
// NO network I/O: it validates token signatures against the keys carried inline
// in jwks (the literal {"keys":[...]} document) instead of discovering the
// issuer and fetching its JWKS over HTTP. The returned verifier still enforces
// iss (== issuerURL), aud (== clientID), and exp/nbf, flowing through the same
// Verify code path as the discovery verifier (it wraps go-oidc's
// *oidc.IDTokenVerifier built over the same hardenedKeySet the discovery path
// uses, binding every token to its header kid and per-key alg).
//
// It is the offline counterpart to DiscoverVerifier for a token issuer that is
// unreachable from this cluster (e.g. a remote cluster's Kubernetes API server
// signing service-account ID tokens). An unparseable JWKS, or a JWKS with no
// usable keys, is an error so the reconciler rejects the spec rather than
// registering a backend whose tokens can never be verified.
func StaticVerifier(issuerURL, clientID string, jwks []byte) (TokenVerifier, error) {
	var keySet jose.JSONWebKeySet
	if err := json.Unmarshal(jwks, &keySet); err != nil {
		return nil, fmt.Errorf("parsing static JWKS: %w", err)
	}

	// A static JWKS is the user's spec, so any key that cannot serve as a signing
	// key (unsupported key type, or a stated `alg` its key type cannot produce) is a
	// spec error rather than something to silently skip — strict=true rejects the
	// whole document so the Backend is never marked Ready with a key it can never
	// verify a token against. The hardened key set then binds every token to its
	// header `kid` and the per-key `alg`, identically to the discovery path.
	hardened, err := newHardenedKeySet(keySet, true)
	if err != nil {
		return nil, fmt.Errorf("static JWKS: %w", err)
	}

	verifier := oidc.NewVerifier(issuerURL, hardened, &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: hardened.supportedSigningAlgs(),
	})
	return NewOIDCVerifier(verifier), nil
}

// signingKey is one trusted JWK reduced to what signature verification needs: its
// key id (possibly empty), the public key material, and the JWS algorithms its
// JWK metadata authorizes it to verify under. algs holds the single stated `alg`
// when the JWK declares one, otherwise the full family the key type can produce.
type signingKey struct {
	keyID string
	algs  []jose.SignatureAlgorithm
	pub   crypto.PublicKey
}

// hardenedKeySet is an oidc.KeySet over a fixed set of trusted signing keys that
// (1) selects the verification key by the token's protected-header `kid` — a
// token naming a kid is verified ONLY against the key carrying that kid — and
// (2) enforces each key's authorized algorithm set before verifying, so a token
// signed with an alg its own key does not declare is rejected even when another
// trusted key in the set declares it. It is the shared model for both the static
// (StaticVerifier) and discovery (DiscoverVerifier) JWKS paths so the two never
// diverge: neither go-oidc's StaticKeySet (tries every key, ignores kid) nor its
// RemoteKeySet (matches kid but binds alg only globally) enforces both.
type hardenedKeySet struct {
	keys []signingKey
	// allowAlgs is the union of every key's authorized algs, the allow-list
	// jose.ParseSigned is given so a token whose alg no key declares is rejected at
	// parse before any key is tried.
	allowAlgs []jose.SignatureAlgorithm
}

// newHardenedKeySet reduces a parsed JWKS to the signing keys used for
// verification, sharing the use/alg validation between the static and discovery
// paths. A JWK with use other than "sig" is excluded (an empty use is permitted —
// RFC 7517 makes it optional); key.Public() strips any private material so a JWKS
// that accidentally carries a private key still yields only its public half
// (defense in depth). The asymmetric algorithm(s) compatible with each key's
// type/curve are derived so the verifier accepts exactly the algs the keys use
// rather than go-oidc's RS256-only default. When strict, an unsupported key type
// or a stated `alg` incompatible with the key type is an error (the static path's
// spec validation); otherwise such a key is skipped (the discovery path, whose
// JWKS is the issuer's, not the user's spec). A JWKS yielding no usable signing
// key is always an error.
func newHardenedKeySet(keySet jose.JSONWebKeySet, strict bool) (*hardenedKeySet, error) {
	var (
		keys []signingKey
		seen = map[jose.SignatureAlgorithm]struct{}{}
		all  []jose.SignatureAlgorithm
	)
	for _, key := range usableJWKSKeys(keySet) {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		pub := key.Public().Key

		compatible := keyAlgs(pub)
		if len(compatible) == 0 {
			if strict {
				return nil, fmt.Errorf("key %q has an unsupported key type for signature verification", key.KeyID)
			}
			continue
		}

		// The JWK `alg` is authoritative when present (RFC 7517 makes it optional). A
		// stated alg must be one the key type can actually produce: a symmetric, typo'd,
		// or mismatched alg (e.g. "HS256" or "ES256" on an RSA key) binds the key to an
		// alg it can never verify under. When absent, the key is bound to the full family
		// its type can produce (RSA→RS*/PS*, P-256→ES256, …).
		var algs []jose.SignatureAlgorithm
		if key.Algorithm != "" {
			if !containsString(compatible, key.Algorithm) {
				if strict {
					return nil, fmt.Errorf("key %q declares alg %q incompatible with its key type", key.KeyID, key.Algorithm)
				}
				continue
			}
			algs = []jose.SignatureAlgorithm{jose.SignatureAlgorithm(key.Algorithm)}
		} else {
			algs = toSigAlgs(compatible)
		}

		keys = append(keys, signingKey{keyID: key.KeyID, algs: algs, pub: pub})
		for _, alg := range algs {
			if _, ok := seen[alg]; !ok {
				seen[alg] = struct{}{}
				all = append(all, alg)
			}
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("contains no usable signing keys")
	}
	return &hardenedKeySet{keys: keys, allowAlgs: all}, nil
}

// VerifySignature implements oidc.KeySet. go-oidc has already checked the token's
// alg against the verifier's SupportedSigningAlgs and the iss/aud/exp claims are
// checked by the caller; this method is the signature check, hardened to bind the
// token to a specific trusted key by `kid` and to that key's authorized alg.
func (k *hardenedKeySet) VerifySignature(_ context.Context, rawToken string) ([]byte, error) {
	jws, err := jose.ParseSigned(rawToken, k.allowAlgs)
	if err != nil {
		return nil, fmt.Errorf("parsing token: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, fmt.Errorf("token must carry exactly one signature, got %d", len(jws.Signatures))
	}
	// Select on the PROTECTED header only: the merged Header field can also carry
	// unprotected (unsigned, untrusted) values, so kid/alg used for key selection
	// must come from the signed header.
	header := jws.Signatures[0].Protected
	tokenKID := header.KeyID
	tokenAlg := jose.SignatureAlgorithm(header.Algorithm)

	matchedKID := false
	for _, sk := range k.keys {
		// A token naming a kid is verified only against the key carrying that kid; a
		// key without a kid matches a token without a kid (go-oidc's RemoteKeySet
		// convention). This rejects a token whose kid points at key B but is signed by
		// key A.
		if tokenKID != "" && sk.keyID != tokenKID {
			continue
		}
		matchedKID = true
		// Enforce the per-key alg binding before the (expensive) signature check: a
		// token whose alg this key does not declare is not verifiable by it even if
		// some other key in the set declares that alg.
		if !containsSigAlg(sk.algs, tokenAlg) {
			continue
		}
		if payload, err := jws.Verify(sk.pub); err == nil {
			return payload, nil
		}
	}
	if !matchedKID {
		return nil, fmt.Errorf("no trusted key matches token kid %q", tokenKID)
	}
	return nil, fmt.Errorf("token signature does not verify against the key bound to kid %q for alg %q", tokenKID, tokenAlg)
}

// supportedSigningAlgs returns the union of authorized algs as the []string
// go-oidc's Config.SupportedSigningAlgs wants, so go-oidc's own alg pre-check
// admits exactly the algs the keys use (not its RS256-only default) before
// delegating the signature check to VerifySignature.
func (k *hardenedKeySet) supportedSigningAlgs() []string {
	out := make([]string, len(k.allowAlgs))
	for i, alg := range k.allowAlgs {
		out[i] = string(alg)
	}
	return out
}

// toSigAlgs converts oidc/JOSE alg name strings to jose.SignatureAlgorithm.
func toSigAlgs(ss []string) []jose.SignatureAlgorithm {
	out := make([]jose.SignatureAlgorithm, len(ss))
	for i, s := range ss {
		out[i] = jose.SignatureAlgorithm(s)
	}
	return out
}

// containsSigAlg reports whether alg is in algs.
func containsSigAlg(algs []jose.SignatureAlgorithm, alg jose.SignatureAlgorithm) bool {
	for _, a := range algs {
		if a == alg {
			return true
		}
	}
	return false
}

// keyAlgs returns the JWS signing algorithms a public key can verify, canonical
// (default) alg first, for validating a JWK's stated `alg` and for choosing the
// alg when the JWK omits the optional `alg` member. An RSA key supports the
// RSASSA-PKCS1-v1.5 and RSASSA-PSS families (RS*/PS*); each NIST curve maps to its
// single ECDSA alg (P-256→ES256, P-384→ES384, P-521→ES512); Ed25519→EdDSA. An
// unrecognized or symmetric key type returns nil so the caller rejects it rather
// than asserting an algorithm the key cannot use.
func keyAlgs(pub crypto.PublicKey) []string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.PS256, oidc.PS384, oidc.PS512}
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return []string{oidc.ES256}
		case elliptic.P384():
			return []string{oidc.ES384}
		case elliptic.P521():
			return []string{oidc.ES512}
		}
		return nil
	case ed25519.PublicKey:
		return []string{oidc.EdDSA}
	default:
		return nil
	}
}

// containsString reports whether s is in xs.
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// usableJWKSKeys returns the keys in keySet whose material is present and
// consistent per JSONWebKey.Valid(). A key set like {"keys":[{}]} unmarshals to
// one element but carries no usable key material; Valid() filters those out. It
// is the shared usable-key predicate for both the discovery JWKS probe
// (probeJWKS) and the static verifier (StaticVerifier) so the two never diverge.
func usableJWKSKeys(keySet jose.JSONWebKeySet) []jose.JSONWebKey {
	usable := make([]jose.JSONWebKey, 0, len(keySet.Keys))
	for i := range keySet.Keys {
		if keySet.Keys[i].Valid() {
			usable = append(usable, keySet.Keys[i])
		}
	}
	return usable
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

	// Bound the overall discovery + JWKS probe with a deadline so a hung issuer
	// cannot pin the reconcile worker. The http.Client also carries its own
	// per-request Timeout (oidcHTTPClient) as a second line of defense for a stalled
	// connection the context cancellation alone might not interrupt promptly.
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	// go-oidc reads the *http.Client from the context for both discovery and the
	// subsequent JWKS fetch the remote KeySet performs, so the caBundle-trusting
	// client is honored end to end.
	ctx = oidc.ClientContext(ctx, httpClient)

	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: %w", issuerURL, err)
	}

	// Discovery only fetched the discovery document. Fetch the advertised JWKS now —
	// rather than letting go-oidc's RemoteKeySet fetch it lazily on first
	// verification — so a reachable issuer with a broken or empty key set fails
	// discovery (Programmed=False) instead of being marked Ready and then failing
	// every token verification later (Codex round 1), AND so the same hardened
	// kid/alg-binding key set the static path uses verifies discovery-path tokens
	// (HOL-1396), keeping the two paths from diverging. The JWKS is snapshotted at
	// discovery time; the reconciler rebuilds the verifier (re-fetching the JWKS) on
	// every Backend reconcile and informer resync, which is when issuer key rotation
	// is picked up.
	keySet, err := fetchJWKS(ctx, httpClient, provider)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: %w", issuerURL, err)
	}
	hardened, err := newHardenedKeySet(keySet, false)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: JWKS %w", issuerURL, err)
	}

	verifier := oidc.NewVerifier(issuerURL, hardened, &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: hardened.supportedSigningAlgs(),
	})
	return &oidcVerifier{verifier: verifier}, nil
}

// fetchJWKS reads the issuer's advertised jwks_uri and returns the parsed JSON Web
// Key Set. A missing jwks_uri, an unreachable endpoint, an unparseable body, or a
// key set with no usable keys is an error so the reconciler marks the backend
// NotReady rather than registering a backend whose tokens can never be verified.
func fetchJWKS(ctx context.Context, httpClient *http.Client, provider *oidc.Provider) (jose.JSONWebKeySet, error) {
	var keySet jose.JSONWebKeySet

	var claims struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&claims); err != nil {
		return keySet, fmt.Errorf("reading discovery claims: %w", err)
	}
	if claims.JWKSURL == "" {
		return keySet, fmt.Errorf("issuer discovery document advertises no jwks_uri")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claims.JWKSURL, nil)
	if err != nil {
		return keySet, fmt.Errorf("building JWKS request for %q: %w", claims.JWKSURL, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return keySet, fmt.Errorf("fetching JWKS from %q: %w", claims.JWKSURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return keySet, fmt.Errorf("fetching JWKS from %q: status %d", claims.JWKSURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBody))
	if err != nil {
		return keySet, fmt.Errorf("reading JWKS from %q: %w", claims.JWKSURL, err)
	}

	// Parse as a real JWK set, not just a "keys" array length check: a key set like
	// {"keys":[{}]} unmarshals to one element but carries no usable key material,
	// and would be marked Ready yet fail every token verification. jose.JSONWebKeySet
	// decodes each key's algorithm/usage/material, and JSONWebKey.Valid() confirms
	// the material is present and consistent. Require at least one valid key.
	if err := json.Unmarshal(body, &keySet); err != nil {
		return keySet, fmt.Errorf("parsing JWKS from %q: %w", claims.JWKSURL, err)
	}
	if len(usableJWKSKeys(keySet)) == 0 {
		return keySet, fmt.Errorf("JWKS from %q contains no usable keys", claims.JWKSURL)
	}
	return keySet, nil
}

// DiscoverFunc is the discovery seam: given an issuer, client ID, and optional CA
// bundle it returns a TokenVerifier. DiscoverVerifier is the production
// implementation; the reconciler holds a DiscoverFunc field so tests inject a
// fake that returns a stub verifier without reaching a live issuer.
type DiscoverFunc func(ctx context.Context, issuerURL, clientID string, caBundle []byte) (TokenVerifier, error)

// oidcHTTPClient builds an *http.Client (with a finite per-request Timeout) whose
// TLS transport trusts caBundle in addition to the system root store. An empty
// caBundle yields a client using system trust only — but never http.DefaultClient,
// which has no timeout and is process-global; a dedicated timed client keeps a
// hung issuer from pinning a reconcile worker. A caBundle that parses to no
// certificate is an error so a misconfigured bundle surfaces at discovery rather
// than silently falling back to system trust and reporting Ready for an unhonored
// spec.
func oidcHTTPClient(caBundle []byte) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if len(caBundle) > 0 {
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
		tlsConfig.RootCAs = systemPool
	}

	return &http.Client{
		Timeout:   discoveryTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}, nil
}

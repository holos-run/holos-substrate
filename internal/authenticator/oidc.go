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
	"sync"
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
	// spec error rather than something to silently skip — newStaticKeySet rejects the
	// whole document so the Backend is never marked Ready with a key it can never
	// verify a token against. The hardened key set then binds every token to its
	// header `kid` and the per-key `alg`, identically to the discovery path.
	hardened, err := newStaticKeySet(keySet)
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

// jwksRefreshInterval rate-limits the discovery key set's refresh-on-unknown-kid
// (see hardenedKeySet.tryRefresh) so a flood of tokens bearing bogus kids cannot
// stampede the issuer's JWKS endpoint: at most one refetch per interval.
const jwksRefreshInterval = time.Minute

// hardenedKeySet is an oidc.KeySet over a set of trusted signing keys that
// (1) selects the verification key by the token's protected-header `kid` — a
// token naming a kid is verified ONLY against the key carrying that kid — and
// (2) enforces each key's authorized algorithm set before verifying, so a token
// signed with an alg its own key does not declare is rejected even when another
// trusted key in the set declares it. It is the shared model for both the static
// (StaticVerifier) and discovery (DiscoverVerifier) JWKS paths so the two never
// diverge: neither go-oidc's StaticKeySet (tries every key, ignores kid) nor its
// RemoteKeySet (matches kid but binds alg only globally) enforces both.
type hardenedKeySet struct {
	// mu guards keys/allowAlgs, which the discovery path replaces in place when it
	// refreshes the issuer JWKS; the static path never mutates them after build.
	mu        sync.RWMutex
	keys      []signingKey
	allowAlgs []jose.SignatureAlgorithm // union of every key's algs — the jose.ParseSigned allow-list

	// refresh, when non-nil, re-fetches and re-reduces the issuer JWKS. It is set
	// only on the discovery path so a key the issuer rotates in after this verifier
	// was built is picked up on the first token that presents the new (currently
	// unknown) kid — mirroring go-oidc's RemoteKeySet refresh-on-unknown-kid — rather
	// than being rejected until the next reconcile. The static path leaves it nil:
	// its JWKS is fixed spec data that only a spec edit (a fresh reconcile) changes.
	refreshMu   sync.Mutex
	refresh     func(context.Context) ([]signingKey, []jose.SignatureAlgorithm, error)
	lastRefresh time.Time
	minRefresh  time.Duration
}

// newStaticKeySet builds the hardened key set for the static path: strict spec
// validation (a key with an unsupported type or an incompatible stated alg is a
// spec error), no issuer-advertised-alg constraint (there is no discovery
// document), and no refresh (the JWKS is fixed spec data).
func newStaticKeySet(keySet jose.JSONWebKeySet) (*hardenedKeySet, error) {
	keys, algs, err := reduceJWKS(keySet, true, nil)
	if err != nil {
		return nil, err
	}
	return &hardenedKeySet{keys: keys, allowAlgs: algs}, nil
}

// reduceJWKS reduces a parsed JWKS to the trusted signing keys and the union of
// their authorized algs — the shared use/alg validation behind both the static and
// discovery paths. A JWK with use other than "sig" is excluded (an empty use is
// permitted — RFC 7517 makes it optional); key.Public() strips any private material
// so a JWKS that accidentally carries a private key still yields only its public
// half (defense in depth). The asymmetric algorithm(s) compatible with each key's
// type/curve are derived so the verifier accepts exactly the algs the keys use
// rather than go-oidc's RS256-only default.
//
// When strict, an unsupported key type or a stated `alg` incompatible with the key
// type is an error (the static path's spec validation); otherwise such a key is
// skipped (the discovery path, whose JWKS is the issuer's, not the user's spec).
// When advertisedAlgs is non-empty (the discovery document's
// id_token_signing_alg_values_supported), each key's authorized algs are
// intersected with it so the verifier never accepts an alg the issuer says it does
// not use — and an alg-less key is not silently widened past the advertised set. A
// JWKS yielding no usable signing key is always an error.
func reduceJWKS(keySet jose.JSONWebKeySet, strict bool, advertisedAlgs []string) ([]signingKey, []jose.SignatureAlgorithm, error) {
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
				return nil, nil, fmt.Errorf("key %q has an unsupported key type for signature verification", key.KeyID)
			}
			continue
		}

		// The JWK `alg` is authoritative when present (RFC 7517 makes it optional). A
		// stated alg must be one the key type can actually produce: a symmetric, typo'd,
		// or mismatched alg (e.g. "HS256" or "ES256" on an RSA key) binds the key to an
		// alg it can never verify under. When absent, the key is bound to the full family
		// its type can produce (RSA→RS*/PS*, P-256→ES256, …).
		var algs []string
		if key.Algorithm != "" {
			if !containsString(compatible, key.Algorithm) {
				if strict {
					return nil, nil, fmt.Errorf("key %q declares alg %q incompatible with its key type", key.KeyID, key.Algorithm)
				}
				continue
			}
			algs = []string{key.Algorithm}
		} else {
			algs = compatible
		}

		// Honor the issuer's advertised signing algs (discovery only): drop any alg the
		// issuer does not advertise so a key is never bound to an alg the issuer says it
		// does not use. A key left with no advertised alg contributes nothing.
		if len(advertisedAlgs) > 0 {
			algs = intersectStrings(algs, advertisedAlgs)
			if len(algs) == 0 {
				continue
			}
		}

		sigAlgs := toSigAlgs(algs)
		keys = append(keys, signingKey{keyID: key.KeyID, algs: sigAlgs, pub: pub})
		for _, alg := range sigAlgs {
			if _, ok := seen[alg]; !ok {
				seen[alg] = struct{}{}
				all = append(all, alg)
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("contains no usable signing keys")
	}
	return keys, all, nil
}

// VerifySignature implements oidc.KeySet. go-oidc has already checked the token's
// alg against the verifier's SupportedSigningAlgs and the iss/aud/exp claims are
// checked by the caller; this method is the signature check, hardened to bind the
// token to a specific trusted key by `kid` and to that key's authorized alg. When
// the token names a kid no loaded key matches and a refresh is wired (discovery),
// it re-fetches the issuer JWKS once and retries — picking up a rotated-in key.
func (k *hardenedKeySet) VerifySignature(ctx context.Context, rawToken string) ([]byte, error) {
	payload, refreshable, err := k.verifyOnce(rawToken)
	if err == nil {
		return payload, nil
	}
	// Refresh only when the token parsed and named a kid no currently-trusted key
	// matches, and a refresh is configured (the discovery path): an unknown kid is
	// the signal a key was rotated in since this set was built. A malformed token, or
	// one that matched a kid but failed to verify, is a genuine rejection (not
	// staleness) and must not provoke a refetch.
	if !refreshable || k.refresh == nil {
		return nil, err
	}
	if !k.tryRefresh(ctx) {
		return nil, err
	}
	payload, _, retryErr := k.verifyOnce(rawToken)
	if retryErr != nil {
		return nil, retryErr
	}
	return payload, nil
}

// verifyOnce parses rawToken and verifies its signature against the currently
// loaded keys. refreshable is true only when the token parsed but named a kid no
// loaded key matched — the one case a JWKS refetch could resolve — so the caller
// distinguishes a rotated-in-key miss from a malformed token or a real signature
// rejection.
func (k *hardenedKeySet) verifyOnce(rawToken string) (payload []byte, refreshable bool, err error) {
	// Snapshot the slice headers under the read lock: refresh replaces them wholesale
	// (it never mutates the backing array), so iterating the snapshot is safe without
	// holding the lock across the verification.
	k.mu.RLock()
	keys := k.keys
	allowAlgs := k.allowAlgs
	k.mu.RUnlock()

	jws, err := jose.ParseSigned(rawToken, allowAlgs)
	if err != nil {
		return nil, false, fmt.Errorf("parsing token: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, false, fmt.Errorf("token must carry exactly one signature, got %d", len(jws.Signatures))
	}
	// Select on the PROTECTED header only: the merged Header field can also carry
	// unprotected (unsigned, untrusted) values, so kid/alg used for key selection
	// must come from the signed header.
	header := jws.Signatures[0].Protected
	tokenKID := header.KeyID
	tokenAlg := jose.SignatureAlgorithm(header.Algorithm)

	matchedKID := false
	for _, sk := range keys {
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
		if pl, verr := jws.Verify(sk.pub); verr == nil {
			return pl, false, nil
		}
	}
	if !matchedKID {
		// Unknown kid — the only miss a JWKS refetch could resolve.
		return nil, true, fmt.Errorf("no trusted key matches token kid %q", tokenKID)
	}
	return nil, false, fmt.Errorf("token signature does not verify against the key bound to kid %q for alg %q", tokenKID, tokenAlg)
}

// tryRefresh re-fetches the issuer JWKS and swaps in the fresh keys, rate-limited
// to at most one refetch per minRefresh so a burst of unknown-kid tokens cannot
// stampede the issuer. It reports whether the caller should retry verification:
// true after a (re)load or when another goroutine refreshed within the window,
// false when the refetch itself failed.
func (k *hardenedKeySet) tryRefresh(ctx context.Context) bool {
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()

	// Coalesce concurrent/bursty unknown-kid misses: if a refresh ran within the
	// window, skip the refetch. A caller that blocked on refreshMu while the holder
	// refreshed falls here and retries against the just-loaded keys.
	if !k.lastRefresh.IsZero() && time.Since(k.lastRefresh) < k.minRefresh {
		return true
	}
	keys, algs, err := k.refresh(ctx)
	k.lastRefresh = time.Now()
	if err != nil {
		return false
	}
	k.mu.Lock()
	k.keys = keys
	k.allowAlgs = algs
	k.mu.Unlock()
	return true
}

// supportedSigningAlgs returns the union of authorized algs as the []string
// go-oidc's Config.SupportedSigningAlgs wants, so go-oidc's own alg pre-check
// admits exactly the algs the keys use (not its RS256-only default) before
// delegating the signature check to VerifySignature. It is read once at verifier
// construction (go-oidc captures it), so a later refresh that introduces a wholly
// new alg family still requires a reconcile to widen this outer gate.
func (k *hardenedKeySet) supportedSigningAlgs() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
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

// intersectStrings returns the elements of xs that are also in ys, preserving xs's
// order.
func intersectStrings(xs, ys []string) []string {
	var out []string
	for _, x := range xs {
		if containsString(ys, x) {
			out = append(out, x)
		}
	}
	return out
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
// is the shared usable-key predicate for both the discovery JWKS fetch
// (fetchJWKSFromURL) and the static verifier (StaticVerifier) so the two never
// diverge.
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

	// Discovery only fetched the discovery document. Read the advertised jwks_uri and
	// id_token_signing_alg_values_supported, then fetch the JWKS now — rather than
	// letting go-oidc's RemoteKeySet fetch it lazily on first verification — so a
	// reachable issuer with a broken or empty key set fails discovery
	// (Programmed=False) instead of being marked Ready and then failing every token
	// verification later (Codex round 1), AND so the same hardened kid/alg-binding
	// key set the static path uses verifies discovery-path tokens (HOL-1396), keeping
	// the two paths from diverging.
	jwksURL, advertisedAlgs, err := discoveryClaims(provider)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: %w", issuerURL, err)
	}
	keySet, err := fetchJWKSFromURL(ctx, httpClient, jwksURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: %w", issuerURL, err)
	}
	keys, algs, err := reduceJWKS(keySet, false, advertisedAlgs)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for issuer %q: JWKS %w", issuerURL, err)
	}

	// Wire refresh-on-unknown-kid so issuer key rotation is picked up at the first
	// token bearing the new kid (mirroring go-oidc's RemoteKeySet), not deferred to
	// the next reconcile. The closure captures the timed, caBundle-trusting client
	// and the jwks_uri; it builds a fresh deadline per refetch because the discovery
	// ctx below is canceled when DiscoverVerifier returns, long before a data-path
	// token triggers a refresh.
	hardened := &hardenedKeySet{
		keys:       keys,
		allowAlgs:  algs,
		minRefresh: jwksRefreshInterval,
		refresh: func(rctx context.Context) ([]signingKey, []jose.SignatureAlgorithm, error) {
			rctx, cancel := context.WithTimeout(rctx, discoveryTimeout)
			defer cancel()
			set, err := fetchJWKSFromURL(rctx, httpClient, jwksURL)
			if err != nil {
				return nil, nil, err
			}
			return reduceJWKS(set, false, advertisedAlgs)
		},
	}

	verifier := oidc.NewVerifier(issuerURL, hardened, &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: hardened.supportedSigningAlgs(),
	})
	return &oidcVerifier{verifier: verifier}, nil
}

// discoveryClaims reads the issuer's jwks_uri and advertised
// id_token_signing_alg_values_supported from the discovery document. A missing
// jwks_uri is an error so the reconciler marks the backend NotReady rather than
// registering a backend whose tokens can never be verified.
func discoveryClaims(provider *oidc.Provider) (jwksURL string, advertisedAlgs []string, err error) {
	var claims struct {
		JWKSURL     string   `json:"jwks_uri"`
		SigningAlgs []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&claims); err != nil {
		return "", nil, fmt.Errorf("reading discovery claims: %w", err)
	}
	if claims.JWKSURL == "" {
		return "", nil, fmt.Errorf("issuer discovery document advertises no jwks_uri")
	}
	return claims.JWKSURL, claims.SigningAlgs, nil
}

// fetchJWKSFromURL GETs jwksURL with httpClient and returns the parsed JSON Web Key
// Set. An unreachable endpoint, a non-200 status, an unparseable body, or a key set
// with no usable keys is an error. It is shared by the initial discovery fetch and
// the refresh-on-unknown-kid refetch so both apply the same usable-key check.
func fetchJWKSFromURL(ctx context.Context, httpClient *http.Client, jwksURL string) (jose.JSONWebKeySet, error) {
	var keySet jose.JSONWebKeySet

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return keySet, fmt.Errorf("building JWKS request for %q: %w", jwksURL, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return keySet, fmt.Errorf("fetching JWKS from %q: %w", jwksURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return keySet, fmt.Errorf("fetching JWKS from %q: status %d", jwksURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBody))
	if err != nil {
		return keySet, fmt.Errorf("reading JWKS from %q: %w", jwksURL, err)
	}

	// Parse as a real JWK set, not just a "keys" array length check: a key set like
	// {"keys":[{}]} unmarshals to one element but carries no usable key material,
	// and would be marked Ready yet fail every token verification. jose.JSONWebKeySet
	// decodes each key's algorithm/usage/material, and JSONWebKey.Valid() confirms
	// the material is present and consistent. Require at least one valid key.
	if err := json.Unmarshal(body, &keySet); err != nil {
		return keySet, fmt.Errorf("parsing JWKS from %q: %w", jwksURL, err)
	}
	if len(usableJWKSKeys(keySet)) == 0 {
		return keySet, fmt.Errorf("JWKS from %q contains no usable keys", jwksURL)
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

package authenticator

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
)

const (
	testIssuer   = "https://issuer.example.test/realms/holos"
	testClientID = "holos-authenticator"
)

// testSigningKey holds an RSA key pair the OIDC tests sign and verify test JWTs
// with, standing in for the issuer's signing key and the JWKS it publishes.
type testSigningKey struct {
	signer  jose.Signer
	public  crypto.PublicKey
	private *rsa.PrivateKey
}

// newTestSigningKey generates a fresh RSA-2048 key pair and a RS256 signer for it.
func newTestSigningKey(t *testing.T) *testSigningKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building signer: %v", err)
	}
	return &testSigningKey{signer: signer, public: priv.Public(), private: priv}
}

// sign builds and signs a JWT carrying the supplied claims.
func (k *testSigningKey) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := jwt.Signed(k.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	return raw
}

// verifierFor builds the production oidcVerifier wrapping a go-oidc verifier
// backed by a StaticKeySet of the supplied public keys, with the clock pinned to
// now so expiry/not-before checks are deterministic.
func verifierFor(pubKeys []crypto.PublicKey, now time.Time) TokenVerifier {
	keySet := &oidc.StaticKeySet{PublicKeys: pubKeys}
	verifier := oidc.NewVerifier(testIssuer, keySet, &oidc.Config{
		ClientID:             testClientID,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  func() time.Time { return now },
	})
	return NewOIDCVerifier(verifier)
}

// baseClaims returns a valid claim set for the given subject and audience,
// timestamped relative to now.
func baseClaims(sub, aud string, now time.Time) map[string]any {
	return map[string]any{
		"iss":    testIssuer,
		"sub":    sub,
		"aud":    aud,
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Add(-time.Minute).Unix(),
		"nbf":    now.Add(-time.Minute).Unix(),
		"groups": []string{"dev", "ops"},
	}
}

// TestOIDCVerifyHappyPath verifies a well-formed, correctly-signed token with the
// right issuer and audience succeeds and yields the claims.
func TestOIDCVerifyHappyPath(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier := verifierFor([]crypto.PublicKey{key.public}, now)

	raw := key.sign(t, baseClaims("alice", testClientID, now))

	vt, err := verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify happy path: %v", err)
	}
	if got := vt.Claims["sub"]; got != "alice" {
		t.Errorf("sub claim = %v, want alice", got)
	}
}

// TestOIDCVerifyExpired asserts an expired token is rejected.
func TestOIDCVerifyExpired(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier := verifierFor([]crypto.PublicKey{key.public}, now)

	claims := baseClaims("alice", testClientID, now)
	claims["exp"] = now.Add(-time.Minute).Unix() // already expired
	raw := key.sign(t, claims)

	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify expired token = nil error, want rejection")
	}
}

// TestOIDCVerifyWrongAudience asserts a token whose audience is not the client ID
// is rejected.
func TestOIDCVerifyWrongAudience(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier := verifierFor([]crypto.PublicKey{key.public}, now)

	raw := key.sign(t, baseClaims("alice", "some-other-client", now))

	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify wrong-audience token = nil error, want rejection")
	}
}

// TestOIDCVerifyBadSignature asserts a token signed by a different key than the
// verifier trusts is rejected (signature mismatch).
func TestOIDCVerifyBadSignature(t *testing.T) {
	now := time.Now()
	signingKey := newTestSigningKey(t)
	trustedKey := newTestSigningKey(t) // a different key the verifier trusts

	verifier := verifierFor([]crypto.PublicKey{trustedKey.public}, now)

	raw := signingKey.sign(t, baseClaims("alice", testClientID, now))

	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify bad-signature token = nil error, want rejection")
	}
}

// TestOIDCVerifyWrongIssuer asserts a token whose iss claim is not the configured
// issuer is rejected.
func TestOIDCVerifyWrongIssuer(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier := verifierFor([]crypto.PublicKey{key.public}, now)

	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = "https://evil.example.test"
	raw := key.sign(t, claims)

	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify wrong-issuer token = nil error, want rejection")
	}
}

// oidcDiscoveryServer starts an httptest server serving an OIDC discovery
// document and a JWKS. jwksKeys controls the published keys: when empty, the JWKS
// endpoint returns a key set with no keys (the broken-issuer case). It returns the
// server and its issuer URL.
func oidcDiscoveryServer(t *testing.T, pub *rsa.PublicKey, jwksKeys int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                 srv.URL,
			"jwks_uri":               srv.URL + "/jwks",
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{}
		for i := 0; i < jwksKeys; i++ {
			set.Keys = append(set.Keys, jose.JSONWebKey{
				Key:       pub,
				KeyID:     fmt.Sprintf("key-%d", i),
				Algorithm: string(jose.RS256),
				Use:       "sig",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})

	return srv
}

// TestDiscoverVerifierHappyPath asserts discovery succeeds against an issuer that
// advertises a non-empty JWKS, and the returned verifier validates a token signed
// by the published key.
func TestDiscoverVerifierHappyPath(t *testing.T) {
	key := newTestSigningKey(t)
	srv := oidcDiscoveryServer(t, &key.private.PublicKey, 1)

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL
	raw := key.sign(t, claims)

	if _, err := verifier.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify against discovered JWKS: %v", err)
	}
}

// TestDiscoverVerifierBrokenJWKS asserts discovery fails (rather than silently
// succeeding) when the issuer's jwks_uri returns an empty key set — the case
// Codex flagged where a Backend would be marked Ready yet fail every token verify.
func TestDiscoverVerifierBrokenJWKS(t *testing.T) {
	key := newTestSigningKey(t)
	srv := oidcDiscoveryServer(t, &key.private.PublicKey, 0) // empty JWKS

	if _, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil); err == nil {
		t.Fatalf("DiscoverVerifier with an empty JWKS = nil error, want failure")
	}
}

// rawJWKSDiscoveryServer serves a discovery document plus a verbatim JWKS body,
// so a test can publish malformed key material (e.g. {"keys":[{}]}).
func rawJWKSDiscoveryServer(t *testing.T, jwksBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jwksBody))
	})
	return srv
}

// TestDiscoverVerifierUnusableJWKSKey asserts discovery fails when the JWKS has a
// non-empty keys array but no usable key material ({"keys":[{}]}) — the deeper
// validation Codex's round-3 finding asked for, beyond a bare length check.
func TestDiscoverVerifierUnusableJWKSKey(t *testing.T) {
	srv := rawJWKSDiscoveryServer(t, `{"keys":[{}]}`)
	if _, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil); err == nil {
		t.Fatalf("DiscoverVerifier with an unusable JWKS key = nil error, want failure")
	}
}

// jwksDocument builds a JWKS document ({"keys":[...]}) publishing the given
// public key under a "sig"/RS256 entry, so a test can hand it to StaticVerifier
// exactly as a real issuer's JWKS would be copied into spec.oidc.jwks.
func jwksDocument(t *testing.T, pub crypto.PublicKey) []byte {
	t.Helper()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       pub,
		KeyID:     "key-0",
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}
	return raw
}

// TestStaticVerifierHappyPath asserts StaticVerifier builds an offline verifier
// from a static JWKS that validates a correctly-signed token with the right
// issuer and audience — no network I/O.
func TestStaticVerifierHappyPath(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)

	verifier, err := StaticVerifier(testIssuer, testClientID, jwksDocument(t, key.public))
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	raw := key.sign(t, baseClaims("alice", testClientID, now))
	vt, err := verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify happy path: %v", err)
	}
	if got := vt.Claims["sub"]; got != "alice" {
		t.Errorf("sub claim = %v, want alice", got)
	}
}

// TestStaticVerifierES256 asserts StaticVerifier honors a non-RS256 JWKS: an
// ES256 key set must derive SupportedSigningAlgs from the JWKS `alg` so an ES256
// token verifies, rather than failing under go-oidc's RS256-only default.
func TestStaticVerifierES256(t *testing.T) {
	now := time.Now()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building ES256 signer: %v", err)
	}

	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       priv.Public(),
		KeyID:     "es-0",
		Algorithm: string(jose.ES256),
		Use:       "sig",
	}}}
	jwks, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling ES256 JWKS: %v", err)
	}

	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	raw, err := jwt.Signed(signer).Claims(baseClaims("alice", testClientID, now)).Serialize()
	if err != nil {
		t.Fatalf("signing ES256 JWT: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify ES256 token against static JWKS: %v", err)
	}
}

// TestStaticVerifierMixedAlgs asserts a JWKS mixing an alg-less RSA key with an
// explicit ES256 key verifies tokens signed by either: the explicit ES256 alg is
// honored, and RS256 is retained for the alg-less RSA key (its inferred alg)
// rather than being dropped when another key carries an explicit alg.
func TestStaticVerifierMixedAlgs(t *testing.T) {
	now := time.Now()

	// RSA key published WITHOUT an alg (relies on the RS256 default).
	rsaKey := newTestSigningKey(t)
	// ECDSA key published WITH an explicit ES256 alg.
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	ecSigner, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: ecPriv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building ES256 signer: %v", err)
	}

	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: rsaKey.public, KeyID: "rsa-0", Use: "sig"}, // no Algorithm
		{Key: ecPriv.Public(), KeyID: "es-0", Algorithm: string(jose.ES256), Use: "sig"},
	}}
	jwks, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling mixed JWKS: %v", err)
	}

	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	// RS256 token signed by the alg-less RSA key must verify.
	rsRaw := rsaKey.sign(t, baseClaims("alice", testClientID, now))
	if _, err := verifier.Verify(context.Background(), rsRaw); err != nil {
		t.Fatalf("Verify RS256 token against mixed JWKS: %v", err)
	}
	// ES256 token signed by the explicit-alg key must verify.
	esRaw, err := jwt.Signed(ecSigner).Claims(baseClaims("bob", testClientID, now)).Serialize()
	if err != nil {
		t.Fatalf("signing ES256 JWT: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), esRaw); err != nil {
		t.Fatalf("Verify ES256 token against mixed JWKS: %v", err)
	}
}

// TestStaticVerifierAlgLessEC asserts StaticVerifier infers the signing alg from
// the key type/curve when the JWK omits the optional `alg`: an alg-less P-256 key
// must accept ES256 tokens, not fall back to RS256-only and reject every token.
func TestStaticVerifierAlgLessEC(t *testing.T) {
	now := time.Now()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building ES256 signer: %v", err)
	}

	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:   priv.Public(),
		KeyID: "es-0",
		Use:   "sig", // no Algorithm — must be inferred as ES256 from the P-256 curve
	}}}
	jwks, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling alg-less EC JWKS: %v", err)
	}

	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	raw, err := jwt.Signed(signer).Claims(baseClaims("alice", testClientID, now)).Serialize()
	if err != nil {
		t.Fatalf("signing ES256 JWT: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify ES256 token against alg-less EC JWKS: %v", err)
	}
}

// TestStaticVerifierMismatchedAlg asserts a JWK whose stated alg is incompatible
// with its key type (e.g. a symmetric HS256, or an ES256 alg on an RSA key) is
// rejected rather than building a verifier that would mark the Backend Ready yet
// reject every token at request time.
func TestStaticVerifierMismatchedAlg(t *testing.T) {
	key := newTestSigningKey(t) // RSA key

	for _, alg := range []string{"HS256", string(jose.ES256), "RS257" /* typo */} {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.public,
			KeyID:     "rsa-0",
			Algorithm: alg,
			Use:       "sig",
		}}}
		jwks, err := json.Marshal(set)
		if err != nil {
			t.Fatalf("marshaling JWKS: %v", err)
		}
		if _, err := StaticVerifier(testIssuer, testClientID, jwks); err == nil {
			t.Errorf("StaticVerifier with RSA key + alg %q = nil error, want rejection", alg)
		}
	}
}

// TestStaticVerifierRSAFamilyAlg asserts an RSA key may legitimately declare any
// RSA-family alg (e.g. PS256), not only RS256: the compatible-alg set must accept
// the whole RS*/PS* family for an RSA key.
func TestStaticVerifierRSAFamilyAlg(t *testing.T) {
	now := time.Now()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.PS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building PS256 signer: %v", err)
	}

	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       priv.Public(),
		KeyID:     "rsa-0",
		Algorithm: string(jose.PS256),
		Use:       "sig",
	}}}
	jwks, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}

	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier with RSA key + PS256: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(baseClaims("alice", testClientID, now)).Serialize()
	if err != nil {
		t.Fatalf("signing PS256 JWT: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify PS256 token: %v", err)
	}
}

// TestStaticVerifierMalformedJWKS asserts an unparseable JWKS document yields an
// error and no verifier.
func TestStaticVerifierMalformedJWKS(t *testing.T) {
	if _, err := StaticVerifier(testIssuer, testClientID, []byte("not json")); err == nil {
		t.Fatalf("StaticVerifier with a malformed JWKS = nil error, want failure")
	}
}

// TestStaticVerifierEmptyJWKS asserts a JWKS with no keys (and the unusable-key
// case {"keys":[{}]}) yields an error and no verifier.
func TestStaticVerifierEmptyJWKS(t *testing.T) {
	for _, body := range []string{`{"keys":[]}`, `{"keys":[{}]}`} {
		if _, err := StaticVerifier(testIssuer, testClientID, []byte(body)); err == nil {
			t.Errorf("StaticVerifier(%q) = nil error, want failure", body)
		}
	}
}

// TestStaticVerifierWrongAudience asserts the offline verifier rejects a token
// whose audience is not the configured client ID.
func TestStaticVerifierWrongAudience(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier, err := StaticVerifier(testIssuer, testClientID, jwksDocument(t, key.public))
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	raw := key.sign(t, baseClaims("alice", "some-other-client", now))
	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify wrong-audience token = nil error, want rejection")
	}
}

// TestStaticVerifierWrongIssuer asserts the offline verifier rejects a token
// whose iss claim is not the configured issuer.
func TestStaticVerifierWrongIssuer(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier, err := StaticVerifier(testIssuer, testClientID, jwksDocument(t, key.public))
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = "https://evil.example.test"
	raw := key.sign(t, claims)
	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify wrong-issuer token = nil error, want rejection")
	}
}

// TestStaticVerifierExpired asserts the offline verifier rejects an expired
// token (exp/nbf enforcement on the static path).
func TestStaticVerifierExpired(t *testing.T) {
	now := time.Now()
	key := newTestSigningKey(t)
	verifier, err := StaticVerifier(testIssuer, testClientID, jwksDocument(t, key.public))
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	claims := baseClaims("alice", testClientID, now)
	claims["exp"] = now.Add(-time.Minute).Unix() // already expired
	raw := key.sign(t, claims)
	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify expired token = nil error, want rejection")
	}
}

// TestStaticVerifierBadSignature asserts the offline verifier rejects a token
// signed by a key absent from the static JWKS.
func TestStaticVerifierBadSignature(t *testing.T) {
	now := time.Now()
	trusted := newTestSigningKey(t)
	other := newTestSigningKey(t) // signs the token but is not in the JWKS

	verifier, err := StaticVerifier(testIssuer, testClientID, jwksDocument(t, trusted.public))
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	raw := other.sign(t, baseClaims("alice", testClientID, now))
	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify bad-signature token = nil error, want rejection")
	}
}

// marshalJWKS serializes the given keys into a JWKS document ({"keys":[...]}) for
// handing to StaticVerifier or a discovery server.
func marshalJWKS(t *testing.T, keys ...jose.JSONWebKey) []byte {
	t.Helper()
	raw, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}
	return raw
}

// signWithKID signs claims with priv under alg, stamping the protected header's
// `kid` to the given value by wrapping the key in a *jose.JSONWebKey (go-jose
// copies its KeyID into the JWS header). This lets a test forge a token whose kid
// names one key while the signature is produced by another, or whose alg differs
// from the key's declared alg.
func signWithKID(t *testing.T, priv any, alg jose.SignatureAlgorithm, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: &jose.JSONWebKey{Key: priv, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building signer (alg %s, kid %q): %v", alg, kid, err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	return raw
}

// TestStaticVerifierKIDMismatch asserts the hardened key selection rejects a
// mixed-JWKS token whose header `kid` names key B but whose signature was produced
// by key A — the cross-key confusion the global StaticKeySet (which tries every
// key and ignores kid) would have accepted.
func TestStaticVerifierKIDMismatch(t *testing.T) {
	now := time.Now()
	keyA := newTestSigningKey(t)
	keyB := newTestSigningKey(t)
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: keyB.public, KeyID: "B", Algorithm: string(jose.RS256), Use: "sig"},
	)
	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	// Signed by key A's private key, but the header kid names key B.
	raw := signWithKID(t, keyA.private, jose.RS256, "B", baseClaims("alice", testClientID, now))
	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify kid-mismatch token = nil error, want rejection")
	}
}

// TestStaticVerifierKIDRouting asserts the happy multi-key path: each token is
// routed to and verified by the key its kid names.
func TestStaticVerifierKIDRouting(t *testing.T) {
	now := time.Now()
	keyA := newTestSigningKey(t)
	keyB := newTestSigningKey(t)
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: keyB.public, KeyID: "B", Algorithm: string(jose.RS256), Use: "sig"},
	)
	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	rawA := signWithKID(t, keyA.private, jose.RS256, "A", baseClaims("alice", testClientID, now))
	if _, err := verifier.Verify(context.Background(), rawA); err != nil {
		t.Fatalf("Verify token routed to key A: %v", err)
	}
	rawB := signWithKID(t, keyB.private, jose.RS256, "B", baseClaims("bob", testClientID, now))
	if _, err := verifier.Verify(context.Background(), rawB); err != nil {
		t.Fatalf("Verify token routed to key B: %v", err)
	}
}

// TestStaticVerifierCrossAlg asserts the per-key alg binding rejects a token
// signed with an algorithm declared only on a *different* key. Key A authorizes
// only RS256; the token is signed by key A using PS256 (an alg the RSA key can
// produce and that key B declares), so the global supported-alg set admits PS256
// at parse, but key A's binding rejects it.
func TestStaticVerifierCrossAlg(t *testing.T) {
	now := time.Now()
	keyA := newTestSigningKey(t)
	keyB := newTestSigningKey(t)
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: keyB.public, KeyID: "B", Algorithm: string(jose.PS256), Use: "sig"},
	)
	verifier, err := StaticVerifier(testIssuer, testClientID, jwks)
	if err != nil {
		t.Fatalf("StaticVerifier: %v", err)
	}

	raw := signWithKID(t, keyA.private, jose.PS256, "A", baseClaims("alice", testClientID, now))
	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatalf("Verify cross-alg token = nil error, want rejection")
	}

	// Control: the same key A signing under its declared RS256 still verifies, so the
	// rejection above is specifically the alg binding, not a broken key.
	ok := signWithKID(t, keyA.private, jose.RS256, "A", baseClaims("alice", testClientID, now))
	if _, err := verifier.Verify(context.Background(), ok); err != nil {
		t.Fatalf("Verify key A under its declared RS256: %v", err)
	}
}

// TestDiscoverVerifierKIDMismatch asserts the discovery path applies the same
// hardened kid-binding model as the static path (no divergence): a token whose kid
// names key B but is signed by key A is rejected, while the correctly-routed token
// verifies.
func TestDiscoverVerifierKIDMismatch(t *testing.T) {
	keyA := newTestSigningKey(t)
	keyB := newTestSigningKey(t)
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: keyB.public, KeyID: "B", Algorithm: string(jose.RS256), Use: "sig"},
	)
	srv := rawJWKSDiscoveryServer(t, string(jwks))

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL

	// Signed by key A but the header kid names key B → reject on the discovery path.
	bad := signWithKID(t, keyA.private, jose.RS256, "B", claims)
	if _, err := verifier.Verify(context.Background(), bad); err == nil {
		t.Fatalf("Verify kid-mismatch token via discovery = nil error, want rejection")
	}

	// Correctly routed to key A → verifies.
	good := signWithKID(t, keyA.private, jose.RS256, "A", claims)
	if _, err := verifier.Verify(context.Background(), good); err != nil {
		t.Fatalf("Verify correctly-routed token via discovery: %v", err)
	}
}

// mutableJWKSDiscoveryServer serves a discovery document (optionally advertising
// id_token_signing_alg_values_supported) plus a JWKS whose body the test can swap
// at runtime via the returned setter — modeling issuer key rotation.
func mutableJWKSDiscoveryServer(t *testing.T, signingAlgs []string, initialJWKS []byte) (*httptest.Server, func([]byte)) {
	t.Helper()
	var (
		mu   sync.Mutex
		body = initialJWKS
	)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks",
		}
		if len(signingAlgs) > 0 {
			doc["id_token_signing_alg_values_supported"] = signingAlgs
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		b := body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})

	return srv, func(b []byte) {
		mu.Lock()
		body = b
		mu.Unlock()
	}
}

// TestDiscoverVerifierRefreshOnRotation asserts the discovery verifier picks up an
// issuer key rotated in *after* it was built: a token bearing the new key's
// (initially unknown) kid triggers a refresh-on-unknown-kid refetch and verifies,
// without rebuilding the verifier — preserving go-oidc RemoteKeySet's behavior the
// snapshot would otherwise have lost.
func TestDiscoverVerifierRefreshOnRotation(t *testing.T) {
	keyA := newTestSigningKey(t)
	keyB := newTestSigningKey(t)
	jwksA := marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
	)
	srv, setJWKS := mutableJWKSDiscoveryServer(t, nil, jwksA)

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL

	// Sanity: key A (in the snapshot) verifies without any refresh.
	tokenA := signWithKID(t, keyA.private, jose.RS256, "A", claims)
	if _, err := verifier.Verify(context.Background(), tokenA); err != nil {
		t.Fatalf("Verify snapshotted key A: %v", err)
	}

	// The issuer rotates key B in (keeping A). A token bearing the new, snapshot-
	// unknown kid "B" must trigger a refresh-on-unknown-kid refetch and verify,
	// without rebuilding the verifier. (The rate-limit is still unconsumed here: key
	// A verified above without refreshing.)
	setJWKS(marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: keyB.public, KeyID: "B", Algorithm: string(jose.RS256), Use: "sig"},
	))
	tokenB := signWithKID(t, keyB.private, jose.RS256, "B", claims)
	if _, err := verifier.Verify(context.Background(), tokenB); err != nil {
		t.Fatalf("Verify rotated-in key B via refresh-on-unknown-kid: %v", err)
	}
}

// TestDiscoverVerifierRefreshNewAlg asserts refresh-on-unknown-kid is not blocked
// at parse when the rotated-in key uses a different (but issuer-advertised) alg than
// the keys present at build: the comprehensive outer/parse alg gate admits the
// PS256 token so it reaches the unknown-kid check and triggers the refetch, after
// which the per-key binding verifies it.
func TestDiscoverVerifierRefreshNewAlg(t *testing.T) {
	keyA := newTestSigningKey(t)
	keyB := newTestSigningKey(t) // RSA, will sign PS256
	jwksA := marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
	)
	// The issuer advertises both RS256 and PS256 up front.
	srv, setJWKS := mutableJWKSDiscoveryServer(t, []string{string(jose.RS256), string(jose.PS256)}, jwksA)

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL

	// Key A (RS256) verifies from the snapshot without a refresh.
	tokenA := signWithKID(t, keyA.private, jose.RS256, "A", claims)
	if _, err := verifier.Verify(context.Background(), tokenA); err != nil {
		t.Fatalf("Verify snapshotted key A: %v", err)
	}

	// Issuer rotates in key B declaring PS256. A PS256 token bearing the new kid must
	// refresh and verify — even though no PS256 key existed when the verifier was
	// built.
	setJWKS(marshalJWKS(t,
		jose.JSONWebKey{Key: keyA.public, KeyID: "A", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: keyB.public, KeyID: "B", Algorithm: string(jose.PS256), Use: "sig"},
	))
	tokenB := signWithKID(t, keyB.private, jose.PS256, "B", claims)
	if _, err := verifier.Verify(context.Background(), tokenB); err != nil {
		t.Fatalf("Verify rotated-in PS256 key B via refresh: %v", err)
	}
}

// TestDiscoverVerifierHonorsAdvertisedAlgs asserts the discovery path constrains
// each key to the issuer's advertised id_token_signing_alg_values_supported: an
// alg-less RSA key (which the key type alone would widen to the whole RS*/PS*
// family) is bound to only the advertised RS256, so a PS256 token is rejected while
// an RS256 token verifies.
func TestDiscoverVerifierHonorsAdvertisedAlgs(t *testing.T) {
	key := newTestSigningKey(t)
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: key.public, KeyID: "rsa-0", Use: "sig"}, // no Algorithm
	)
	srv, _ := mutableJWKSDiscoveryServer(t, []string{string(jose.RS256)}, jwks)

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL

	rs := signWithKID(t, key.private, jose.RS256, "rsa-0", claims)
	if _, err := verifier.Verify(context.Background(), rs); err != nil {
		t.Fatalf("Verify advertised RS256 token: %v", err)
	}

	ps := signWithKID(t, key.private, jose.PS256, "rsa-0", claims)
	if _, err := verifier.Verify(context.Background(), ps); err == nil {
		t.Fatalf("Verify unadvertised PS256 token = nil error, want rejection")
	}
}

// TestDiscoverVerifierAlgLessHonorsAdvertised asserts an alg-less JWK on the
// discovery path may verify any alg its key type can produce that the issuer
// advertises — not just the canonical one: an alg-less RSA key with the issuer
// advertising RS256+PS256 verifies both an RS256 and a PS256 token.
func TestDiscoverVerifierAlgLessHonorsAdvertised(t *testing.T) {
	key := newTestSigningKey(t) // RSA
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: key.public, KeyID: "rsa-0", Use: "sig"}, // no Algorithm
	)
	srv, _ := mutableJWKSDiscoveryServer(t, []string{string(jose.RS256), string(jose.PS256)}, jwks)

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL

	rs := signWithKID(t, key.private, jose.RS256, "rsa-0", claims)
	if _, err := verifier.Verify(context.Background(), rs); err != nil {
		t.Fatalf("Verify RS256 token (alg-less key, advertised RS256+PS256): %v", err)
	}
	ps := signWithKID(t, key.private, jose.PS256, "rsa-0", claims)
	if _, err := verifier.Verify(context.Background(), ps); err != nil {
		t.Fatalf("Verify PS256 token (alg-less key, advertised RS256+PS256): %v", err)
	}
}

// TestDiscoverVerifierUnadvertisedDefaultsRS256 asserts the discovery path holds an
// issuer that advertises no id_token_signing_alg_values_supported to RS256
// (go-oidc's conservative default): an RS256 key verifies while an ES256 key the
// same issuer publishes is excluded, so its ES256 token is rejected.
func TestDiscoverVerifierUnadvertisedDefaultsRS256(t *testing.T) {
	rsaKey := newTestSigningKey(t)
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	ecSigner, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: &jose.JSONWebKey{Key: ecPriv, KeyID: "es-0"}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building ES256 signer: %v", err)
	}

	// Issuer advertises NO id_token_signing_alg_values_supported, yet publishes both
	// an RS256 and an ES256 key.
	jwks := marshalJWKS(t,
		jose.JSONWebKey{Key: rsaKey.public, KeyID: "rsa-0", Algorithm: string(jose.RS256), Use: "sig"},
		jose.JSONWebKey{Key: ecPriv.Public(), KeyID: "es-0", Algorithm: string(jose.ES256), Use: "sig"},
	)
	srv, _ := mutableJWKSDiscoveryServer(t, nil, jwks)

	verifier, err := DiscoverVerifier(context.Background(), srv.URL, testClientID, nil)
	if err != nil {
		t.Fatalf("DiscoverVerifier: %v", err)
	}

	now := time.Now()
	claims := baseClaims("alice", testClientID, now)
	claims["iss"] = srv.URL

	// RS256 is the conservative default → verifies.
	rs := signWithKID(t, rsaKey.private, jose.RS256, "rsa-0", claims)
	if _, err := verifier.Verify(context.Background(), rs); err != nil {
		t.Fatalf("Verify RS256 token from unadvertised issuer: %v", err)
	}

	// ES256 is not advertised → the ES key was excluded → token rejected.
	claims["sub"] = "bob"
	esRaw, err := jwt.Signed(ecSigner).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("signing ES256 JWT: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), esRaw); err == nil {
		t.Fatalf("Verify ES256 token from unadvertised issuer = nil error, want rejection")
	}
}

// fakeVerifier is a TokenVerifier stub returning a fixed result, used by the
// Authenticator tests so they exercise the verify→map pipeline without a real
// OIDC token.
type fakeVerifier struct {
	claims map[string]any
	err    error
}

func (f *fakeVerifier) Verify(_ context.Context, _ string) (*VerifiedToken, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &VerifiedToken{Claims: f.claims}, nil
}

// TestAuthenticatorDefaultGroups exercises the Authenticator end to end with a
// fake verifier and the default group mapping: username from sub, groups from the
// groups claim.
func TestAuthenticatorDefaultGroups(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{
		"sub":    "alice",
		"groups": []any{"dev", "ops"},
	}}
	auth := NewAuthenticator(verifier, mapper, "sub", "", "", nil, nil)

	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "alice" {
		t.Errorf("username = %q, want alice", id.Username)
	}
	if want := []string{"dev", "ops"}; !equalStringSlice(id.Groups, want) {
		t.Errorf("groups = %v, want %v", id.Groups, want)
	}
}

// TestAuthenticatorCustomUsernameClaim asserts the username is read from the
// configured claim, not hard-coded to sub.
func TestAuthenticatorCustomUsernameClaim(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{
		"email":  "alice@example.com",
		"groups": []any{"dev"},
	}}
	auth := NewAuthenticator(verifier, mapper, "email", "", "", nil, nil)

	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "alice@example.com" {
		t.Errorf("username = %q, want alice@example.com", id.Username)
	}
}

// TestAuthenticatorUsernamePrefix asserts the configured username prefix is
// prepended to the username read from the username claim — the equivalent of the
// apiserver --oidc-username-prefix=oidc: flag. It is applied independently of the
// groups prefix and regardless of which username claim is configured.
func TestAuthenticatorUsernamePrefix(t *testing.T) {
	tests := []struct {
		name          string
		usernameClaim string
		prefix        string
		claims        map[string]any
		want          string
	}{
		{
			name:          "no prefix prepends nothing",
			usernameClaim: "sub",
			prefix:        "",
			claims:        map[string]any{"sub": "alice"},
			want:          "alice",
		},
		{
			name:          "oidc prefix is prepended",
			usernameClaim: "sub",
			prefix:        "oidc:",
			claims:        map[string]any{"sub": "alice"},
			want:          "oidc:alice",
		},
		{
			name:          "prefix applies to a custom username claim",
			usernameClaim: "email",
			prefix:        "oidc:",
			claims:        map[string]any{"email": "alice@example.com"},
			want:          "oidc:alice@example.com",
		},
		{
			name:          "prefix isolates a built-in system: username",
			usernameClaim: "sub",
			prefix:        "oidc:",
			claims:        map[string]any{"sub": "system:admin"},
			want:          "oidc:system:admin",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
			if err != nil {
				t.Fatalf("NewGroupMapper: %v", err)
			}
			auth := NewAuthenticator(&fakeVerifier{claims: tc.claims}, mapper, tc.usernameClaim, tc.prefix, "", nil, nil)

			id, err := auth.Authenticate(context.Background(), "raw-token")
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id.Username != tc.want {
				t.Errorf("username = %q, want %q", id.Username, tc.want)
			}
		})
	}
}

// TestAuthenticatorUsernameAndGroupsPrefixIndependent asserts the username prefix
// and the groups prefix are independent: each is applied on its own path, so a
// Backend may set one without the other.
func TestAuthenticatorUsernameAndGroupsPrefixIndependent(t *testing.T) {
	// Username prefixed, groups unprefixed.
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{
		"sub":    "alice",
		"groups": []any{"dev", "ops"},
	}}
	auth := NewAuthenticator(verifier, mapper, "sub", "oidc:", "", nil, nil)
	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "oidc:alice" {
		t.Errorf("username = %q, want oidc:alice", id.Username)
	}
	if want := []string{"dev", "ops"}; !equalStringSlice(id.Groups, want) {
		t.Errorf("groups = %v, want %v (groups must not inherit the username prefix)", id.Groups, want)
	}

	// Groups prefixed, username unprefixed.
	prefixedMapper, err := NewGroupMapper(DefaultGroupExpression("groups", "oidc:"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	auth = NewAuthenticator(verifier, prefixedMapper, "sub", "", "", nil, nil)
	id, err = auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "alice" {
		t.Errorf("username = %q, want alice (username must not inherit the groups prefix)", id.Username)
	}
	if want := []string{"oidc:dev", "oidc:ops"}; !equalStringSlice(id.Groups, want) {
		t.Errorf("groups = %v, want %v", id.Groups, want)
	}
}

// TestAuthenticatorMissingUsernameClaim asserts a token missing the username
// claim is rejected (the proxy cannot impersonate without a username).
func TestAuthenticatorMissingUsernameClaim(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{"groups": []any{"dev"}}}
	auth := NewAuthenticator(verifier, mapper, "sub", "", "", nil, nil)

	if _, err := auth.Authenticate(context.Background(), "raw-token"); err == nil {
		t.Fatalf("Authenticate with missing username claim = nil error, want rejection")
	}
}

// TestAuthenticatorMissingGroupsClaimIsEmpty asserts a token with no groups claim
// authenticates with an empty group set (not an error).
func TestAuthenticatorMissingGroupsClaimIsEmpty(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{"sub": "alice"}}
	auth := NewAuthenticator(verifier, mapper, "sub", "", "", nil, nil)

	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(id.Groups) != 0 {
		t.Errorf("groups = %v, want empty", id.Groups)
	}
}

// TestAuthenticatorVerifyError asserts a verification failure propagates from
// Authenticate rather than being swallowed.
func TestAuthenticatorVerifyError(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{err: context.DeadlineExceeded}
	auth := NewAuthenticator(verifier, mapper, "sub", "", "", nil, nil)

	if _, err := auth.Authenticate(context.Background(), "raw-token"); err == nil {
		t.Fatalf("Authenticate with a verifier error = nil error, want propagation")
	}
}

// TestAuthenticatorUIDClaim asserts that when a UID claim is configured the UID is
// read from it into Identity.UID, and that an unconfigured UID claim leaves the UID
// empty (the backward-compatible default).
func TestAuthenticatorUIDClaim(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	claims := map[string]any{"sub": "uid-123", "email": "alice@example.com"}

	// UID claim configured: Identity.UID is the sub claim.
	auth := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "email", "", "sub", nil, nil)
	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.UID != "uid-123" {
		t.Errorf("uid = %q, want uid-123", id.UID)
	}
	if id.Username != "alice@example.com" {
		t.Errorf("username = %q, want alice@example.com", id.Username)
	}

	// No UID claim configured: Identity.UID is empty.
	authNoUID := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "email", "", "", nil, nil)
	idNoUID, err := authNoUID.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if idNoUID.UID != "" {
		t.Errorf("uid = %q, want empty when no uid claim configured", idNoUID.UID)
	}
}

// TestAuthenticatorUIDClaimMissingDenies asserts that a configured UID claim that is
// absent, empty, or a non-string on the token is an error (fail-closed): a stable
// UID the operator asked for must not be silently dropped.
func TestAuthenticatorUIDClaimMissingDenies(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	cases := map[string]map[string]any{
		"absent":     {"sub": "alice"},
		"empty":      {"sub": "alice", "uid": ""},
		"non-string": {"sub": "alice", "uid": []any{"a", "b"}},
	}
	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			auth := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "sub", "", "uid", nil, nil)
			if _, err := auth.Authenticate(context.Background(), "raw-token"); err == nil {
				t.Fatalf("Authenticate with %s uid claim = nil error, want rejection", name)
			}
		})
	}
}

// TestAuthenticatorExtraMappings asserts the extra mappings resolve each configured
// claim into Identity.Extra, that an absent claim is skipped (not an error), and
// that a present-but-non-string claim fails closed.
func TestAuthenticatorExtraMappings(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}

	// Three mappings: one claim present with a value, one absent, one present but an
	// empty string. The valued one is mapped, the absent one is skipped (no entry, no
	// error), and the present-empty one is emitted verbatim (absent vs present is the
	// contract — a present empty value is not silently dropped).
	extra := []authenticatorv1alpha1.ExtraMapping{
		{Key: "email", ValueClaim: "email"},
		{Key: "tenant", ValueClaim: "tenant_id"},
		{Key: "note", ValueClaim: "note"},
	}
	claims := map[string]any{"sub": "alice", "email": "alice@example.com", "note": ""}
	auth := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "sub", "", "", extra, nil)
	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got, want := id.Extra["email"], "alice@example.com"; got != want {
		t.Errorf("extra[email] = %q, want %q", got, want)
	}
	if _, ok := id.Extra["tenant"]; ok {
		t.Errorf("extra[tenant] present, want skipped (claim absent)")
	}
	if v, ok := id.Extra["note"]; !ok || v != "" {
		t.Errorf("extra[note] = (%q, %v), want (\"\", true) — a present empty claim is emitted, not skipped", v, ok)
	}

	// No extra configured: Identity.Extra is nil.
	authNone := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "sub", "", "", nil, nil)
	idNone, err := authNone.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if idNone.Extra != nil {
		t.Errorf("extra = %v, want nil when no extra configured", idNone.Extra)
	}

	// A present-but-non-string claim is a misconfiguration and fails closed.
	badExtra := []authenticatorv1alpha1.ExtraMapping{{Key: "groups", ValueClaim: "groups"}}
	authBad := NewAuthenticator(
		&fakeVerifier{claims: map[string]any{"sub": "alice", "groups": []any{"dev", "ops"}}},
		mapper, "sub", "", "", badExtra, nil)
	if _, err := authBad.Authenticate(context.Background(), "raw-token"); err == nil {
		t.Fatalf("Authenticate with a non-string extra claim = nil error, want rejection")
	}
}

// TestAuthenticatorActorExtraMappings mirrors TestAuthenticatorExtraMappings for
// spec.impersonation.actorExtra: the Authenticator resolves actor-extra claim
// mappings into Identity.ActorExtra with the same absent/present/non-string
// semantics as spec.oidc.extra, keeping ActorExtra separate from Extra and on a
// disjoint key namespace (HOL-1432).
func TestAuthenticatorActorExtraMappings(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}

	// One oidc.extra mapping and three disjoint actorExtra mappings: one claim
	// present with a value, one absent, one present but an empty string. The valued
	// one is mapped, the absent one is skipped, the present-empty one is emitted
	// verbatim — exactly the extra contract, applied to the actorExtra namespace.
	extra := []authenticatorv1alpha1.ExtraMapping{{Key: "email", ValueClaim: "email"}}
	actorExtra := []authenticatorv1alpha1.ExtraMapping{
		{Key: "actor-email", ValueClaim: "actor_email"},
		{Key: "actor-tenant", ValueClaim: "actor_tenant_id"},
		{Key: "actor-note", ValueClaim: "actor_note"},
	}
	claims := map[string]any{
		"sub":         "alice",
		"email":       "alice@example.com",
		"actor_email": "bob@example.com",
		"actor_note":  "",
	}
	auth := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "sub", "", "", extra, actorExtra)
	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// oidc.extra and actorExtra are resolved into separate maps.
	if got, want := id.Extra["email"], "alice@example.com"; got != want {
		t.Errorf("extra[email] = %q, want %q", got, want)
	}
	if _, ok := id.Extra["actor-email"]; ok {
		t.Errorf("extra must not carry actorExtra keys; extra = %v", id.Extra)
	}
	if got, want := id.ActorExtra["actor-email"], "bob@example.com"; got != want {
		t.Errorf("actorExtra[actor-email] = %q, want %q", got, want)
	}
	if _, ok := id.ActorExtra["actor-tenant"]; ok {
		t.Errorf("actorExtra[actor-tenant] present, want skipped (claim absent)")
	}
	if v, ok := id.ActorExtra["actor-note"]; !ok || v != "" {
		t.Errorf("actorExtra[actor-note] = (%q, %v), want (\"\", true) — a present empty claim is emitted, not skipped", v, ok)
	}

	// No actorExtra configured: Identity.ActorExtra is nil.
	authNone := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "sub", "", "", extra, nil)
	idNone, err := authNone.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if idNone.ActorExtra != nil {
		t.Errorf("actorExtra = %v, want nil when no actorExtra configured", idNone.ActorExtra)
	}

	// A present-but-non-string actorExtra claim is a misconfiguration and fails closed.
	badActorExtra := []authenticatorv1alpha1.ExtraMapping{{Key: "actor-groups", ValueClaim: "groups"}}
	authBad := NewAuthenticator(
		&fakeVerifier{claims: map[string]any{"sub": "alice", "groups": []any{"dev", "ops"}}},
		mapper, "sub", "", "", nil, badActorExtra)
	if _, err := authBad.Authenticate(context.Background(), "raw-token"); err == nil {
		t.Fatalf("Authenticate with a non-string actorExtra claim = nil error, want rejection")
	}
}

// equalStringSlice reports element-wise slice equality.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

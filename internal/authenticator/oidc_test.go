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
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
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
// token verifies, rather than failing under go-oidc's RS256-only default (Codex
// round 1).
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
// honored, and RS256 is retained for the alg-less RSA key (go-oidc's default)
// rather than being dropped when another key carries an alg (Codex round 2).
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
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{
		"sub":    "alice",
		"groups": []any{"dev", "ops"},
	}}
	auth := NewAuthenticator(verifier, mapper, "sub")

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
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{
		"email":  "alice@example.com",
		"groups": []any{"dev"},
	}}
	auth := NewAuthenticator(verifier, mapper, "email")

	id, err := auth.Authenticate(context.Background(), "raw-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "alice@example.com" {
		t.Errorf("username = %q, want alice@example.com", id.Username)
	}
}

// TestAuthenticatorMissingUsernameClaim asserts a token missing the username
// claim is rejected (the proxy cannot impersonate without a username).
func TestAuthenticatorMissingUsernameClaim(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{"groups": []any{"dev"}}}
	auth := NewAuthenticator(verifier, mapper, "sub")

	if _, err := auth.Authenticate(context.Background(), "raw-token"); err == nil {
		t.Fatalf("Authenticate with missing username claim = nil error, want rejection")
	}
}

// TestAuthenticatorMissingGroupsClaimIsEmpty asserts a token with no groups claim
// authenticates with an empty group set (not an error).
func TestAuthenticatorMissingGroupsClaimIsEmpty(t *testing.T) {
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{claims: map[string]any{"sub": "alice"}}
	auth := NewAuthenticator(verifier, mapper, "sub")

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
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	verifier := &fakeVerifier{err: context.DeadlineExceeded}
	auth := NewAuthenticator(verifier, mapper, "sub")

	if _, err := auth.Authenticate(context.Background(), "raw-token"); err == nil {
		t.Fatalf("Authenticate with a verifier error = nil error, want propagation")
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

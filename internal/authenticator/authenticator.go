package authenticator

import (
	"context"
	"fmt"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
)

// Identity is the resolved Kubernetes identity for a validated token: the
// username the proxy impersonates, the groups it impersonates alongside it, and
// the optional UID and extra fields it carries.
type Identity struct {
	// Username is the Kubernetes username, read from the configured username claim
	// (spec.oidc.usernameClaim, default "sub") and prefixed with the configured
	// username prefix (spec.oidc.usernamePrefix) when one is set.
	Username string
	// Groups are the Kubernetes groups, produced by evaluating the backend's
	// compiled group-mapping CEL expression over the validated claims. It may be
	// empty (the user is in no extra groups).
	Groups []string
	// UID is the Kubernetes user UID, read from the configured UID claim
	// (spec.oidc.uidClaim) when one is set. Empty when no UID claim is configured;
	// the server emits an Impersonate-Uid header only for a non-empty UID.
	UID string
	// Extra maps each configured extra key (spec.oidc.extra[].key) to the single
	// string value read from its claim, which the server emits as an
	// Impersonate-Extra-<key> header. It is nil/empty when no extra mappings are
	// configured or none of their claims were present on the token.
	Extra map[string]string
	// ImpersonationExtra maps each configured spec.impersonation.extra key to the
	// single string value read from its claim, describing the **actor** identity in
	// delegated impersonation. It is resolved only for authorized delegated
	// requests; self mode ignores spec.impersonation.extra entirely.
	ImpersonationExtra map[string]string
	claims             map[string]any
}

// Authenticator validates an OIDC bearer token for a single Backend and resolves
// the Kubernetes identity it represents. It pairs a TokenVerifier (signature +
// iss/aud/exp/nbf validation) with a GroupMapper (claims → groups) and the
// configured username claim. One Authenticator is built per Backend by the
// reconciler and stored in the Store's Entry; it is safe for concurrent use.
type Authenticator struct {
	// verifier validates the raw token (signature against the discovered JWKS,
	// iss, aud == clientID, exp/nbf).
	verifier TokenVerifier
	// mapper evaluates the compiled group-mapping CEL expression over the
	// validated claims.
	mapper *GroupMapper
	// usernameClaim is the token claim the username is read from.
	usernameClaim string
	// usernamePrefix is prepended to the username read from usernameClaim before it
	// is impersonated. Empty prepends nothing (the backward-compatible default).
	usernamePrefix string
	// uidClaim is the token claim the UID is read from (spec.oidc.uidClaim). Empty
	// disables UID impersonation (no Impersonate-Uid header).
	uidClaim string
	// extra is the set of claim→extra-key mappings (spec.oidc.extra) the
	// authenticator resolves into Identity.Extra. Nil/empty emits no extra headers.
	extra []authenticatorv1alpha1.ExtraMapping
	// impersonationExtra is the set of claim→extra-key mappings
	// (spec.impersonation.extra) describing the actor identity in delegated
	// impersonation, resolved into Identity.ImpersonationExtra only on the delegated
	// branch. Nil/empty resolves no actor extras.
	impersonationExtra []authenticatorv1alpha1.ExtraMapping
}

// NewAuthenticator constructs an Authenticator from a verifier, a compiled group
// mapper, the username claim, the username prefix, the UID claim, the oidc.extra
// claim mappings, and the impersonation.extra claim mappings. usernameClaim
// defaults to "sub" when empty, matching the OIDC convention and the Backend CRD
// default. usernamePrefix is prepended to the resolved username; an empty prefix
// prepends nothing. An empty uidClaim or nil extra/impersonation extra emits no UID / no
// extra / no impersonation extra fields (the backward-compatible default).
func NewAuthenticator(verifier TokenVerifier, mapper *GroupMapper, usernameClaim, usernamePrefix, uidClaim string, extra, impersonationExtra []authenticatorv1alpha1.ExtraMapping) *Authenticator {
	if usernameClaim == "" {
		usernameClaim = "sub"
	}
	return &Authenticator{
		verifier:           verifier,
		mapper:             mapper,
		usernameClaim:      usernameClaim,
		usernamePrefix:     usernamePrefix,
		uidClaim:           uidClaim,
		extra:              extra,
		impersonationExtra: impersonationExtra,
	}
}

// Authenticate validates rawToken and returns the resolved Kubernetes identity.
// It first verifies the token (a failed signature, wrong audience, expired, or
// not-yet-valid token returns an error), then reads the username from the
// configured username claim (prepending the configured username prefix) and maps
// the claims to groups via the compiled CEL expression.
//
// A missing or non-string username claim is an error: the proxy cannot
// impersonate a request without a username. Missing groups, by contrast, are not
// an error — the default mapping over a token with no groups claim yields no
// groups.
func (a *Authenticator) Authenticate(ctx context.Context, rawToken string) (*Identity, error) {
	verified, err := a.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}

	username, err := stringClaim(verified.Claims, a.usernameClaim)
	if err != nil {
		return nil, err
	}

	groups, err := a.mapper.Groups(verified.Claims)
	if err != nil {
		return nil, fmt.Errorf("mapping token claims to groups: %w", err)
	}

	identity := &Identity{Username: a.usernamePrefix + username, Groups: groups, claims: verified.Claims}

	// UID is opt-in. When a UID claim is configured it MUST resolve to a non-empty
	// string: the operator asked for a stable UID (e.g. sub) for audit, so a token
	// missing it is rejected rather than silently impersonated without one.
	if a.uidClaim != "" {
		uid, err := stringClaim(verified.Claims, a.uidClaim)
		if err != nil {
			return nil, fmt.Errorf("reading uid claim: %w", err)
		}
		identity.UID = uid
	}

	// Extra fields are optional context: a claim absent from this token is skipped,
	// but a claim present as a non-string is a misconfiguration and fails closed.
	if identity.Extra, err = resolveExtra(verified.Claims, a.extra, "extra"); err != nil {
		return nil, err
	}
	return identity, nil
}

// ResolveImpersonationExtra resolves spec.impersonation.extra against the
// already-verified token claims for identity. It is called only after the Check
// path has selected and authorized delegated mode, so a misconfigured
// spec.impersonation.extra denies delegated requests fail-closed without affecting
// self-mode requests.
func (a *Authenticator) ResolveImpersonationExtra(identity *Identity) (map[string]string, error) {
	if identity == nil {
		return nil, fmt.Errorf("identity is nil")
	}
	return resolveExtra(identity.claims, a.impersonationExtra, "impersonation.extra")
}

// resolveExtra resolves a set of claim→extra-key mappings against the validated
// claims into a map, using the optional-string-claim semantics shared by
// spec.oidc.extra and spec.impersonation.extra: a claim absent from the token
// (or carrying JSON null) is skipped, a present string is emitted verbatim
// (including an empty string), and a present non-string fails closed. label names
// the mapping group in error messages ("extra" or "impersonation.extra"). It returns a nil
// map when no mapping's claim was present.
func resolveExtra(claims map[string]any, mappings []authenticatorv1alpha1.ExtraMapping, label string) (map[string]string, error) {
	var out map[string]string
	for _, m := range mappings {
		value, ok, err := optionalStringClaim(claims, m.ValueClaim)
		if err != nil {
			return nil, fmt.Errorf("reading %s %q claim: %w", label, m.Key, err)
		}
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(mappings))
		}
		out[m.Key] = value
	}
	return out, nil
}

// stringClaim reads claim from claims as a non-empty string. A missing claim, a
// nil value, an empty string, or a non-string value is an error so a token that
// cannot identify a user is rejected rather than impersonating an empty username.
func stringClaim(claims map[string]any, claim string) (string, error) {
	raw, ok := claims[claim]
	if !ok || raw == nil {
		return "", fmt.Errorf("token is missing the %q claim", claim)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("token claim %q is not a string (got %T)", claim, raw)
	}
	if s == "" {
		return "", fmt.Errorf("token claim %q is empty", claim)
	}
	return s, nil
}

// optionalStringClaim reads claim from claims as a string for an optional mapping
// (an impersonation extra). It distinguishes a claim that is absent from one that is
// present: a token missing the claim — or carrying it as JSON null — returns
// ("", false, nil) so the caller skips the entry (mirroring the "missing groups
// claim → no groups" behavior), while a claim present as a string is returned with
// ok=true and emitted verbatim, including an empty string. The absent-vs-present
// distinction is the whole contract, so a present empty value is NOT silently
// dropped — it is emitted as an empty extra value. A present non-string value (a
// list, object, number, or bool) is an error so a misconfigured mapping pointing at
// a non-string claim fails closed rather than emitting a malformed extra value.
func optionalStringClaim(claims map[string]any, claim string) (string, bool, error) {
	raw, ok := claims[claim]
	if !ok || raw == nil {
		return "", false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("token claim %q is not a string (got %T)", claim, raw)
	}
	return s, true, nil
}

package authenticator

import (
	"context"
	"fmt"
)

// Identity is the resolved Kubernetes identity for a validated token: the
// username the proxy impersonates and the groups it impersonates alongside it.
type Identity struct {
	// Username is the Kubernetes username, read from the configured username claim
	// (spec.oidc.usernameClaim, default "sub") and prefixed with the configured
	// username prefix (spec.oidc.usernamePrefix) when one is set.
	Username string
	// Groups are the Kubernetes groups, produced by evaluating the backend's
	// compiled group-mapping CEL expression over the validated claims. It may be
	// empty (the user is in no extra groups).
	Groups []string
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
}

// NewAuthenticator constructs an Authenticator from a verifier, a compiled group
// mapper, the username claim, and the username prefix. usernameClaim defaults to
// "sub" when empty, matching the OIDC convention and the Backend CRD default.
// usernamePrefix is prepended to the resolved username; an empty prefix prepends
// nothing.
func NewAuthenticator(verifier TokenVerifier, mapper *GroupMapper, usernameClaim, usernamePrefix string) *Authenticator {
	if usernameClaim == "" {
		usernameClaim = "sub"
	}
	return &Authenticator{
		verifier:       verifier,
		mapper:         mapper,
		usernameClaim:  usernameClaim,
		usernamePrefix: usernamePrefix,
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

	return &Identity{Username: a.usernamePrefix + username, Groups: groups}, nil
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

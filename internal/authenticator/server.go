// Package authenticator implements the holos-authenticator service (ADR-23): an
// Istio/Envoy gRPC external authorizer (envoy.service.auth.v3.Authorization)
// that validates an OIDC identity token, maps the token's claims to Kubernetes
// groups via a CEL expression, and returns Kubernetes impersonation headers so
// Envoy can forward an authenticated request to a backend API server with no
// other reverse proxy in the path.
//
// The ext_authz Check (HOL-1388) routes each request to a Backend by Host,
// validates the caller's OIDC bearer token, and on success returns an OK response
// that sets Kubernetes impersonation headers (Impersonate-User, plus a single
// comma-joined groups header — by default X-Impersonate-Groups, configurable via
// --impersonate-groups-header — paired with a Lua split filter, see okResponse),
// injects the backend's privileged
// credential as the upstream Authorization, and removes the caller's original
// token — so Envoy forwards the request straight to the API server. Any failure
// (unknown host, missing/invalid token, credential read failure, internal error)
// returns a fail-closed Denied 401/403; the server never returns OK on error. The
// GRPCServer manager.Runnable runs Check on the manager's lifecycle and
// leader-election context.
//
// The groups header is deliberately NOT Impersonate-Group: Envoy's ext_authz
// path classifies an authorizer-returned header carrying the deprecated
// append=true bool into headers_to_append, which Envoy applies with appendCopy
// ONLY IF the request already carries that header — and the inbound request never
// carries Impersonate-Group (it is rejected fail-closed), so an appended
// Impersonate-Group would be silently dropped before reaching the API server
// (HOL-1416). Emitting the groups as a single overwrite (set / setCopy, which
// adds the header unconditionally) into a distinct non-Impersonate-* header
// sidesteps that drop entirely; the paired Lua split filter, running after
// ext_authz, unpacks the comma list into one Impersonate-Group line per group for
// the API server.
package authenticator

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kubernetes impersonation and authorization header names. The API server matches
// these case-insensitively; the canonical forms are used so the configured value
// is legible. HTTP/2 lowercases header names on the wire, but Envoy applies the
// configured value verbatim, so these are set as the API server expects.
const (
	// headerAuthorization is the bearer-token header: the caller's token is read
	// from it and the backend's impersonator token is written back to it.
	headerAuthorization = "Authorization"
	// headerImpersonateUser sets the Kubernetes user the request is impersonated as.
	headerImpersonateUser = "Impersonate-User"
	// headerImpersonateUID sets the Kubernetes user UID the request is impersonated
	// as (Impersonate-Uid). Like Impersonate-User it is a single value, so it is set
	// directly under its Impersonate-* name with the overwrite/set action — no
	// comma-join + Lua split helper is needed (that exists only for the multi-valued
	// groups header the ext_authz append path would otherwise drop, HOL-1416).
	headerImpersonateUID = "Impersonate-Uid"
	// headerImpersonateExtraPrefix is the prefix of a Kubernetes impersonation extra
	// header; the per-entry extra key is appended verbatim to form
	// Impersonate-Extra-<key>. Each extra is single-valued in this phase, so each is
	// emitted as one overwrite/set header (no append, no split).
	headerImpersonateExtraPrefix = "Impersonate-Extra-"
	// defaultGroupsHeader is the default name of the single comma-joined groups
	// header the authorizer writes the mapped Kubernetes groups into (one CSV
	// value, e.g. "oidc:dev,oidc:ops"), with an overwrite/set action so Envoy adds
	// it unconditionally rather than dropping it (HOL-1416). It is deliberately a
	// non-Impersonate-* header so the inbound-rejection guard and the reject Lua
	// filter can refuse a client-supplied copy without colliding with the real
	// Impersonate-Group the split Lua filter ultimately emits. It is configurable
	// per deployment via --impersonate-groups-header (CheckServer.groupsHeader).
	defaultGroupsHeader = "x-impersonate-groups"
	// headerWWWAuthenticate is returned on a missing-token 401 per RFC 7235.
	headerWWWAuthenticate = "WWW-Authenticate"
	// impersonatePrefix is the lowercase prefix every Kubernetes impersonation
	// header shares (Impersonate-User, Impersonate-Group, Impersonate-Uid,
	// Impersonate-Extra-*). A caller-supplied header with this prefix is a
	// smuggling attempt and is denied fail-closed.
	impersonatePrefix = "impersonate-"
	// bearerPrefix is the case-insensitive scheme prefix on an Authorization
	// header carrying a bearer token.
	bearerPrefix = "bearer "
	// wwwAuthenticateBearer is the challenge returned with a missing-token 401.
	wwwAuthenticateBearer = "Bearer"
)

// CheckServer implements the Envoy ext_authz Authorization service
// (envoy.service.auth.v3.Authorization): it routes each request to a Backend by
// Host, validates the caller's OIDC bearer token, resolves the backend's
// privileged credential, and returns Kubernetes impersonation headers on success
// or a fail-closed Denied response on any failure (ADR-23).
type CheckServer struct {
	// UnimplementedAuthorizationServer provides forward-compatible defaults for
	// any service methods added to the proto in the future, per the gRPC
	// recommendation for server implementations.
	authv3.UnimplementedAuthorizationServer

	// store is the shared host-keyed registry of ready backends, written by the
	// BackendReconciler and read here by the request's :authority/Host to resolve
	// the backend's Authenticator and credentialsSecretRef. It is injected so the
	// reconciler and this server share one instance.
	store *Store

	// reader resolves the backend's privileged impersonator credential Secret. It
	// is the manager's APIReader (a non-caching reader) so a credential rotation is
	// seen immediately and no cluster-wide Secret informer cache is required.
	reader client.Reader

	// tokenManager mints, caches, and rotates ServiceAccount tokens via the
	// TokenRequest API for backends whose Entry carries a ServiceAccountRef
	// (HOL-1400). It holds the manager's writable client because TokenRequest is a
	// create. It may be nil when no writable client was wired (e.g. Secret-only test
	// servers); a nil manager with a ServiceAccount-backed backend is handled
	// fail-closed in resolveCredential.
	tokenManager *TokenManager

	// namespace is the authorizer's own namespace, where every backend credential
	// Secret and impersonator ServiceAccount lives (the Backend's
	// spec.credentialsSecretRef / spec.serviceAccountRef name only the object, not a
	// namespace).
	namespace string

	// groupsHeader is the name of the single comma-joined groups header the OK
	// response writes the mapped groups into (HOL-1416), set from the
	// --impersonate-groups-header flag. An empty value resolves to
	// defaultGroupsHeader ("x-impersonate-groups") via groupsHeaderName, so a
	// zero-value CheckServer behaves as the documented default.
	groupsHeader string

	log logr.Logger
}

// NewCheckServer returns a CheckServer that resolves backends from store, reads
// impersonator credential Secrets from namespace via reader (the manager's
// APIReader), mints ServiceAccount tokens via writer (the manager's writable
// client, used for the TokenRequest create), writes the mapped groups into the
// groupsHeader header (empty resolves to defaultGroupsHeader), and logs through
// log. store is the same registry the BackendReconciler writes, so the Check path
// sees backends as they become ready. writer may be nil for a Secret-only server
// (e.g. in tests); a backend that resolves its credential from a ServiceAccount
// then denies fail-closed.
func NewCheckServer(store *Store, reader client.Reader, writer client.Client, namespace, groupsHeader string, log logr.Logger) *CheckServer {
	var tm *TokenManager
	if writer != nil {
		tm = NewTokenManager(writer, namespace)
	}
	return &CheckServer{store: store, reader: reader, tokenManager: tm, namespace: namespace, groupsHeader: groupsHeader, log: log}
}

// groupsHeaderName returns the configured groups header name, defaulting to
// defaultGroupsHeader ("x-impersonate-groups") when groupsHeader is empty so a
// zero-value or test CheckServer behaves as the documented default.
func (s *CheckServer) groupsHeaderName() string {
	if s.groupsHeader == "" {
		return defaultGroupsHeader
	}
	return s.groupsHeader
}

// ValidateGroupsHeader canonicalizes name to lowercase and validates it is usable
// as the groups header the OK response writes (HOL-1416). The name MUST be a
// non-empty, valid HTTP header field name (RFC 7230 token characters only) and MUST
// be neither the Authorization header nor an Impersonate-* header: configuring
// "Authorization" would treat the caller's required bearer token as a smuggled
// groups header and deny every request, while "Impersonate-Group" (or any
// Impersonate-* name) would collide with the Kubernetes impersonation header space
// the reject/split Lua filters govern and could push the comma-joined value
// straight at the API server. It returns the canonical lowercase name on success.
// The caller (main) validates the --impersonate-groups-header flag with this before
// constructing the CheckServer and exits on error.
func ValidateGroupsHeader(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("groups header name must not be empty")
	}
	for _, r := range name {
		if !isHeaderTokenChar(r) {
			return "", fmt.Errorf("groups header name %q contains an invalid character %q", name, r)
		}
	}
	lower := strings.ToLower(name)
	if lower == strings.ToLower(headerAuthorization) {
		return "", fmt.Errorf("groups header name must not be the Authorization header")
	}
	if strings.HasPrefix(lower, impersonatePrefix) {
		return "", fmt.Errorf("groups header name %q must not be an Impersonate-* header (it would collide with the Kubernetes impersonation headers the reject/split filters govern)", name)
	}
	return lower, nil
}

// ValidateExtraKey reports whether key is usable as the suffix of an
// Impersonate-Extra-<key> impersonation header. The authorizer emits the key
// verbatim, but the Kubernetes API server derives the extra-map key from the header
// name by **lowercasing and percent-unescaping** it, so the key must be canonical —
// already lowercase and free of '%' — for the emitted header to round-trip to the
// extra key the operator wrote. Specifically it must be a non-empty string of HTTP
// header field-name token characters (RFC 7230) restricted to lowercase: ASCII
// lowercase letters, digits, and the token punctuation `!#$&'*+-.^_`+"`"+`|~`
// (the RFC 7230 set minus '%'). This keeps the CRD's case-sensitive listMapKey
// uniqueness aligned with the API server's lowercased keys (so `Email` and `email`
// cannot both be admitted to collide) and prevents a '%' from percent-decoding into
// a different key (e.g. `tenant%2fid` → `tenant/id`). The BackendReconciler
// validates every spec.oidc.extra[].key with this and rejects the Backend
// (Accepted=False) on failure, mirroring how ValidateGroupsHeader guards the
// --impersonate-groups-header flag.
func ValidateExtraKey(key string) error {
	if key == "" {
		return fmt.Errorf("extra key must not be empty")
	}
	for _, r := range key {
		if !isExtraKeyChar(r) {
			return fmt.Errorf("extra key %q contains an invalid character %q (keys must be lowercase HTTP header tokens without %%)", key, r)
		}
	}
	return nil
}

// isExtraKeyChar reports whether r is a valid character in a canonical extra key:
// an HTTP header field-name token character (RFC 7230) restricted to lowercase and
// excluding '%'. Uppercase letters are rejected because the API server lowercases
// the key (so an uppercase key would never round-trip to itself), and '%' is
// rejected because the API server percent-unescapes the key (so a '%' would decode
// into a different key). See ValidateExtraKey.
func isExtraKeyChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case strings.ContainsRune("!#$&'*+-.^_`|~", r): // RFC 7230 token punctuation minus '%'
		return true
	default:
		return false
	}
}

// isHeaderTokenChar reports whether r is a valid HTTP header field-name character
// (RFC 7230 token: ALPHA / DIGIT / a fixed set of punctuation), so an operator
// cannot configure a name with a space, colon, or other separator that would not be
// a usable header.
func isHeaderTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		return true
	default:
		return false
	}
}

// Check implements envoy.service.auth.v3.Authorization. It executes the full
// authorization flow:
//
//  1. Resolve the Backend from the request :authority/Host; unknown host →
//     Denied 403.
//  2. Extract the bearer token from the Authorization header; missing → Denied
//     401 with a WWW-Authenticate challenge.
//  3. Validate the OIDC token and map groups via the backend's Authenticator;
//     invalid → Denied 401.
//  4. Resolve the backend's privileged impersonator credential (a minted
//     ServiceAccount token or the credential Secret); resolution failure →
//     Denied 403.
//  5. Return OK setting Impersonate-User and a single comma-joined groups header
//     (the configured groupsHeader, default X-Impersonate-Groups) via overwrite,
//     paired with a Lua split filter that unpacks it into one Impersonate-Group
//     line per group — see okResponse — with the impersonator token as the
//     upstream Authorization, removing the caller's original Authorization. Groups
//     unsafe under that comma-joined encoding (a comma or surrounding whitespace)
//     are denied fail-closed before this step.
//
// Every failure path — including any internal error — returns a Denied response,
// never OK, so the authorizer fails closed. The gRPC error return is always nil:
// the allow/deny decision is carried in the CheckResponse, not the RPC status,
// which is the ext_authz contract Envoy expects.
func (s *CheckServer) Check(ctx context.Context, req *authv3.CheckRequest) (resp *authv3.CheckResponse, err error) {
	http := req.GetAttributes().GetRequest().GetHttp()
	host := http.GetHost()

	// Log every header the authorizer returns to Envoy at a single exit point,
	// whichever branch produced the response (HOL-1415). The deferred call sees the
	// named resp return, so it covers the OK path and all fail-closed denials
	// without threading logging through each return. It is a no-op unless V(1) debug
	// logging is enabled, so it adds no cost on the normal path.
	defer func() { s.logResponseHeaders(host, resp) }()

	// 1. Route by Host to a ready backend. An unknown host is a 403 (the request
	// reached us but matches no configured backend) — distinct from an
	// authentication failure on a known host.
	entry, ok := s.store.Get(host)
	if !ok {
		s.log.V(1).Info("denying request for unknown host", "host", host)
		return deniedResponse(typev3.StatusCode_Forbidden, "unknown host", nil), nil
	}

	// 2. Sanitize inbound impersonation headers (mandatory, ADR-23). The authorizer
	// injects the backend's privileged credential downstream, so any client-supplied
	// Impersonate-* header would otherwise be impersonated at that privilege — a
	// confused-deputy / header-smuggling hole (e.g. a smuggled
	// Impersonate-Group: system:masters). Because Envoy does not document a
	// guaranteed apply order between the OkResponse's header sets and
	// headers_to_remove, we fail closed and DENY any request carrying an
	// Impersonate-* header rather than relying on a scrub the upstream might race —
	// ADR-23: "denied rather than silently scrubbed if there is any doubt the
	// upstream would see the client's version." The caller's own Authorization
	// (their OIDC token) is expected and is overwritten in place below, so it is not
	// part of this denial.
	if name, ok := firstImpersonationHeader(http.GetHeaders()); ok {
		s.log.V(1).Info("denying request carrying inbound impersonation header", "host", host, "header", name)
		return deniedResponse(typev3.StatusCode_Forbidden, "inbound impersonation header not allowed", nil), nil
	}

	// 2b. Reject an inbound copy of the configured groups header too (HOL-1416).
	// The groups header (default X-Impersonate-Groups) is NOT Impersonate-* prefixed,
	// so the prefix guard above does not cover it; but the Lua split filter turns its
	// comma list into Impersonate-Group lines for the API server, so a client-supplied
	// value carrying it (e.g. X-Impersonate-Groups: system:masters) would be smuggled
	// the same way an Impersonate-Group would. The OK path overwrites the header on a
	// request that maps to at least one group, but a request mapping to zero groups
	// emits no groups header at all — leaving a smuggled value intact — so deny
	// fail-closed up front rather than relying on the overwrite. This mirrors the
	// reject Lua filter (which also runs before ext_authz) as defense in depth.
	if _, ok := http.GetHeaders()[strings.ToLower(s.groupsHeaderName())]; ok {
		s.log.V(1).Info("denying request carrying inbound groups header", "host", host, "header", s.groupsHeaderName())
		return deniedResponse(typev3.StatusCode_Forbidden, "inbound impersonation header not allowed", nil), nil
	}

	// 3. Extract the caller's bearer token. A missing token is a 401 with a
	// WWW-Authenticate challenge so a compliant client knows to authenticate.
	token, ok := bearerToken(http.GetHeaders())
	if !ok {
		s.log.V(1).Info("denying request with no bearer token", "host", host)
		return deniedResponse(
			typev3.StatusCode_Unauthorized,
			"missing bearer token",
			[]*corev3.HeaderValueOption{overwriteHeader(headerWWWAuthenticate, wwwAuthenticateBearer)},
		), nil
	}

	// 4. Validate the OIDC token and map its claims to a Kubernetes identity. A
	// verification or mapping failure is a 401 — the caller authenticated but the
	// token is not acceptable.
	identity, err := entry.Authenticator.Authenticate(ctx, token)
	if err != nil {
		s.log.V(1).Info("denying request with invalid token", "host", host, "error", err.Error())
		return deniedResponse(typev3.StatusCode_Unauthorized, "invalid token", nil), nil
	}

	// 4b. Reject any mapped group that is unsafe under the comma-joined groups
	// encoding (failure-closed). The groups are returned as a single comma-joined
	// groups header (see okResponse), which the paired Lua filter splits back on
	// commas and trims of surrounding whitespace. Two group shapes break that
	// round-trip and could smuggle a different group:
	//   - a comma in the value ("dev,system:masters") splits into two groups; and
	//   - leading/trailing whitespace (" system:masters") is trimmed by the split
	//     filter into the bare group.
	// Both are denied rather than impersonated. The username is set with a single
	// overwrite header (no comma-join, no split filter), so it needs no such guard.
	if group, ok := firstUnsafeGroup(identity.Groups); ok {
		s.log.V(1).Info("denying request: mapped group is unsafe for the Impersonate-Group encoding", "host", host, "group", group)
		return deniedResponse(typev3.StatusCode_Forbidden, "mapped group contains a comma or surrounding whitespace", nil), nil
	}

	// 5. Resolve the backend's privileged impersonator credential from whichever
	// source the backend declares: a minted ServiceAccount token (serviceAccountRef)
	// or the credential Secret (credentialsSecretRef). A failure — a missing Secret,
	// a TokenRequest mint error, or an unwired TokenManager — is a 403 and fails
	// closed: the caller is authenticated but the authorizer cannot act on their
	// behalf, which is an internal/configuration fault, not a client authentication
	// error.
	impersonatorToken, err := resolveCredential(ctx, s.reader, s.tokenManager, s.namespace, entry)
	if err != nil {
		s.log.Error(err, "denying request: cannot resolve impersonator credential", "host", host)
		return deniedResponse(typev3.StatusCode_Forbidden, "credential unavailable", nil), nil
	}

	// 6. Build the OK response: set the impersonation identity and replace the
	// caller's Authorization with the impersonator token. Inbound impersonation
	// headers were already rejected in step 2, so the only Impersonate-* headers
	// the upstream sees are the derived ones.
	s.log.V(1).Info("allowing request",
		"host", host, "user", identity.Username, "groups", len(identity.Groups))
	return s.okResponse(identity, impersonatorToken), nil
}

// firstImpersonationHeader returns the name of the first inbound Kubernetes
// impersonation header (Impersonate-User, Impersonate-Group, Impersonate-Uid, or
// any Impersonate-Extra-* header) present in the request headers, and whether one
// was found. Envoy lowercases header keys in the ext_authz Headers map, so the
// prefix match is against the lowercase "impersonate-". The returned name is the
// key as Envoy presented it, for logging. A caller-supplied impersonation header
// is a smuggling attempt; the Check path denies the request fail-closed rather
// than scrubbing it (ADR-23).
func firstImpersonationHeader(headers map[string]string) (string, bool) {
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), impersonatePrefix) {
			return name, true
		}
	}
	return "", false
}

// firstUnsafeGroup returns the first group that is unsafe under the comma-joined
// groups encoding and true, or ("", false) if all groups are safe. A
// group is unsafe when it contains a comma or has leading/trailing whitespace,
// because the authorizer joins the mapped groups into one comma-separated header
// (see okResponse) and the paired Lua filter splits it back on commas and trims
// each element: a comma in a value would fan it into multiple impersonated groups,
// and surrounding whitespace (" system:masters") would be trimmed into the bare
// group — both privilege-escalation smuggling vectors. The Check path denies such
// a request fail-closed rather than emitting it. (Whitespace interior to a value
// is left intact; only the surrounding whitespace the split filter would strip is
// rejected.)
func firstUnsafeGroup(groups []string) (string, bool) {
	for _, group := range groups {
		if strings.Contains(group, ",") || strings.TrimSpace(group) != group {
			return group, true
		}
	}
	return "", false
}

// bearerToken extracts the bearer token from the request's Authorization header.
// Envoy lowercases header keys in the ext_authz Headers map, so the lookup is by
// the lowercase "authorization" key. The "Bearer " scheme prefix is matched
// case-insensitively and stripped. It returns ("", false) when the header is
// absent, not a bearer credential, or carries an empty token.
func bearerToken(headers map[string]string) (string, bool) {
	raw, ok := headers[strings.ToLower(headerAuthorization)]
	if !ok {
		return "", false
	}
	if len(raw) < len(bearerPrefix) || !strings.EqualFold(raw[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(raw[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// okResponse builds the allow CheckResponse carrying the Kubernetes impersonation
// headers. Impersonate-User, the groups header, and Authorization all use
// OVERWRITE_IF_EXISTS_OR_ADD so they replace any caller-supplied value. When the
// backend configures a UID claim (spec.oidc.uidClaim) the resolved UID is set as
// Impersonate-Uid, and each configured extra mapping (spec.oidc.extra) whose claim
// was present is set as an Impersonate-Extra-<key> header — both single-valued and
// emitted with the same overwrite/set action directly under their Impersonate-*
// names. Unlike the multi-valued groups header they need no comma-join + Lua split:
// a single overwrite header is added unconditionally by Envoy (setCopy), so the
// ext_authz append-drop that motivated the groups helper (HOL-1416) does not apply. The mapped
// groups are emitted as ONE comma-joined value (e.g. "oidc:dev,oidc:ops") under
// the configured groups header (s.groupsHeaderName, default X-Impersonate-Groups),
// NOT as repeated Impersonate-Group append options (HOL-1416): Envoy's ext_authz
// path puts an append=true header in headers_to_append, which it applies with
// appendCopy only if the request ALREADY carries that header — and the inbound
// request never carries Impersonate-Group (it is rejected fail-closed), so an
// appended Impersonate-Group is silently dropped before it reaches the API server.
// An overwrite (setCopy) adds the header unconditionally, and a distinct
// non-Impersonate-* name keeps it clear of the inbound-rejection / reject-Lua guard
// for Impersonate-*. The paired Lua split filter, running after ext_authz, unpacks
// the comma list into one Impersonate-Group header per element before the request
// reaches the API server (the API server requires one Impersonate-Group header per
// group and does NOT split a comma-separated value). A request that maps to zero
// groups emits no groups header at all. See docs/runbooks/holos-authenticator.md
// ("Splitting the comma-joined groups header") and ADR-23.
//
// The caller's original Authorization is NOT listed in HeadersToRemove. Setting
// Authorization with OVERWRITE_IF_EXISTS_OR_ADD already discards any
// caller-supplied value and replaces it with the impersonator token (overwriting
// it in place), so the upstream never sees the caller's credential. Envoy does
// not document a guaranteed apply order between the headers (set/append) and
// headers_to_remove mutations, so additionally listing Authorization in
// HeadersToRemove would risk removing the impersonator value we just set on the
// path where removals are applied last — leaving the request unauthenticated to
// the API server. The AC scopes HeadersToRemove to the "cannot be overwritten in
// place" case, which does not apply here.
func (s *CheckServer) okResponse(identity *Identity, impersonatorToken string) *authv3.CheckResponse {
	headers := make([]*corev3.HeaderValueOption, 0, 4+len(identity.Extra))
	headers = append(headers, overwriteHeader(headerImpersonateUser, identity.Username))
	// UID is single-valued, so it is set directly as Impersonate-Uid with the
	// overwrite action — Envoy adds it unconditionally (setCopy), exactly like
	// Impersonate-User. No comma-join + Lua split is needed (that exists only for
	// the multi-valued groups header).
	if identity.UID != "" {
		headers = append(headers, overwriteHeader(headerImpersonateUID, identity.UID))
	}
	if len(identity.Groups) > 0 {
		headers = append(headers, overwriteHeader(s.groupsHeaderName(), strings.Join(identity.Groups, ",")))
	}
	// Each extra is single-valued and set directly as Impersonate-Extra-<key> with
	// the overwrite action. Keys are emitted in lexical order so the response (and
	// the debug log) is deterministic regardless of Go map iteration order.
	for _, key := range sortedExtraKeys(identity.Extra) {
		headers = append(headers, overwriteHeader(headerImpersonateExtraPrefix+key, identity.Extra[key]))
	}
	headers = append(headers, overwriteHeader(headerAuthorization, "Bearer "+impersonatorToken))

	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				Headers: headers,
			},
		},
	}
}

// deniedResponse builds a fail-closed Denied CheckResponse with the given HTTP
// status and any response headers (e.g. a WWW-Authenticate challenge). The gRPC
// status is PermissionDenied, which maps the response to Envoy's ext_authz Denied
// branch; the HTTP status is what the downstream client receives.
func deniedResponse(code typev3.StatusCode, message string, headers []*corev3.HeaderValueOption) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{
			Code:    int32(codes.PermissionDenied),
			Message: "holos-authenticator: " + message,
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status:  &typev3.HttpStatus{Code: code},
				Headers: headers,
				Body:    "holos-authenticator: " + message + "\n",
			},
		},
	}
}

// logResponseHeaders logs, at V(1) debug verbosity, every header the authorizer
// returns to Envoy for this request — one log line per HeaderValueOption — so an
// operator troubleshooting whether Envoy mishandles the impersonation headers can
// confirm exactly which headers were emitted and how each is applied (HOL-1415).
// It reports the decision branch (ok vs denied) and, for a denial, the HTTP status
// the downstream client receives. Each header line carries its name, value, the
// append_action enum, and the deprecated append bool — the field Envoy's ext_authz
// path actually reads (HOL-1414) — so a mismatch between the two is visible in the
// logs. Every header the authorizer emits now uses the overwrite/set action (the
// append bool is false), the groups header included (HOL-1416). The whole function
// is skipped unless V(1) is enabled, so
// it costs nothing on the normal path; the Authorization value is redacted because
// it carries the impersonator credential.
func (s *CheckServer) logResponseHeaders(host string, resp *authv3.CheckResponse) {
	log := s.log.V(1)
	if !log.Enabled() || resp == nil {
		return
	}

	var (
		decision string
		status   string
		headers  []*corev3.HeaderValueOption
	)
	switch http := resp.GetHttpResponse().(type) {
	case *authv3.CheckResponse_OkResponse:
		decision = "ok"
		headers = http.OkResponse.GetHeaders()
	case *authv3.CheckResponse_DeniedResponse:
		decision = "denied"
		status = http.DeniedResponse.GetStatus().GetCode().String()
		headers = http.DeniedResponse.GetHeaders()
	default:
		decision = "unknown"
	}

	log.Info("returning response headers to caller",
		"host", host, "decision", decision, "status", status, "headerCount", len(headers))
	for i, h := range headers {
		hv := h.GetHeader()
		log.Info("response header",
			"host", host,
			"decision", decision,
			"index", i,
			"name", hv.GetKey(),
			"value", redactHeaderValue(hv.GetKey(), hv.GetValue()),
			"appendAction", h.GetAppendAction().String(),
			//nolint:staticcheck // SA1019: ext_authz reads the deprecated append bool (HOL-1414); log it to debug header handling
			"append", h.GetAppend().GetValue(),
		)
	}
}

// redactHeaderValue returns a log-safe rendering of a response header value. The
// Authorization header carries the backend's impersonator bearer token — a
// credential that must never reach the logs — so its value is replaced with a
// marker preserving only the byte length, enough to confirm the header was set
// without exposing the secret. Every other response header value (Impersonate-User,
// Impersonate-Group, WWW-Authenticate) is returned verbatim for troubleshooting.
func redactHeaderValue(name, value string) string {
	if strings.EqualFold(name, headerAuthorization) {
		return fmt.Sprintf("<redacted %d-byte credential>", len(value))
	}
	return value
}

// overwriteHeader builds a HeaderValueOption that sets name to value, replacing
// any existing header of that name (or adding it when absent). Every header the
// authorizer returns now uses this overwrite/set action: the groups header is a
// single comma-joined value the Lua split filter unpacks, so no append option is
// emitted (HOL-1416). An overwrite header lands in Envoy's headers_to_set bucket,
// applied with setCopy, which adds the header even when the request did not carry
// it — unlike an append header, which Envoy drops on the ext_authz path when no
// matching request header already exists.
func overwriteHeader(name, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: name, Value: value},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}
}

// sortedExtraKeys returns the keys of extra in lexical order, or nil when extra is
// empty. Sorting makes the emitted Impersonate-Extra-<key> headers (and the V(1)
// debug log of the response headers) deterministic regardless of Go's randomized
// map iteration order, so tests can assert an exact header-option slice.
func sortedExtraKeys(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// GRPCServer is a controller-runtime manager.Runnable that serves the
// CheckServer over gRPC. Registering it with mgr.Add ties its lifecycle to the
// manager: it starts when the manager starts and shuts down gracefully when the
// manager's context is cancelled (the same context leader election uses), so the
// gRPC server and the reconcilers share one process lifecycle.
type GRPCServer struct {
	// Addr is the TCP bind address for the gRPC server, e.g. ":9000". It is used
	// only when Listener is nil; Start binds it then.
	Addr string
	// Listener, when non-nil, is served directly instead of binding Addr. This is
	// for tests that pre-bind a listener (e.g. on an ephemeral port) and hand it
	// in, avoiding the close-then-reopen race a bind-by-address test would have.
	Listener net.Listener
	// Check is the Authorization service implementation to serve.
	Check *CheckServer
	// Log records server lifecycle events.
	Log logr.Logger
}

// NeedLeaderElection reports whether this Runnable requires leader election. The
// ext_authz server must answer Envoy on every replica, not only the elected
// leader, so it returns false and runs on all replicas.
func (*GRPCServer) NeedLeaderElection() bool { return false }

// Start runs the gRPC server until ctx is cancelled, then stops it gracefully.
// It satisfies controller-runtime's manager.Runnable interface.
func (g *GRPCServer) Start(ctx context.Context) error {
	lis := g.Listener
	if lis == nil {
		var err error
		lis, err = net.Listen("tcp", g.Addr)
		if err != nil {
			return fmt.Errorf("authenticator: listen on %q: %w", g.Addr, err)
		}
	}

	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, g.Check)

	// Report the actual bound address, which differs from g.Addr when a listener
	// was injected or when g.Addr used port 0.
	addr := lis.Addr().String()

	// Translate context cancellation into a graceful stop. GracefulStop drains
	// in-flight RPCs; the goroutine returns once Serve below unblocks.
	go func() {
		<-ctx.Done()
		g.Log.Info("shutting down ext_authz gRPC server", "addr", addr)
		srv.GracefulStop()
	}()

	g.Log.Info("starting ext_authz gRPC server", "addr", addr)
	if err := srv.Serve(lis); err != nil {
		return fmt.Errorf("authenticator: serve gRPC: %w", err)
	}
	return nil
}

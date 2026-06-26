// Package authenticator implements the holos-authenticator service (ADR-23): an
// Istio/Envoy gRPC external authorizer (envoy.service.auth.v3.Authorization)
// that validates an OIDC identity token, maps the token's claims to Kubernetes
// groups via a CEL expression, and returns Kubernetes impersonation headers so
// Envoy can forward an authenticated request to a backend API server with no
// other reverse proxy in the path.
//
// The ext_authz Check (HOL-1388) routes each request to a Backend by Host,
// validates the caller's OIDC bearer token, and on success returns an OK response
// that sets Kubernetes impersonation headers (Impersonate-User, plus one
// Impersonate-Group APPEND_IF_EXISTS_OR_ADD option per mapped group that Envoy
// comma-joins into a single header — paired with a Lua split filter, see
// okResponse), injects the backend's privileged
// credential as the upstream Authorization, and removes the caller's original
// token — so Envoy forwards the request straight to the API server. Any failure
// (unknown host, missing/invalid token, credential read failure, internal error)
// returns a fail-closed Denied 401/403; the server never returns OK on error. The
// GRPCServer manager.Runnable runs Check on the manager's lifecycle and
// leader-election context.
package authenticator

import (
	"context"
	"fmt"
	"net"
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
	// headerImpersonateGroup is repeated once per mapped Kubernetes group.
	headerImpersonateGroup = "Impersonate-Group"
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

	log logr.Logger
}

// NewCheckServer returns a CheckServer that resolves backends from store, reads
// impersonator credential Secrets from namespace via reader (the manager's
// APIReader), mints ServiceAccount tokens via writer (the manager's writable
// client, used for the TokenRequest create), and logs through log. store is the
// same registry the BackendReconciler writes, so the Check path sees backends as
// they become ready. writer may be nil for a Secret-only server (e.g. in tests);
// a backend that resolves its credential from a ServiceAccount then denies
// fail-closed.
func NewCheckServer(store *Store, reader client.Reader, writer client.Client, namespace string, log logr.Logger) *CheckServer {
	var tm *TokenManager
	if writer != nil {
		tm = NewTokenManager(writer, namespace)
	}
	return &CheckServer{store: store, reader: reader, tokenManager: tm, namespace: namespace, log: log}
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
//  5. Return OK setting Impersonate-User and one Impersonate-Group
//     APPEND_IF_EXISTS_OR_ADD option per mapped group (Envoy comma-joins them into
//     one header, paired with a Lua split filter — see okResponse), with the
//     impersonator token as the upstream Authorization, removing the caller's
//     original Authorization. Groups unsafe under that encoding (a comma or
//     surrounding whitespace) are denied fail-closed before this step.
//
// Every failure path — including any internal error — returns a Denied response,
// never OK, so the authorizer fails closed. The gRPC error return is always nil:
// the allow/deny decision is carried in the CheckResponse, not the RPC status,
// which is the ext_authz contract Envoy expects.
func (s *CheckServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	http := req.GetAttributes().GetRequest().GetHttp()
	host := http.GetHost()

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

	// 4b. Reject any mapped group that is unsafe under the comma-joined
	// Impersonate-Group encoding (failure-closed). Each group is returned as an
	// APPEND_IF_EXISTS_OR_ADD Impersonate-Group option that Envoy comma-joins into a
	// single header (see okResponse), which the paired Lua filter splits back on
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
	return okResponse(identity, impersonatorToken), nil
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
// Impersonate-Group encoding and true, or ("", false) if all groups are safe. A
// group is unsafe when it contains a comma or has leading/trailing whitespace,
// because Envoy joins the per-group append options into one comma-separated header
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
// headers. Impersonate-User and Authorization use OVERWRITE_IF_EXISTS_OR_ADD so
// they replace any caller-supplied value; each Impersonate-Group uses
// APPEND_IF_EXISTS_OR_ADD so the groups accumulate rather than the last value
// overwriting the rest. When Envoy applies several APPEND_IF_EXISTS_OR_ADD
// options for the same header it comma-concatenates their values into a single
// header line — Impersonate-Group: dev,ops — rather than emitting one header line
// per value. The Kubernetes API server's impersonation feature requires one
// Impersonate-Group header per group and does NOT split a comma-separated value,
// so this comma-joined header MUST be paired with an Envoy Lua filter that unpacks
// the comma list into one Impersonate-Group header per element before the request
// reaches the API server. See docs/runbooks/holos-authenticator.md ("Splitting the
// comma-joined Impersonate-Group header") and ADR-23.
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
func okResponse(identity *Identity, impersonatorToken string) *authv3.CheckResponse {
	headers := make([]*corev3.HeaderValueOption, 0, len(identity.Groups)+2)
	headers = append(headers, overwriteHeader(headerImpersonateUser, identity.Username))
	for _, group := range identity.Groups {
		headers = append(headers, appendHeader(headerImpersonateGroup, group))
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

// overwriteHeader builds a HeaderValueOption that sets name to value, replacing
// any existing header of that name (or adding it when absent).
func overwriteHeader(name, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: name, Value: value},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}
}

// appendHeader builds a HeaderValueOption with AppendAction
// APPEND_IF_EXISTS_OR_ADD, so repeated calls for the same header name accumulate
// rather than overwrite. Envoy applies these by comma-concatenating the values
// into a single header line (Impersonate-Group: dev,ops), not by emitting one line
// per value; a paired Lua filter splits that comma list back into one
// Impersonate-Group header per group for the API server (see okResponse and the
// runbook).
func appendHeader(name, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: name, Value: value},
		AppendAction: corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
	}
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

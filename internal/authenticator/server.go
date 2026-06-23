// Package authenticator implements the holos-authenticator service (ADR-23): an
// Istio/Envoy gRPC external authorizer (envoy.service.auth.v3.Authorization)
// that validates an OIDC identity token, maps the token's claims to Kubernetes
// groups via a CEL expression, and returns Kubernetes impersonation headers so
// Envoy can forward an authenticated request to a backend API server with no
// other reverse proxy in the path.
//
// The ext_authz Check (HOL-1388) routes each request to a Backend by Host,
// validates the caller's OIDC bearer token, and on success returns an OK response
// that sets Kubernetes impersonation headers (Impersonate-User, one
// Impersonate-Group per mapped group), injects the backend's privileged
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

	// namespace is the authorizer's own namespace, where every backend credential
	// Secret lives (the Backend's spec.credentialsSecretRef names only the Secret,
	// not a namespace).
	namespace string

	log logr.Logger
}

// NewCheckServer returns a CheckServer that resolves backends from store, reads
// impersonator credential Secrets from namespace via reader (the manager's
// APIReader), and logs through log. store is the same registry the
// BackendReconciler writes, so the Check path sees backends as they become ready.
func NewCheckServer(store *Store, reader client.Reader, namespace string, log logr.Logger) *CheckServer {
	return &CheckServer{store: store, reader: reader, namespace: namespace, log: log}
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
//  4. Resolve the backend's privileged impersonator credential; read failure →
//     Denied 403.
//  5. Return OK setting Impersonate-User, one Impersonate-Group per mapped group,
//     and the impersonator token as Authorization, removing the caller's original
//     Authorization.
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

	// 2. Extract the caller's bearer token. A missing token is a 401 with a
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

	// 3. Validate the OIDC token and map its claims to a Kubernetes identity. A
	// verification or mapping failure is a 401 — the caller authenticated but the
	// token is not acceptable.
	identity, err := entry.Authenticator.Authenticate(ctx, token)
	if err != nil {
		s.log.V(1).Info("denying request with invalid token", "host", host, "error", err.Error())
		return deniedResponse(typev3.StatusCode_Unauthorized, "invalid token", nil), nil
	}

	// 4. Resolve the backend's privileged impersonator credential. A read failure
	// is a 403 and fails closed — the caller is authenticated but the authorizer
	// cannot act on their behalf, which is an internal/configuration fault, not a
	// client authentication error.
	impersonatorToken, err := resolveImpersonatorToken(ctx, s.reader, s.namespace, entry.CredentialsSecretRef)
	if err != nil {
		s.log.Error(err, "denying request: cannot resolve impersonator credential", "host", host)
		return deniedResponse(typev3.StatusCode_Forbidden, "credential unavailable", nil), nil
	}

	// 5. Build the OK response: set the impersonation identity, replace the
	// caller's Authorization with the impersonator token, and remove the original
	// Authorization so the upstream never sees the caller's credential.
	s.log.V(1).Info("allowing request",
		"host", host, "user", identity.Username, "groups", len(identity.Groups))
	return okResponse(identity, impersonatorToken), nil
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
// APPEND_IF_EXISTS_OR_ADD so the groups accumulate into repeated headers (the
// standard multi-group impersonation encoding, compatible with any conformant
// cluster). The caller's original Authorization is also listed in
// HeadersToRemove: setting Authorization overwrites it, but the removal is a
// defense-in-depth guarantee that no caller credential reaches the upstream even
// if Envoy applied the options in an unexpected order.
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
				Headers:         headers,
				HeadersToRemove: []string{headerAuthorization},
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

// appendHeader builds a HeaderValueOption that appends value as another instance
// of header name, so repeated calls accumulate into multiple same-named headers
// (the multi-group Impersonate-Group encoding).
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

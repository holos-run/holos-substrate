// Package authenticator implements the holos-authenticator service (ADR-23): an
// Istio/Envoy gRPC external authorizer (envoy.service.auth.v3.Authorization)
// that — in later phases — validates an OIDC identity token, maps the token's
// claims to Kubernetes groups via a CEL expression, and returns Kubernetes
// impersonation headers so Envoy can forward an authenticated request to a
// backend API server with no other reverse proxy in the path.
//
// This phase (HOL-1385) ships only the scaffold: the gRPC server type that
// implements the ext_authz Authorization service with a stub Check returning an
// always-Denied (HTTP 403) response, plus the controller-runtime
// manager.Runnable adapter that runs it on the manager's lifecycle and
// leader-election context. The real OIDC, CEL, and CRD-driven configuration land
// in HOL-1386..HOL-1389.
package authenticator

import (
	"context"
	"fmt"
	"net"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// CheckServer implements the Envoy ext_authz Authorization service
// (envoy.service.auth.v3.Authorization). In this phase its Check is a stub that
// always denies with HTTP 403; later phases replace the body with OIDC
// validation, CEL claim→group mapping, and Kubernetes impersonation-header
// injection (ADR-23).
type CheckServer struct {
	// UnimplementedAuthorizationServer provides forward-compatible defaults for
	// any service methods added to the proto in the future, per the gRPC
	// recommendation for server implementations.
	authv3.UnimplementedAuthorizationServer

	// store is the shared host-keyed registry of ready backends, written by the
	// BackendReconciler and read here by host to validate a request's bearer token
	// (HOL-1388). It is injected so the reconciler and this server share one
	// instance. This phase does not yet read it — the Check body still stubs — but
	// the wiring is in place so HOL-1388 only changes the Check body.
	store *Store

	log logr.Logger
}

// NewCheckServer returns a CheckServer that resolves backends from store and logs
// through the supplied logr.Logger. store is the same registry the
// BackendReconciler writes, so the Check path sees backends as they become ready.
func NewCheckServer(store *Store, log logr.Logger) *CheckServer {
	return &CheckServer{store: store, log: log}
}

// Check implements envoy.service.auth.v3.Authorization. The scaffold response is
// a deterministic Denied (HTTP 403) result: it proves the ext_authz proto wiring
// compiles and serves end to end without yet making any authorization decision.
// HOL-1388 replaces this with the real allow/deny logic and the impersonation
// headers an allowed request carries.
func (s *CheckServer) Check(_ context.Context, _ *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	// Record the scaffold denial at debug verbosity so it is observable without
	// flooding logs on a hot data path. HOL-1388 replaces this with the real
	// allow/deny decision and structured request context.
	s.log.V(1).Info("ext_authz scaffold stub denying request")
	return &authv3.CheckResponse{
		// A non-OK gRPC status maps the response to the Denied branch in Envoy's
		// ext_authz filter; PermissionDenied is the conventional code for an
		// authorization failure.
		Status: &rpcstatus.Status{
			Code:    int32(codes.PermissionDenied),
			Message: "holos-authenticator: scaffold stub denies all requests",
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
				Body:   "holos-authenticator: scaffold stub denies all requests\n",
			},
		},
	}, nil
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

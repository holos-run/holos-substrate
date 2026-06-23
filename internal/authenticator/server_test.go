package authenticator

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestCheckStubDenies starts the CheckServer over an in-memory bufconn listener,
// dials it as an Envoy ext_authz client, calls Check, and asserts the scaffold
// stub returns a Denied (HTTP 403 / PermissionDenied) response. This proves the
// envoy.service.auth.v3.Authorization proto wiring compiles and serves end to
// end, which is the whole contract of the HOL-1385 scaffold phase.
func TestCheckStubDenies(t *testing.T) {
	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)

	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, NewCheckServer(NewStore(), logr.Discard()))

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Errorf("serve bufconn: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{})
	if err != nil {
		t.Fatalf("Check RPC: %v", err)
	}

	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.PermissionDenied; got != want {
		t.Errorf("status code = %v, want %v", got, want)
	}

	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatalf("expected a DeniedResponse, got %T", resp.GetHttpResponse())
	}
	if got, want := denied.GetStatus().GetCode(), typev3.StatusCode_Forbidden; got != want {
		t.Errorf("HTTP status = %v, want %v", got, want)
	}
}

// TestGRPCServerStartStop runs the GRPCServer Runnable on a real loopback
// listener, dials it, calls Check, and then cancels the context to confirm Start
// returns cleanly on graceful shutdown — exercising the manager.Runnable adapter
// the manager drives via mgr.Add.
func TestGRPCServerStartStop(t *testing.T) {
	// Bind :0 to let the kernel choose a free port and inject the listener
	// directly. Injecting a pre-bound listener (rather than closing it and asking
	// Start to reopen the same address) closes the close-then-reopen window in
	// which another process could claim the port and make the test flaky.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()

	g := &GRPCServer{
		Listener: lis,
		Check:    NewCheckServer(NewStore(), logr.Discard()),
		Log:      logr.Discard(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- g.Start(ctx) }()

	conn, err := dialReady(t, addr)
	if err != nil {
		cancel()
		t.Fatalf("dial server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	if _, err := authv3.NewAuthorizationClient(conn).Check(callCtx, &authv3.CheckRequest{}); err != nil {
		// A PermissionDenied gRPC status is not returned as an error here because
		// the stub encodes denial in the CheckResponse, not the RPC status, so any
		// error is a real wiring failure.
		if status.Code(err) == codes.Unavailable {
			cancel()
			t.Fatalf("server unavailable: %v", err)
		}
		cancel()
		t.Fatalf("Check RPC: %v", err)
	}

	// Cancel the context and confirm Start returns without error (graceful stop).
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5s of context cancellation")
	}
}

// dialReady creates a client for addr and actively waits for the connection to
// reach Ready, so the test does not race the server's Listen in Start. Because
// grpc.NewClient is lazy (it returns before any TCP connection is attempted),
// the readiness wait — Connect plus WaitForStateChange — is what proves the
// server is actually accepting connections, not merely that a client handle was
// allocated.
func dialReady(t *testing.T, addr string) (*grpc.ClientConn, error) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn.Connect()
	for {
		s := conn.GetState()
		if s == connectivity.Ready {
			return conn, nil
		}
		if !conn.WaitForStateChange(ctx, s) {
			_ = conn.Close()
			return nil, fmt.Errorf("connection not Ready within timeout (last state %v): %w", s, ctx.Err())
		}
	}
}

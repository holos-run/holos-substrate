package authenticator

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
)

const testNamespace = "holos-authenticator"

// newTestAuthenticator builds an Authenticator whose verifier is a fake returning
// the given claims (so no live issuer is needed) and whose group mapping is the
// default groups-claim mapping. usernameClaim selects the username claim.
func newTestAuthenticator(t *testing.T, claims map[string]any, usernameClaim string) *Authenticator {
	t.Helper()
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	return NewAuthenticator(&fakeVerifier{claims: claims}, mapper, usernameClaim)
}

// newFailingAuthenticator builds an Authenticator whose verifier always fails,
// modeling an invalid/expired/wrong-audience token.
func newFailingAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	return NewAuthenticator(&fakeVerifier{err: fmt.Errorf("token expired")}, mapper, "sub")
}

// secretReader returns a non-caching client.Reader backed by the given objects,
// modeling the manager's APIReader for credential Secret resolution.
func secretReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// credentialSecret builds the impersonator credential Secret in the authorizer's
// namespace with the given token under the conventional "token" key.
func credentialSecret(name, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{credentialKeyToken: []byte(token)},
	}
}

// serveCheck starts srv over an in-memory bufconn listener and returns a connected
// Authorization client. The listener, server, and connection are torn down via
// t.Cleanup.
func serveCheck(t *testing.T, srv *CheckServer) authv3.AuthorizationClient {
	t.Helper()
	const bufSize = 1024 * 1024
	lis := bufconn.Listen(bufSize)

	gsrv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(gsrv, srv)
	go func() {
		if err := gsrv.Serve(lis); err != nil {
			t.Errorf("serve bufconn: %v", err)
		}
	}()
	t.Cleanup(gsrv.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return authv3.NewAuthorizationClient(conn)
}

// checkRequest builds a CheckRequest with the given Host and request headers.
// Header keys are lowercased to match how Envoy presents them in the ext_authz
// Headers map.
func checkRequest(host string, headers map[string]string) *authv3.CheckRequest {
	return &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Host:    host,
					Headers: headers,
				},
			},
		},
	}
}

// TestCheckAllowSetsImpersonationHeaders is the happy path: a known host, a valid
// token, and a resolvable credential. It asserts the OK response sets
// Impersonate-User (overwrite), one Impersonate-Group per mapped group (append),
// and the impersonator token as Authorization (overwrite, which replaces the
// caller's token in place — so HeadersToRemove is empty).
func TestCheckAllowSetsImpersonationHeaders(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice", "groups": []any{"dev", "ops"}}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization": "Bearer caller-token",
	}))

	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	ok := resp.GetOkResponse()
	if ok == nil {
		t.Fatalf("expected an OkResponse, got %T", resp.GetHttpResponse())
	}

	// Exact header-option set: Impersonate-User overwrite, two Impersonate-Group
	// appends in order, Authorization overwrite with the impersonator token.
	want := []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		appendHeader(headerImpersonateGroup, "dev"),
		appendHeader(headerImpersonateGroup, "ops"),
		overwriteHeader(headerAuthorization, "Bearer impersonator-token"),
	}
	assertHeaderOptions(t, ok.GetHeaders(), want)

	// The caller's original Authorization is replaced in place by the overwrite
	// above, so nothing is listed in headers_to_remove — listing it there would
	// risk removing the impersonator token on the undocumented apply-order path
	// where removals run after sets.
	if got := ok.GetHeadersToRemove(); len(got) != 0 {
		t.Errorf("headers_to_remove = %v, want empty", got)
	}
}

// TestCheckAllowNoGroups asserts a valid token with no groups claim still allows
// and emits no Impersonate-Group headers (empty groups are not an error).
func TestCheckAllowNoGroups(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "bob"}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer t"}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	want := []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "bob"),
		overwriteHeader(headerAuthorization, "Bearer imp"),
	}
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), want)
}

// TestCheckInboundImpersonationHeaderDenies asserts that a request carrying any
// client-supplied Impersonate-* header is denied fail-closed (HTTP 403), closing
// the header-smuggling / confused-deputy hole (ADR-23): the authorizer injects
// the backend's privileged credential downstream, so a smuggled
// Impersonate-Group: system:masters must never survive to the API server. Each
// impersonation-header variant is rejected, even when an otherwise-valid bearer
// token is present.
func TestCheckInboundImpersonationHeaderDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, testNamespace, logr.Discard()))

	// Envoy lowercases header keys in the ext_authz Headers map.
	smuggled := []string{
		"impersonate-user",
		"impersonate-group",
		"impersonate-uid",
		"impersonate-extra-scopes",
	}
	for _, name := range smuggled {
		t.Run(name, func(t *testing.T) {
			resp := mustCheck(t, client, checkRequest(host, map[string]string{
				"authorization": "Bearer good",
				name:            "system:masters",
			}))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckUnknownHostDenies asserts an unconfigured host is denied with HTTP 403.
func TestCheckUnknownHostDenies(t *testing.T) {
	client := serveCheck(t, NewCheckServer(NewStore(), secretReader(t), testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest("unknown.example.com", map[string]string{
		"authorization": "Bearer t",
	}))
	assertDenied(t, resp, typev3.StatusCode_Forbidden)
}

// TestCheckMissingTokenDenies asserts a request with no bearer token is denied
// with HTTP 401 and a WWW-Authenticate challenge.
func TestCheckMissingTokenDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:          host,
		Authenticator: newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
	})
	client := serveCheck(t, NewCheckServer(store, secretReader(t), testNamespace, logr.Discard()))

	// No Authorization header at all.
	resp := mustCheck(t, client, checkRequest(host, map[string]string{}))
	denied := assertDenied(t, resp, typev3.StatusCode_Unauthorized)

	want := []*corev3.HeaderValueOption{overwriteHeader(headerWWWAuthenticate, wwwAuthenticateBearer)}
	assertHeaderOptions(t, denied.GetHeaders(), want)
}

// TestCheckNonBearerTokenDenies asserts an Authorization header that is not a
// bearer credential is treated as a missing token (401).
func TestCheckNonBearerTokenDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:          host,
		Authenticator: newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
	})
	client := serveCheck(t, NewCheckServer(store, secretReader(t), testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization": "Basic dXNlcjpwYXNz",
	}))
	assertDenied(t, resp, typev3.StatusCode_Unauthorized)
}

// TestCheckInvalidTokenDenies asserts a token that fails OIDC verification is
// denied with HTTP 401.
func TestCheckInvalidTokenDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newFailingAuthenticator(t),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer bad"}))
	assertDenied(t, resp, typev3.StatusCode_Unauthorized)
}

// TestCheckCredentialReadFailureDenies asserts that when the impersonator
// credential Secret is missing the request is denied fail-closed (HTTP 403),
// never allowed — even though the caller's token is valid.
func TestCheckCredentialReadFailureDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "absent"},
	})
	// Reader has no Secret named "absent".
	client := serveCheck(t, NewCheckServer(store, secretReader(t), testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer good"}))
	assertDenied(t, resp, typev3.StatusCode_Forbidden)
}

// TestCheckMultipleBackendsRouteByHost asserts two backends with distinct hosts,
// OIDC identities, and credentials are served concurrently, each routed by Host.
func TestCheckMultipleBackendsRouteByHost(t *testing.T) {
	store := NewStore()
	store.Set(testNamespace+"/a", &Entry{
		Host:                 "a.example.com",
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice", "groups": []any{"team-a"}}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds-a"},
	})
	store.Set(testNamespace+"/b", &Entry{
		Host:                 "b.example.com",
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "bob", "groups": []any{"team-b"}}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds-b"},
	})
	reader := secretReader(t, credentialSecret("creds-a", "tok-a"), credentialSecret("creds-b", "tok-b"))
	client := serveCheck(t, NewCheckServer(store, reader, testNamespace, logr.Discard()))

	respA := mustCheck(t, client, checkRequest("a.example.com", map[string]string{"authorization": "Bearer x"}))
	assertHeaderOptions(t, respA.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		appendHeader(headerImpersonateGroup, "team-a"),
		overwriteHeader(headerAuthorization, "Bearer tok-a"),
	})

	respB := mustCheck(t, client, checkRequest("b.example.com", map[string]string{"authorization": "Bearer y"}))
	assertHeaderOptions(t, respB.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "bob"),
		appendHeader(headerImpersonateGroup, "team-b"),
		overwriteHeader(headerAuthorization, "Bearer tok-b"),
	})
}

// TestCheckCustomTokenKey asserts the credential Secret's token can be read from a
// non-default key named by spec.credentialsSecretRef.Key.
func TestCheckCustomTokenKey(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds", Key: "sa-token"},
	})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: testNamespace},
		Data:       map[string][]byte{"sa-token": []byte("custom-key-token")},
	}
	client := serveCheck(t, NewCheckServer(store, secretReader(t, secret), testNamespace, logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer x"}))
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(headerAuthorization, "Bearer custom-key-token"),
	})
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
		Check:    NewCheckServer(NewStore(), secretReader(t), testNamespace, logr.Discard()),
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
	// An empty request has no host, so the server denies it; that is encoded in
	// the CheckResponse, not the RPC status, so any RPC error is a real wiring
	// failure.
	if _, err := authv3.NewAuthorizationClient(conn).Check(callCtx, &authv3.CheckRequest{}); err != nil {
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

// mustCheck calls Check and fails the test on an RPC error (the allow/deny
// decision is in the response, so any RPC error is a wiring failure).
func mustCheck(t *testing.T, client authv3.AuthorizationClient, req *authv3.CheckRequest) *authv3.CheckResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Check(ctx, req)
	if err != nil {
		t.Fatalf("Check RPC: %v", err)
	}
	return resp
}

// assertDenied asserts resp is a Denied response with gRPC PermissionDenied and
// the given HTTP status, and returns the DeniedHttpResponse for further checks.
func assertDenied(t *testing.T, resp *authv3.CheckResponse, httpStatus typev3.StatusCode) *authv3.DeniedHttpResponse {
	t.Helper()
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.PermissionDenied; got != want {
		t.Errorf("status code = %v, want %v", got, want)
	}
	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatalf("expected a DeniedResponse, got %T", resp.GetHttpResponse())
	}
	if got := denied.GetStatus().GetCode(); got != httpStatus {
		t.Errorf("HTTP status = %v, want %v", got, httpStatus)
	}
	return denied
}

// assertHeaderOptions asserts got matches want exactly, in order, comparing each
// option's header name, value, and append action.
func assertHeaderOptions(t *testing.T, got, want []*corev3.HeaderValueOption) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("header option count = %d, want %d\n got: %s\nwant: %s",
			len(got), len(want), formatOptions(got), formatOptions(want))
	}
	for i := range want {
		gh, wh := got[i].GetHeader(), want[i].GetHeader()
		if gh.GetKey() != wh.GetKey() || gh.GetValue() != wh.GetValue() {
			t.Errorf("header[%d] = %q:%q, want %q:%q", i,
				gh.GetKey(), gh.GetValue(), wh.GetKey(), wh.GetValue())
		}
		if got[i].GetAppendAction() != want[i].GetAppendAction() {
			t.Errorf("header[%d] %q append action = %v, want %v", i,
				gh.GetKey(), got[i].GetAppendAction(), want[i].GetAppendAction())
		}
	}
}

func formatOptions(opts []*corev3.HeaderValueOption) string {
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		parts = append(parts, fmt.Sprintf("%s=%q(%v)", o.GetHeader().GetKey(), o.GetHeader().GetValue(), o.GetAppendAction()))
	}
	return fmt.Sprintf("%v", parts)
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

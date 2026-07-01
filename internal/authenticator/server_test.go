package authenticator

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
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
	authenticationv1 "k8s.io/api/authentication/v1"
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
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	return NewAuthenticator(&fakeVerifier{claims: claims}, mapper, usernameClaim, "", "", nil, nil)
}

// newFailingAuthenticator builds an Authenticator whose verifier always fails,
// modeling an invalid/expired/wrong-audience token.
func newFailingAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	return NewAuthenticator(&fakeVerifier{err: fmt.Errorf("token expired")}, mapper, "sub", "", "", nil, nil)
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

// fakeMintClient is a minimal client.Client whose token sub-resource Create either
// populates the TokenRequest status with a fixed token (modeling a successful
// TokenRequest mint) or returns err (modeling a mint failure), without a live API
// server. Only SubResource("token").Create is exercised by the SA-path Check
// tests; every other method panics so an unexpected call is caught loudly.
type fakeMintClient struct {
	client.Client
	token string
	err   error
}

func (c *fakeMintClient) SubResource(string) client.SubResourceClient {
	return &fakeMintSubResource{parent: c}
}

type fakeMintSubResource struct {
	client.SubResourceClient
	parent *fakeMintClient
}

func (s *fakeMintSubResource) Create(_ context.Context, _ client.Object, sub client.Object, _ ...client.SubResourceCreateOption) error {
	if s.parent.err != nil {
		return s.parent.err
	}
	tr, ok := sub.(*authenticationv1.TokenRequest)
	if !ok {
		return fmt.Errorf("fakeMintClient: unexpected sub-resource type %T", sub)
	}
	tr.Status.Token = s.parent.token
	tr.Status.ExpirationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
	return nil
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
// Impersonate-User (overwrite), a single comma-joined groups header (overwrite,
// default x-impersonate-groups — HOL-1416), and the impersonator token as
// Authorization (overwrite, which replaces the caller's token in place — so
// HeadersToRemove is empty).
func TestCheckAllowSetsImpersonationHeaders(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice", "groups": []any{"dev", "ops"}}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

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

	// Exact header-option set: Impersonate-User overwrite, the comma-joined groups
	// header (overwrite, default name), Authorization overwrite with the impersonator
	// token.
	want := []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(defaultGroupsHeader, "dev,ops"),
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
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

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

// TestCheckAllowSetsUIDAndExtraHeaders is the end-to-end happy path for the UID and
// extra mappings (HOL-1419): a backend configured with a UID claim and an extra
// mapping returns Impersonate-Uid and Impersonate-Extra-<key> alongside the username,
// groups, and impersonator Authorization. The username is read from email, the UID
// from sub (a stable identifier), and the extra carries email.
func TestCheckAllowSetsUIDAndExtraHeaders(t *testing.T) {
	const host = "api.example.com"
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	claims := map[string]any{
		"sub":    "uid-123",
		"email":  "alice@example.com",
		"groups": []any{"dev"},
	}
	auth := NewAuthenticator(&fakeVerifier{claims: claims}, mapper, "email", "", "sub",
		[]authenticatorv1alpha1.ExtraMapping{{Key: "email", ValueClaim: "email"}}, nil)

	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        auth,
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer caller-token"}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice@example.com"),
		overwriteHeader(headerImpersonateUID, "uid-123"),
		overwriteHeader(defaultGroupsHeader, "dev"),
		overwriteHeader(headerImpersonateExtraPrefix+"email", "alice@example.com"),
		overwriteHeader(headerAuthorization, "Bearer impersonator-token"),
	})
}

// TestOkResponseGroupsHeaderIsCommaJoinedOverwrite pins the HOL-1416 fix: the
// mapped groups are emitted as a SINGLE comma-joined value under the configured
// groups header, with the OVERWRITE (set) action and no append bool — never as
// repeated Impersonate-Group append options. An append header is dropped by Envoy's
// ext_authz path when the request does not already carry it (the inbound request
// never carries Impersonate-Group), so a set into a distinct header is what
// survives to the paired Lua split filter.
func TestOkResponseGroupsHeaderIsCommaJoinedOverwrite(t *testing.T) {
	s := &CheckServer{groupsHeader: "x-custom-groups", log: logr.Discard()}
	resp := s.okResponse(&Identity{Username: "alice", Groups: []string{"dev", "ops"}}, "tok")

	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader("x-custom-groups", "dev,ops"),
		overwriteHeader(headerAuthorization, "Bearer tok"),
	})

	// The groups header must use the overwrite/set action with no append bool, so
	// Envoy adds it unconditionally (setCopy) rather than dropping it (the append
	// path applies only when the header already exists).
	groups := resp.GetOkResponse().GetHeaders()[1]
	if got, want := groups.GetAppendAction(), corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD; got != want {
		t.Errorf("groups header append action = %v, want %v", got, want)
	}
	if got := groups.GetAppend().GetValue(); got != false { //nolint:staticcheck // SA1019: ext_authz reads the deprecated append bool (HOL-1416)
		t.Errorf("groups header append bool = %v, want false (set, not append)", got)
	}
}

// TestOkResponseGroupsHeaderDefaultName asserts a CheckServer with no configured
// groups header falls back to the default x-impersonate-groups name (AC #2).
func TestOkResponseGroupsHeaderDefaultName(t *testing.T) {
	s := &CheckServer{log: logr.Discard()}
	resp := s.okResponse(&Identity{Username: "alice", Groups: []string{"dev"}}, "tok")
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader("x-impersonate-groups", "dev"),
		overwriteHeader(headerAuthorization, "Bearer tok"),
	})
}

// TestOkResponseSetsUIDAndExtraHeaders asserts the OK response sets Impersonate-Uid
// (when a UID is resolved) and one Impersonate-Extra-<key> header per extra mapping,
// both single-valued overwrite/set headers like Impersonate-User. The full
// header-option slice is asserted in order: user, uid, groups, the extra headers in
// lexical key order, then Authorization.
func TestOkResponseSetsUIDAndExtraHeaders(t *testing.T) {
	s := &CheckServer{log: logr.Discard()}
	resp := s.okResponse(&Identity{
		Username: "alice@example.com",
		UID:      "uid-123",
		Groups:   []string{"dev", "ops"},
		// Deliberately out of lexical order to prove the emission is sorted.
		Extra: map[string]string{"tenant": "acme", "email": "alice@example.com"},
	}, "tok")

	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice@example.com"),
		overwriteHeader(headerImpersonateUID, "uid-123"),
		overwriteHeader(defaultGroupsHeader, "dev,ops"),
		overwriteHeader(headerImpersonateExtraPrefix+"email", "alice@example.com"),
		overwriteHeader(headerImpersonateExtraPrefix+"tenant", "acme"),
		overwriteHeader(headerAuthorization, "Bearer tok"),
	})

	// The UID header uses the overwrite/set action (no append bool) so Envoy adds it
	// unconditionally, exactly like Impersonate-User — single-valued, no Lua split.
	uid := resp.GetOkResponse().GetHeaders()[1]
	if got, want := uid.GetAppendAction(), corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD; got != want {
		t.Errorf("uid header append action = %v, want %v", got, want)
	}
	if got := uid.GetAppend().GetValue(); got != false { //nolint:staticcheck // SA1019: ext_authz reads the deprecated append bool
		t.Errorf("uid header append bool = %v, want false (set, not append)", got)
	}
}

// TestOkResponseOmitsUIDAndExtraWhenUnset asserts a backend with no UID claim and no
// extra mappings emits neither Impersonate-Uid nor any Impersonate-Extra-* header —
// the backward-compatible default (only user, groups, Authorization).
func TestOkResponseOmitsUIDAndExtraWhenUnset(t *testing.T) {
	s := &CheckServer{log: logr.Discard()}
	resp := s.okResponse(&Identity{Username: "alice", Groups: []string{"dev"}}, "tok")
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(defaultGroupsHeader, "dev"),
		overwriteHeader(headerAuthorization, "Bearer tok"),
	})
}

// TestValidateExtraKey asserts the extra-key validation the reconciler uses: a
// canonical (lowercase, no '%') HTTP header token passes, and an empty key, an
// uppercase key, a key containing '%', or one with non-token characters (space,
// slash, colon) is rejected — so the emitted Impersonate-Extra-<key> header
// round-trips to the same extra key after the API server lowercases and
// percent-unescapes the suffix (and the case-sensitive listMapKey uniqueness stays
// aligned with the API server's lowercased keys).
func TestValidateExtraKey(t *testing.T) {
	valid := []string{"email", "example.com-email", "tenant_id", "scopes.v1"}
	for _, in := range valid {
		t.Run("valid/"+in, func(t *testing.T) {
			if err := ValidateExtraKey(in); err != nil {
				t.Errorf("ValidateExtraKey(%q) returned error: %v", in, err)
			}
		})
	}
	invalid := map[string]string{
		"empty":     "",
		"uppercase": "Email",       // API server lowercases — would not round-trip
		"percent":   "tenant%2fid", // API server percent-unescapes — would decode to a different key
		"space":     "ex ample",
		"slash":     "example.com/email",
		"colon":     "ex:ample",
	}
	for name, in := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if err := ValidateExtraKey(in); err == nil {
				t.Errorf("ValidateExtraKey(%q) = nil, want an error", in)
			}
		})
	}
}

// TestValidateGroupsHeader asserts the --impersonate-groups-header validation
// (HOL-1416): valid names are canonicalized to lowercase, and an empty name, a name
// with non-token characters, the Authorization header, or any Impersonate-* header
// is rejected — the misconfigurations that would break the authorizer's security
// model.
func TestValidateGroupsHeader(t *testing.T) {
	valid := map[string]string{
		"default":           "x-impersonate-groups",
		"uppercase":         "X-Impersonate-Groups",
		"mixed-case canon":  "X-Custom-Groups",
		"token punctuation": "x-groups_v1.0",
	}
	for name, in := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			got, err := ValidateGroupsHeader(in)
			if err != nil {
				t.Fatalf("ValidateGroupsHeader(%q) returned error: %v", in, err)
			}
			if want := strings.ToLower(in); got != want {
				t.Errorf("ValidateGroupsHeader(%q) = %q, want %q (canonical lowercase)", in, got, want)
			}
		})
	}

	invalid := map[string]string{
		"empty":              "",
		"space":              "x impersonate groups",
		"colon":              "x:groups",
		"authorization":      "Authorization",
		"impersonate-group":  "Impersonate-Group",
		"impersonate-prefix": "Impersonate-Extra-scopes",
	}
	for name, in := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if got, err := ValidateGroupsHeader(in); err == nil {
				t.Errorf("ValidateGroupsHeader(%q) = %q, nil; want an error", in, got)
			}
		})
	}
}

// TestCheckDeniesUnsafeGroup asserts a request whose mapped groups include a value
// that is unsafe under the comma-joined groups encoding is denied fail-closed (HTTP
// 403). Groups are returned as a single comma-joined groups header which the paired
// Lua filter splits back on commas and trims of surrounding whitespace; a value
// holding its own comma ("dev,system:masters") would fan out into multiple
// impersonated groups, and surrounding whitespace (" system:masters") would be
// trimmed into the bare group — both must be rejected, not impersonated (HOL-1413).
func TestCheckDeniesUnsafeGroup(t *testing.T) {
	const host = "api.example.com"
	cases := map[string][]any{
		"comma":           {"dev,system:masters", "ops"},
		"leading-space":   {" system:masters", "ops"},
		"trailing-space":  {"system:masters ", "ops"},
		"leading-tab":     {"\tsystem:masters", "ops"},
		"whitespace-only": {"  ", "ops"},
	}
	for name, groups := range cases {
		t.Run(name, func(t *testing.T) {
			store := NewStore()
			store.Set(testNamespace+"/backend", &Entry{
				Host:                 host,
				Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice", "groups": groups}, "sub"),
				CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
			})
			reader := secretReader(t, credentialSecret("creds", "imp"))
			client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

			resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer good"}))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckInboundImpersonationHeaderDenies asserts that on a backend with delegated
// impersonation DISABLED (spec.impersonation nil — the default), a request carrying
// any client-supplied Impersonate-* header is denied fail-closed (HTTP 403), closing
// the header-smuggling / confused-deputy hole (ADR-23): the authorizer injects
// the backend's privileged credential downstream, so a smuggled
// Impersonate-Group: system:masters must never survive to the API server. Each
// impersonation-header variant is rejected, even when an otherwise-valid bearer
// token is present. (When spec.impersonation IS set, a non-reserved Impersonate-*
// header instead switches to delegated mode for an authorized actor — see the
// TestCheckDelegatedMode* cases; the nil-impersonation denial here is the
// backward-compatible default.)
func TestCheckInboundImpersonationHeaderDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		// Impersonation is nil ⇒ delegated impersonation disabled.
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

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

// TestCheckInboundGroupsHeaderDenies asserts that a request carrying a
// client-supplied copy of the configured groups header (default
// x-impersonate-groups, and a custom name) is denied fail-closed (HTTP 403),
// closing the smuggling vector the Lua reject filter also guards (HOL-1416): the
// split Lua turns that header's comma list into Impersonate-Group lines, so a
// client value (e.g. x-impersonate-groups: system:masters) must never survive — and
// a request mapping to zero groups emits no groups header to overwrite it.
func TestCheckInboundGroupsHeaderDenies(t *testing.T) {
	const host = "api.example.com"
	cases := map[string]struct {
		groupsHeader string
		inbound      string
	}{
		"default header": {groupsHeader: "", inbound: "x-impersonate-groups"},
		"custom header":  {groupsHeader: "x-custom-groups", inbound: "x-custom-groups"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := NewStore()
			store.Set(testNamespace+"/backend", &Entry{
				Host:                 host,
				Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
				CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
			})
			reader := secretReader(t, credentialSecret("creds", "imp"))
			client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, tc.groupsHeader, logr.Discard()))

			resp := mustCheck(t, client, checkRequest(host, map[string]string{
				"authorization": "Bearer good",
				tc.inbound:      "system:masters",
			}))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckUnknownHostDenies asserts an unconfigured host is denied with HTTP 403.
func TestCheckUnknownHostDenies(t *testing.T) {
	client := serveCheck(t, NewCheckServer(NewStore(), secretReader(t), nil, testNamespace, "", logr.Discard()))

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
	client := serveCheck(t, NewCheckServer(store, secretReader(t), nil, testNamespace, "", logr.Discard()))

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
	client := serveCheck(t, NewCheckServer(store, secretReader(t), nil, testNamespace, "", logr.Discard()))

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
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

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
	client := serveCheck(t, NewCheckServer(store, secretReader(t), nil, testNamespace, "", logr.Discard()))

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
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	respA := mustCheck(t, client, checkRequest("a.example.com", map[string]string{"authorization": "Bearer x"}))
	assertHeaderOptions(t, respA.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(defaultGroupsHeader, "team-a"),
		overwriteHeader(headerAuthorization, "Bearer tok-a"),
	})

	respB := mustCheck(t, client, checkRequest("b.example.com", map[string]string{"authorization": "Bearer y"}))
	assertHeaderOptions(t, respB.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "bob"),
		overwriteHeader(defaultGroupsHeader, "team-b"),
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
	client := serveCheck(t, NewCheckServer(store, secretReader(t, secret), nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer x"}))
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(headerAuthorization, "Bearer custom-key-token"),
	})
}

// TestCheckServiceAccountSourceMintsToken asserts that when a backend's Entry
// carries a ServiceAccountRef, the Check path resolves its impersonator credential
// by minting a token via the TokenManager (the serviceAccountRef source) rather
// than reading a credential Secret. The minted token is what is forwarded as the
// upstream Authorization.
func TestCheckServiceAccountSourceMintsToken(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:              host,
		Authenticator:     newTestAuthenticator(t, map[string]any{"sub": "alice", "groups": []any{"dev"}}, "sub"),
		ServiceAccountRef: &authenticatorv1alpha1.ServiceAccountReference{Name: "impersonator", ExpirationSeconds: ptrInt64(3600)},
	})

	// A reader with NO Secret proves the SA path is taken: a Secret read would fail.
	srv := &CheckServer{
		store:        store,
		reader:       secretReader(t),
		tokenManager: NewTokenManager(&fakeMintClient{token: "minted-sa-token"}, testNamespace),
		namespace:    testNamespace,
		log:          logr.Discard(),
	}
	client := serveCheck(t, srv)

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer caller"}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(defaultGroupsHeader, "dev"),
		overwriteHeader(headerAuthorization, "Bearer minted-sa-token"),
	})
}

// TestCheckServiceAccountPreferredOverSecret asserts that if an Entry somehow
// carries BOTH a ServiceAccountRef and a CredentialsSecretRef (the CRD CEL rule
// rejects this, but the Check path defends it at runtime), the ServiceAccount
// source wins deterministically — the minted token is forwarded, not the Secret's.
func TestCheckServiceAccountPreferredOverSecret(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		ServiceAccountRef:    &authenticatorv1alpha1.ServiceAccountReference{Name: "impersonator", ExpirationSeconds: ptrInt64(3600)},
	})
	reader := secretReader(t, credentialSecret("creds", "secret-token"))
	srv := &CheckServer{
		store:        store,
		reader:       reader,
		tokenManager: NewTokenManager(&fakeMintClient{token: "minted-sa-token"}, testNamespace),
		namespace:    testNamespace,
		log:          logr.Discard(),
	}
	client := serveCheck(t, srv)

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer caller"}))
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(headerAuthorization, "Bearer minted-sa-token"),
	})
}

// TestCheckServiceAccountMintFailureDenies asserts a TokenRequest mint failure on
// the serviceAccountRef path denies fail-closed (HTTP 403), exactly as a missing
// credential Secret does — the caller is authenticated but the authorizer cannot
// mint the impersonator credential.
func TestCheckServiceAccountMintFailureDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:              host,
		Authenticator:     newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		ServiceAccountRef: &authenticatorv1alpha1.ServiceAccountReference{Name: "impersonator", ExpirationSeconds: ptrInt64(3600)},
	})
	srv := &CheckServer{
		store:        store,
		reader:       secretReader(t),
		tokenManager: NewTokenManager(&fakeMintClient{err: fmt.Errorf("forbidden")}, testNamespace),
		namespace:    testNamespace,
		log:          logr.Discard(),
	}
	client := serveCheck(t, srv)

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer caller"}))
	assertDenied(t, resp, typev3.StatusCode_Forbidden)
}

// TestCheckServiceAccountNoTokenManagerDenies asserts that a ServiceAccount-backed
// backend served by a CheckServer with no TokenManager (no writable client wired)
// denies fail-closed rather than panicking — the resolveCredential nil-manager
// guard.
func TestCheckServiceAccountNoTokenManagerDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:              host,
		Authenticator:     newTestAuthenticator(t, map[string]any{"sub": "alice"}, "sub"),
		ServiceAccountRef: &authenticatorv1alpha1.ServiceAccountReference{Name: "impersonator", ExpirationSeconds: ptrInt64(3600)},
	})
	// nil writer ⇒ no TokenManager.
	client := serveCheck(t, NewCheckServer(store, secretReader(t), nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer caller"}))
	assertDenied(t, resp, typev3.StatusCode_Forbidden)
}

// ptrInt64 returns a pointer to v, for the ServiceAccountReference.ExpirationSeconds
// pointer field in test fixtures.
func ptrInt64(v int64) *int64 { return &v }

// newImpersonationAuthenticator builds an Authenticator whose verifier returns the
// given claims, mapping the "groups" claim to Kubernetes groups (self mode) and
// resolving the given actorExtra mappings into Identity.ActorExtra — the delegated-
// impersonation fixtures need both the actor's groups (to gate authz) and its
// actorExtra (stamped into reserved headers).
func newImpersonationAuthenticator(t *testing.T, claims map[string]any, usernameClaim string, actorExtra []authenticatorv1alpha1.ExtraMapping) *Authenticator {
	t.Helper()
	mapper, err := NewGroupMapper(DefaultGroupExpression("groups", ""))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	return NewAuthenticator(&fakeVerifier{claims: claims}, mapper, usernameClaim, "", "", nil, actorExtra)
}

// resolvedImpersonation builds a *ResolvedImpersonation for the Entry from an
// allowlisted group set and actorExtra mappings, mirroring what the reconciler
// stores (HOL-1432).
func resolvedImpersonation(groups []string, actorExtra []authenticatorv1alpha1.ExtraMapping) *ResolvedImpersonation {
	set := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		set[g] = struct{}{}
	}
	return &ResolvedImpersonation{Groups: set, ActorExtra: actorExtra}
}

// actorExtraEmailUID is the actorExtra mapping used across the delegated tests: the
// actor's email and sub are stamped into reserved Impersonate-Extra-actor-email /
// Impersonate-Extra-actor-uid headers.
var actorExtraEmailUID = []authenticatorv1alpha1.ExtraMapping{
	{Key: "actor-email", ValueClaim: "email"},
	{Key: "actor-uid", ValueClaim: "sub"},
}

// TestCheckSelfModeEmitsActorExtra asserts case (a): with no inbound Impersonate-*
// header, a Backend that configures spec.impersonation.actorExtra is served in self
// mode, emitting the DERIVED identity headers (user, groups) PLUS the actor-identity
// Impersonate-Extra-<actorKey> headers — the actor identity is always recorded, even
// in self mode (HOL-1433 backward-compat criterion).
func TestCheckSelfModeEmitsActorExtra(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "alice", "email": "alice@example.com", "groups": []any{"platform-admins"},
		}, "sub", actorExtraEmailUID),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{"authorization": "Bearer caller"}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	// Derived user + groups, then the actorExtra headers in lexical key order, then
	// Authorization.
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "alice"),
		overwriteHeader(defaultGroupsHeader, "platform-admins"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-email", "alice@example.com"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-uid", "alice"),
		overwriteHeader(headerAuthorization, "Bearer impersonator-token"),
	})
}

// TestCheckDelegatedModeAuthorizedActor asserts case (b): an authorized actor (mapped
// groups intersect spec.impersonation.groups) supplying Impersonate-User and two
// --as-group values (arriving Envoy-comma-joined) is served in delegated mode. The
// actor-supplied target user passes through verbatim, the groups round-trip through
// the comma-joined groups header, the actor identity is stamped into the reserved
// actorExtra headers, the DERIVED actor identity (its own user/groups) is absent, and
// Authorization carries the impersonator token.
func TestCheckDelegatedModeAuthorizedActor(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
		}, "sub", actorExtraEmailUID),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization":     "Bearer caller",
		"impersonate-user":  "target-user",
		"impersonate-group": "dev,ops", // two --as-group values, Envoy-comma-joined
	}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	// Passed-through target user, the passthrough groups via the groups header, the
	// actor-identity reserved extras (lexical order), then Authorization. The actor's
	// OWN derived user ("actor-sub") and groups ("platform-admins") must NOT appear.
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "target-user"),
		overwriteHeader(defaultGroupsHeader, "dev,ops"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-email", "actor@example.com"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-uid", "actor-sub"),
		overwriteHeader(headerAuthorization, "Bearer impersonator-token"),
	})
}

// TestCheckDelegatedModeForwardsTargetUIDAndExtra asserts the actor-supplied
// Impersonate-Uid and a non-reserved Impersonate-Extra-* target field are forwarded
// verbatim alongside the target user, while the reserved actor-extra headers still
// carry the ACTOR identity (HOL-1433 passthrough criterion).
func TestCheckDelegatedModeForwardsTargetUIDAndExtra(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
		}, "sub", actorExtraEmailUID),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization":                "Bearer caller",
		"impersonate-user":             "target-user",
		"impersonate-uid":              "target-uid",
		"impersonate-extra-department": "sales", // non-reserved target extra
	}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	// Target user, target uid, the passthrough non-reserved extra, then the reserved
	// actor extras (lexical), then Authorization. No groups header (no --as-group).
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "target-user"),
		overwriteHeader(headerImpersonateUID, "target-uid"),
		overwriteHeader(headerImpersonateExtraPrefix+"department", "sales"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-email", "actor@example.com"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-uid", "actor-sub"),
		overwriteHeader(headerAuthorization, "Bearer impersonator-token"),
	})
}

// TestCheckDelegatedModeUsesImpersonatorCredentialFromServiceAccount asserts case (b)
// on the serviceAccountRef credential source: delegated mode uses the same
// resolveCredential path as self mode, so the minted SA token is what backs the
// impersonation and is written to Authorization.
func TestCheckDelegatedModeUsesImpersonatorCredentialFromServiceAccount(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
		}, "sub", actorExtraEmailUID),
		ServiceAccountRef: &authenticatorv1alpha1.ServiceAccountReference{Name: "impersonator", ExpirationSeconds: ptrInt64(3600)},
		Impersonation:     resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	srv := &CheckServer{
		store:        store,
		reader:       secretReader(t), // no Secret ⇒ proves the SA path is taken
		tokenManager: NewTokenManager(&fakeMintClient{token: "minted-sa-token"}, testNamespace),
		namespace:    testNamespace,
		log:          logr.Discard(),
	}
	client := serveCheck(t, srv)

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization":    "Bearer caller",
		"impersonate-user": "target-user",
	}))
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "target-user"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-email", "actor@example.com"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-uid", "actor-sub"),
		overwriteHeader(headerAuthorization, "Bearer minted-sa-token"),
	})
}

// TestCheckDelegatedModeUnauthorizedActorDenies asserts case (c): an actor whose
// mapped groups do NOT intersect spec.impersonation.groups is denied 403 when it
// supplies an Impersonate-* header — an unauthorized actor gains nothing.
func TestCheckDelegatedModeUnauthorizedActorDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"unprivileged"},
		}, "sub", actorExtraEmailUID),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization":    "Bearer caller",
		"impersonate-user": "target-user",
	}))
	assertDenied(t, resp, typev3.StatusCode_Forbidden)
}

// TestCheckDelegatedModeNilImpersonationDenies asserts case (d): a Backend with
// spec.impersonation nil denies any inbound Impersonate-* header fail-closed exactly
// as before HOL-1433 — backward compatibility. Even a caller whose groups would be
// allowlisted by some other backend gets no delegated behavior here.
func TestCheckDelegatedModeNilImpersonationDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host:                 host,
		Authenticator:        newTestAuthenticator(t, map[string]any{"sub": "alice", "groups": []any{"platform-admins"}}, "sub"),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		// Impersonation is nil ⇒ delegated impersonation disabled.
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	for _, name := range []string{"impersonate-user", "impersonate-group", "impersonate-uid", "impersonate-extra-scopes"} {
		t.Run(name, func(t *testing.T) {
			resp := mustCheck(t, client, checkRequest(host, map[string]string{
				"authorization": "Bearer caller",
				name:            "target",
			}))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckReservedActorExtraHeaderDeniesBothModes asserts case (e): an inbound copy
// of a reserved actor-extra header (Impersonate-Extra-actor-email) is rejected 403 —
// even for an otherwise-authorized actor, and both when it would be self mode (the
// reserved header is the only Impersonate-* present, but it is reserved so it is not a
// mode switch) and delegated mode (a real target header is also present). An actor
// must never be able to spoof their own actor-identity headers.
func TestCheckReservedActorExtraHeaderDeniesBothModes(t *testing.T) {
	const host = "api.example.com"
	newStore := func() *Store {
		store := NewStore()
		store.Set(testNamespace+"/backend", &Entry{
			Host: host,
			Authenticator: newImpersonationAuthenticator(t, map[string]any{
				"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
			}, "sub", actorExtraEmailUID),
			CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
			Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
		})
		return store
	}
	cases := map[string]map[string]string{
		"self-mode-only-reserved": {
			"authorization":                 "Bearer caller",
			"impersonate-extra-actor-email": "spoofed@evil.example.com",
		},
		"delegated-mode-with-reserved": {
			"authorization":               "Bearer caller",
			"impersonate-user":            "target-user",
			"impersonate-extra-actor-uid": "spoofed-uid",
		},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			reader := secretReader(t, credentialSecret("creds", "imp"))
			client := serveCheck(t, NewCheckServer(newStore(), reader, nil, testNamespace, "", logr.Discard()))
			resp := mustCheck(t, client, checkRequest(host, headers))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckDelegatedModeUnsafePassthroughGroupDenies asserts case (f): a delegated
// request whose actor-supplied groups contain an element unsafe under the comma-joined
// groups encoding (a comma-bearing or surrounding-whitespace element) is denied 403,
// the same firstUnsafeGroup guard applied to derived groups. A literal comma inside a
// group value cannot be represented on the Envoy-comma-joined passthrough path.
func TestCheckDelegatedModeUnsafePassthroughGroupDenies(t *testing.T) {
	const host = "api.example.com"
	cases := map[string]string{
		// A single element with surrounding whitespace: the split filter would trim it
		// into the bare group " system:masters" → "system:masters".
		"leading-space": "dev, system:masters",
		// Two commas cannot distinguish an intended literal-comma group from two
		// groups; a trailing-space element is likewise unsafe.
		"trailing-space": "system:masters ,dev",
	}
	for name, inbound := range cases {
		t.Run(name, func(t *testing.T) {
			store := NewStore()
			store.Set(testNamespace+"/backend", &Entry{
				Host: host,
				Authenticator: newImpersonationAuthenticator(t, map[string]any{
					"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
				}, "sub", actorExtraEmailUID),
				CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
				Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
			})
			reader := secretReader(t, credentialSecret("creds", "imp"))
			client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

			resp := mustCheck(t, client, checkRequest(host, map[string]string{
				"authorization":     "Bearer caller",
				"impersonate-user":  "target-user",
				"impersonate-group": inbound,
			}))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckDelegatedModeMultipleGroupsRoundTrip asserts case (g): multiple --as-group
// values arriving Envoy-comma-joined as one impersonate-group header are re-emitted as
// the single comma-joined groups header, which the paired Lua split filter unpacks
// into one Impersonate-Group line per group. Three groups round-trip alongside the
// required target user (Kubernetes rejects group-only impersonation, so a target user
// is always present on a valid delegated request).
func TestCheckDelegatedModeMultipleGroupsRoundTrip(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
		}, "sub", actorExtraEmailUID),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	reader := secretReader(t, credentialSecret("creds", "impersonator-token"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	resp := mustCheck(t, client, checkRequest(host, map[string]string{
		"authorization":     "Bearer caller",
		"impersonate-user":  "target-user",
		"impersonate-group": "dev,ops,sre",
	}))
	if got, want := codes.Code(resp.GetStatus().GetCode()), codes.OK; got != want {
		t.Fatalf("status code = %v, want %v", got, want)
	}
	// Target user, the three groups as one CSV value, the actor extras, then
	// Authorization.
	assertHeaderOptions(t, resp.GetOkResponse().GetHeaders(), []*corev3.HeaderValueOption{
		overwriteHeader(headerImpersonateUser, "target-user"),
		overwriteHeader(defaultGroupsHeader, "dev,ops,sre"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-email", "actor@example.com"),
		overwriteHeader(headerImpersonateExtraPrefix+"actor-uid", "actor-sub"),
		overwriteHeader(headerAuthorization, "Bearer impersonator-token"),
	})
}

// TestCheckDelegatedModeRequiresTargetUser asserts that an authorized actor supplying
// only groups/UID/extras but NO Impersonate-User is denied 403: Kubernetes rejects
// impersonation without a target user, so the authorizer must not emit an OK that
// swaps in the impersonator credential for a request that cannot succeed upstream
// (which would leave the impersonator ServiceAccount acting as itself).
func TestCheckDelegatedModeRequiresTargetUser(t *testing.T) {
	const host = "api.example.com"
	cases := map[string]map[string]string{
		"group-only": {"authorization": "Bearer caller", "impersonate-group": "dev,ops"},
		"uid-only":   {"authorization": "Bearer caller", "impersonate-uid": "target-uid"},
		"extra-only": {"authorization": "Bearer caller", "impersonate-extra-department": "sales"},
		"empty-user": {"authorization": "Bearer caller", "impersonate-user": "", "impersonate-group": "dev"},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			store := NewStore()
			store.Set(testNamespace+"/backend", &Entry{
				Host: host,
				Authenticator: newImpersonationAuthenticator(t, map[string]any{
					"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
				}, "sub", actorExtraEmailUID),
				CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
				Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
			})
			reader := secretReader(t, credentialSecret("creds", "imp"))
			client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

			resp := mustCheck(t, client, checkRequest(host, headers))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
	}
}

// TestCheckDelegatedModeUnrecognizedHeaderDenies asserts that an authorized actor
// supplying an unrecognized Impersonate-* header (one delegatedResponse does not
// forward, e.g. a typo'd "Impersonate-Foo") is denied 403 even alongside a valid
// target user — allowing it would flip to delegated mode yet drop the unrecognized
// header, and if it were the ONLY impersonation intent it would leave the
// impersonator SA acting as itself. Rejecting surfaces the misconfiguration.
func TestCheckDelegatedModeUnrecognizedHeaderDenies(t *testing.T) {
	const host = "api.example.com"
	store := NewStore()
	store.Set(testNamespace+"/backend", &Entry{
		Host: host,
		Authenticator: newImpersonationAuthenticator(t, map[string]any{
			"sub": "actor-sub", "email": "actor@example.com", "groups": []any{"platform-admins"},
		}, "sub", actorExtraEmailUID),
		CredentialsSecretRef: authenticatorv1alpha1.SecretReference{Name: "creds"},
		Impersonation:        resolvedImpersonation([]string{"platform-admins"}, actorExtraEmailUID),
	})
	reader := secretReader(t, credentialSecret("creds", "imp"))
	client := serveCheck(t, NewCheckServer(store, reader, nil, testNamespace, "", logr.Discard()))

	// Both an unrecognized header alone and one alongside a valid target are denied.
	cases := map[string]map[string]string{
		"unrecognized-only": {
			"authorization":   "Bearer caller",
			"impersonate-foo": "bar",
		},
		"unrecognized-with-valid-target": {
			"authorization":    "Bearer caller",
			"impersonate-user": "target-user",
			"impersonate-foo":  "bar",
		},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			resp := mustCheck(t, client, checkRequest(host, headers))
			assertDenied(t, resp, typev3.StatusCode_Forbidden)
		})
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
		Check:    NewCheckServer(NewStore(), secretReader(t), nil, testNamespace, "", logr.Discard()),
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
// option's header name, value, append action, and the deprecated append bool. The
// append bool is what Envoy's ext_authz path actually reads to choose append vs.
// overwrite, so it is part of the contract, not incidental — every header the
// authorizer now emits uses the overwrite/set action with the bool unset (HOL-1416).
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
		//nolint:staticcheck // SA1019: ext_authz reads the deprecated append bool (HOL-1414)
		gotAppend, wantAppend := got[i].GetAppend().GetValue(), want[i].GetAppend().GetValue()
		if gotAppend != wantAppend {
			t.Errorf("header[%d] %q append bool = %v, want %v", i,
				gh.GetKey(), gotAppend, wantAppend)
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

// TestRedactHeaderValue asserts the Authorization value (the impersonator
// credential) is replaced with a length-only marker while every other header value
// is returned verbatim, and that the Authorization match is case-insensitive (Envoy
// lowercases header keys on the wire).
func TestRedactHeaderValue(t *testing.T) {
	cases := []struct {
		name, header, value, want string
	}{
		{"authorization redacted", headerAuthorization, "Bearer secret-token", "<redacted 19-byte credential>"},
		{"authorization case-insensitive", "authorization", "Bearer s", "<redacted 8-byte credential>"},
		{"impersonate user verbatim", headerImpersonateUser, "alice", "alice"},
		{"groups header verbatim", defaultGroupsHeader, "dev,ops", "dev,ops"},
		{"www-authenticate verbatim", headerWWWAuthenticate, wwwAuthenticateBearer, wwwAuthenticateBearer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactHeaderValue(tc.header, tc.value); got != tc.want {
				t.Errorf("redactHeaderValue(%q, %q) = %q, want %q", tc.header, tc.value, got, tc.want)
			}
		})
	}
	// The redaction must never leak the credential itself.
	if got := redactHeaderValue(headerAuthorization, "Bearer super-secret"); strings.Contains(got, "super-secret") {
		t.Errorf("redacted Authorization value %q leaks the credential", got)
	}
}

// TestLogResponseHeadersLogsEachOkHeader asserts the debug hook emits one summary
// line plus one line per header an OK response carries, with each header's name,
// append action, and the deprecated append bool, and with the Authorization token
// redacted (HOL-1415).
func TestLogResponseHeadersLogsEachOkHeader(t *testing.T) {
	sink := newCaptureSink(true)
	s := &CheckServer{log: logr.New(sink)}

	identity := &Identity{Username: "alice", Groups: []string{"dev", "ops"}}
	s.logResponseHeaders("api.example.com", s.okResponse(identity, "impersonator-token"))

	summary := sink.find(t, "returning response headers to caller")
	if got, want := summary.kv["decision"], "ok"; got != want {
		t.Errorf("summary decision = %v, want %v", got, want)
	}
	if got, want := summary.kv["headerCount"], 3; got != want {
		t.Errorf("summary headerCount = %v, want %v", got, want)
	}

	headerLines := sink.findAll("response header")
	if got, want := len(headerLines), 3; got != want {
		t.Fatalf("response header line count = %d, want %d", got, want)
	}

	// Impersonate-User overwrite, the single comma-joined groups header (overwrite,
	// append bool false — HOL-1416), Authorization overwrite with the token redacted.
	wantLines := []struct {
		name, value  string
		appendBool   bool
		appendAction string
	}{
		{headerImpersonateUser, "alice", false, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD.String()},
		{defaultGroupsHeader, "dev,ops", false, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD.String()},
		{headerAuthorization, "<redacted 25-byte credential>", false, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD.String()},
	}
	for i, want := range wantLines {
		line := headerLines[i]
		if got := line.kv["name"]; got != want.name {
			t.Errorf("header[%d] name = %v, want %v", i, got, want.name)
		}
		if got := line.kv["value"]; got != want.value {
			t.Errorf("header[%d] value = %v, want %v", i, got, want.value)
		}
		if got := line.kv["append"]; got != want.appendBool {
			t.Errorf("header[%d] append bool = %v, want %v", i, got, want.appendBool)
		}
		if got := line.kv["appendAction"]; got != want.appendAction {
			t.Errorf("header[%d] appendAction = %v, want %v", i, got, want.appendAction)
		}
	}

	// The bearer token must never appear in any logged value.
	for _, line := range headerLines {
		if v, ok := line.kv["value"].(string); ok && strings.Contains(v, "impersonator-token") {
			t.Errorf("logged header value %q leaks the impersonator token", v)
		}
	}
}

// TestLogResponseHeadersDeniedReportsStatus asserts a denied response logs the
// denied decision, the HTTP status the client receives, and any challenge header it
// carries (HOL-1415).
func TestLogResponseHeadersDeniedReportsStatus(t *testing.T) {
	sink := newCaptureSink(true)
	s := &CheckServer{log: logr.New(sink)}

	resp := deniedResponse(
		typev3.StatusCode_Unauthorized,
		"missing bearer token",
		[]*corev3.HeaderValueOption{overwriteHeader(headerWWWAuthenticate, wwwAuthenticateBearer)},
	)
	s.logResponseHeaders("api.example.com", resp)

	summary := sink.find(t, "returning response headers to caller")
	if got, want := summary.kv["decision"], "denied"; got != want {
		t.Errorf("summary decision = %v, want %v", got, want)
	}
	if got, want := summary.kv["status"], typev3.StatusCode_Unauthorized.String(); got != want {
		t.Errorf("summary status = %v, want %v", got, want)
	}

	line := sink.find(t, "response header")
	if got, want := line.kv["name"], headerWWWAuthenticate; got != want {
		t.Errorf("header name = %v, want %v", got, want)
	}
}

// TestLogResponseHeadersDisabledNoOp asserts the hook emits nothing when V(1) debug
// logging is disabled, so it costs nothing on the normal path.
func TestLogResponseHeadersDisabledNoOp(t *testing.T) {
	sink := newCaptureSink(false)
	s := &CheckServer{log: logr.New(sink)}

	s.logResponseHeaders("api.example.com", s.okResponse(&Identity{Username: "alice"}, "tok"))

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("logged %d entries with debug disabled, want 0", got)
	}
}

// captureEntry is one captured log line: its message and the key/value pairs
// flattened into a map for assertion.
type captureEntry struct {
	msg string
	kv  map[string]any
}

// captureSink is a logr.LogSink that records Info entries for assertion. enabled
// controls whether V(1) lines are emitted, modeling the debug-verbosity gate.
type captureSink struct {
	mu      sync.Mutex
	enabled bool
	entries []captureEntry
}

func newCaptureSink(enabled bool) *captureSink { return &captureSink{enabled: enabled} }

func (s *captureSink) Init(logr.RuntimeInfo) {}

func (s *captureSink) Enabled(int) bool { return s.enabled }

func (s *captureSink) Info(_ int, msg string, kv ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		m[key] = kv[i+1]
	}
	s.entries = append(s.entries, captureEntry{msg: msg, kv: m})
}

func (s *captureSink) Error(error, string, ...any) {}

func (s *captureSink) WithValues(...any) logr.LogSink { return s }

func (s *captureSink) WithName(string) logr.LogSink { return s }

func (s *captureSink) snapshot() []captureEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]captureEntry(nil), s.entries...)
}

// find returns the first captured entry whose message equals msg, failing the test
// if none was recorded.
func (s *captureSink) find(t *testing.T, msg string) captureEntry {
	t.Helper()
	for _, e := range s.snapshot() {
		if e.msg == msg {
			return e
		}
	}
	t.Fatalf("no log entry with message %q", msg)
	return captureEntry{}
}

// findAll returns every captured entry whose message equals msg, in order.
func (s *captureSink) findAll(msg string) []captureEntry {
	var out []captureEntry
	for _, e := range s.snapshot() {
		if e.msg == msg {
			out = append(out, e)
		}
	}
	return out
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

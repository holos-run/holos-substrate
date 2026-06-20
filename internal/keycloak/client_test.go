package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// tokenPath is the conventional OAuth2 token endpoint the test server serves so
// the client's client_credentials grant succeeds before any admin call.
const tokenPath = "/realms/holos/protocol/openid-connect/token"

// recordingHandler captures the request a single admin call makes and replies
// with a canned status and body, so each test asserts method/path/headers/body
// and the client's decoding of the response in one place. It transparently
// answers the token endpoint so the client can authenticate first.
type recordingHandler struct {
	t          *testing.T
	wantMethod string
	wantPath   string
	status     int
	respBody   string
	// location, when set, is returned as the Location header (create endpoints).
	location string

	// captured request fields
	gotMethod  string
	gotPath    string
	gotAuth    string
	gotContent string
	gotAccept  string
	gotBody    map[string]any
	gotArray   []any
	gotRawBody string
	gotEscaped string
	gotQuery   string

	tokenHits int32
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == tokenPath {
		atomic.AddInt32(&h.tokenHits, 1)
		writeToken(w)
		return
	}
	h.gotMethod = r.Method
	h.gotPath = r.URL.Path
	h.gotEscaped = r.URL.EscapedPath()
	h.gotQuery = r.URL.RawQuery
	h.gotAuth = r.Header.Get("Authorization")
	h.gotContent = r.Header.Get("Content-Type")
	h.gotAccept = r.Header.Get("Accept")
	body, _ := io.ReadAll(r.Body)
	h.gotRawBody = string(body)
	if len(body) > 0 {
		// Try object then array, since some endpoints take an array body.
		if err := json.Unmarshal(body, &h.gotBody); err != nil {
			if err2 := json.Unmarshal(body, &h.gotArray); err2 != nil {
				h.t.Errorf("request body is not valid JSON: %v (%q)", err, string(body))
			}
		}
	}
	if h.location != "" {
		w.Header().Set("Location", h.location)
	}
	if h.status == 0 {
		h.status = http.StatusOK
	}
	w.WriteHeader(h.status)
	if h.respBody != "" {
		_, _ = io.WriteString(w, h.respBody)
	}
}

// writeToken answers the OAuth2 token endpoint with a usable access token.
func writeToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"access_token":"admin-token","expires_in":300}`)
}

// testCreds is the client_credentials material every test client authenticates
// with.
func testCreds() Credentials {
	return Credentials{ClientID: "holos-controller", ClientSecret: "s3cr3t"}
}

// newTestClient spins up an httptest server with the given handler and returns a
// Client (realm "holos") pointed at it. The caller's handler must answer the
// token endpoint (recordingHandler and muxHandler both do).
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "holos", testCreds(), srv.Client()), srv
}

// assertCommonRequest checks the auth header and (when a body is sent) the
// content type, which every JSON admin request must carry.
func assertCommonRequest(t *testing.T, h *recordingHandler, expectBody bool) {
	t.Helper()
	if h.gotMethod != h.wantMethod {
		t.Errorf("method = %q, want %q", h.gotMethod, h.wantMethod)
	}
	if h.gotPath != h.wantPath {
		t.Errorf("path = %q, want %q", h.gotPath, h.wantPath)
	}
	if h.gotAuth != "Bearer admin-token" {
		t.Errorf("Authorization = %q, want %q", h.gotAuth, "Bearer admin-token")
	}
	if h.gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", h.gotAccept)
	}
	if expectBody && h.gotContent != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", h.gotContent)
	}
	if !expectBody && h.gotRawBody != "" {
		t.Errorf("expected no request body, got %q", h.gotRawBody)
	}
}

// muxHandler routes by method+path so a single test can drive a sequence of
// distinct requests. It answers the token endpoint transparently.
type muxHandler struct {
	t       *testing.T
	routes  map[string]func(w http.ResponseWriter, r *http.Request)
	unknown int
}

func (m *muxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == tokenPath {
		writeToken(w)
		return
	}
	key := r.Method + " " + r.URL.Path
	if fn, ok := m.routes[key]; ok {
		fn(w, r)
		return
	}
	m.unknown++
	m.t.Errorf("unexpected request %s", key)
	w.WriteHeader(http.StatusInternalServerError)
}

func TestNewClientNormalizesBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"server root", "https://kc.example.com", "https://kc.example.com"},
		{"trailing slash", "https://kc.example.com/", "https://kc.example.com"},
		{"admin root", "https://kc.example.com/admin", "https://kc.example.com"},
		{"admin root trailing slash", "https://kc.example.com/admin/", "https://kc.example.com"},
		{"subpath instance", "https://host/auth", "https://host/auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(tc.in, "holos", testCreds(), nil)
			if c.baseURL != tc.want {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.want)
			}
		})
	}
}

func TestNewClientNilClientGetsTimeout(t *testing.T) {
	c := NewClient("https://kc.example.com", "holos", testCreds(), nil)
	if c.httpClient == http.DefaultClient {
		t.Error("nil http.Client must not be the timeout-less http.DefaultClient")
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("default client Timeout = %v, want %v", c.httpClient.Timeout, defaultTimeout)
	}
}

func TestTokenURLDerivedAndOverride(t *testing.T) {
	c := NewClient("https://kc.example.com/", "holos", testCreds(), nil)
	want := "https://kc.example.com/realms/holos/protocol/openid-connect/token"
	if got := c.tokenURL(); got != want {
		t.Errorf("derived tokenURL = %q, want %q", got, want)
	}
	c2 := NewClient("https://kc.example.com", "holos", Credentials{TokenURL: "https://idp/token"}, nil)
	if got := c2.tokenURL(); got != "https://idp/token" {
		t.Errorf("override tokenURL = %q, want the explicit value", got)
	}
}

func TestClientAuthenticatesWithClientCredentials(t *testing.T) {
	var grantType, clientID, clientSecret string
	m := &muxHandler{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET " + adminPathPrefix + "/realms/holos/groups/g1": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"g1","name":"owner"}`)
		},
	}}
	// Override the token route to capture the grant body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			body, _ := io.ReadAll(r.Body)
			vals, _ := parseForm(string(body))
			grantType = vals["grant_type"]
			clientID = vals["client_id"]
			clientSecret = vals["client_secret"]
			writeToken(w)
			return
		}
		m.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "holos", testCreds(), srv.Client())

	if _, err := c.GetGroup(context.Background(), "g1"); err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if grantType != "client_credentials" {
		t.Errorf("grant_type = %q, want client_credentials", grantType)
	}
	if clientID != "holos-controller" || clientSecret != "s3cr3t" {
		t.Errorf("creds = %q/%q, want holos-controller/s3cr3t", clientID, clientSecret)
	}
}

func TestClientCachesToken(t *testing.T) {
	h := &recordingHandler{t: t, wantMethod: http.MethodGet, wantPath: adminPathPrefix + "/realms/holos/groups/g1", status: http.StatusOK, respBody: `{"id":"g1"}`}
	c, _ := newTestClient(t, h)

	for i := 0; i < 3; i++ {
		if _, err := c.GetGroup(context.Background(), "g1"); err != nil {
			t.Fatalf("GetGroup #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&h.tokenHits); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (token must be cached)", got)
	}
}

func TestTokenEndpointErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"bad secret"}`)
			return
		}
		t.Errorf("admin endpoint must not be reached when auth fails: %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "holos", testCreds(), srv.Client())

	_, err := c.GetGroup(context.Background(), "g1")
	if err == nil {
		t.Fatal("expected an auth error")
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected a 401 *APIError, got %v", err)
	}
	if ae.Message != "bad secret" {
		t.Errorf("Message = %q, want the error_description surfaced", ae.Message)
	}
}

func TestIDFromLocation(t *testing.T) {
	cases := map[string]string{
		"https://kc/admin/realms/holos/groups/abc-123":  "abc-123",
		"https://kc/admin/realms/holos/groups/abc-123/": "abc-123",
		"abc-123": "abc-123",
		"":        "",
	}
	for in, want := range cases {
		if got := idFromLocation(in); got != want {
			t.Errorf("idFromLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServerError500(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: adminPathPrefix + "/realms/holos/groups/g1",
		status:   http.StatusInternalServerError,
		respBody: `{"errorMessage":"boom"}`,
	}
	c, _ := newTestClient(t, h)

	_, err := c.GetGroup(context.Background(), "g1")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if IsNotFound(err) || IsConflict(err) {
		t.Errorf("500 must be neither not-found nor conflict: %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *APIError: %v", err)
	}
	if ae.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", ae.StatusCode)
	}
	if ae.Message != "boom" {
		t.Errorf("Message = %q, want the errorMessage surfaced", ae.Message)
	}
	if ae.Body != `{"errorMessage":"boom"}` {
		t.Errorf("Body should surface raw body, got %q", ae.Body)
	}
}

// parseForm is a tiny URL-encoded form parser for the token grant body, avoiding
// a net/url import in the assertion path.
func parseForm(body string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(body, "&") {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		k := kv[0]
		v := ""
		if len(kv) == 2 {
			v = kv[1]
		}
		out[urlDecode(k)] = urlDecode(v)
	}
	return out, nil
}

func urlDecode(s string) string {
	// The values used in tests have no escapes except the secret/clientid which
	// are plain; a minimal decode of '+' to space suffices here.
	return strings.ReplaceAll(s, "+", " ")
}

// decodeJSONObject unmarshals a request body into a generic map for assertion in
// tests that capture a POST body off the wire.
func decodeJSONObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("request body is not a JSON object: %v (%q)", err, string(body))
	}
	return out
}

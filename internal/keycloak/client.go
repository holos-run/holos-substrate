// Package keycloak is a focused REST client for the Keycloak 26.x Admin REST
// API, covering exactly the realm, group, user, client, client-role, and
// fine-grained-permission operations the holos-controller KeycloakInstance,
// KeycloakGroup, KeycloakUser, and KeycloakClient reconcilers consume
// (ADR-20).
//
// The client authenticates to the Admin API with an OAuth2 client_credentials
// grant against the instance's realm — a confidential service-account client
// holding the scoped realm-management roles (ADR-20, "Admin credential") — and
// refreshes the bearer token internally as it nears expiry. It targets Keycloak
// 26.x only (the platform runs 26.6.3): native subgroups via the children
// endpoint and Fine-Grained Admin Permissions v2 group scope.
//
// This package deliberately imports neither controller-runtime nor the
// keycloak.holos.run API types: it is a plain HTTP client so it stays
// unit-testable without a cluster, and the single seam every reconciler calls
// (the AC #3 dependency boundary, mirroring internal/quay's independence from
// api/quay).
package keycloak

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// defaultTimeout bounds requests made by the client constructed with a nil
// *http.Client. A finite timeout keeps a stalled Keycloak connection from
// pinning a reconciler worker indefinitely even when a caller forgets to set a
// context deadline.
const defaultTimeout = 30 * time.Second

// tokenExpiryLeeway is subtracted from a token's reported lifetime so the client
// refreshes shortly before the access token actually expires, avoiding a race
// where a request is sent with a token that expires in flight.
const tokenExpiryLeeway = 30 * time.Second

// adminPathPrefix is the Keycloak Admin REST API root. Every admin operation
// path the client builds is rooted at /admin/realms/{realm}, so a base URL that
// already ends in /admin is trimmed in NewClient to avoid a doubled
// .../admin/admin/... path.
const adminPathPrefix = "/admin"

// Credentials carries the OAuth2 client_credentials grant material the client
// authenticates the Admin API with: a confidential service-account client's ID
// and secret in the instance's realm (ADR-20's preferred admin credential). The
// optional TokenURL overrides the derived token endpoint for an out-of-cluster
// target whose token path differs from the conventional
// {baseURL}/realms/{realm}/protocol/openid-connect/token.
type Credentials struct {
	// ClientID is the confidential client's clientId in the realm.
	ClientID string
	// ClientSecret is that client's secret.
	ClientSecret string
	// TokenURL, when non-empty, is the exact OAuth2 token endpoint to use; when
	// empty the client derives it from the base URL and realm.
	TokenURL string
}

// Client is a typed Keycloak Admin REST API client scoped to a single realm.
// Construct it with NewClient or NewClientWithCABundle. It is safe for
// concurrent use: the cached bearer token is guarded by a mutex.
type Client struct {
	// baseURL is the Keycloak server root (e.g. https://keycloak.example.com),
	// stored without a trailing slash or /admin suffix.
	baseURL string
	// realm is the realm every admin operation targets.
	realm string
	// creds is the client_credentials grant material.
	creds Credentials
	// httpClient performs requests. Never nil after construction.
	httpClient *http.Client

	// mu guards the cached token and its expiry.
	mu sync.Mutex
	// token is the cached access token, refreshed when empty or near expiry.
	token string
	// tokenExpiry is when the cached token stops being usable (already adjusted
	// by tokenExpiryLeeway).
	tokenExpiry time.Time
}

// NewClient returns a Client targeting baseURL's Admin REST API for the given
// realm, authenticating with the OAuth2 client_credentials grant in creds.
//
// baseURL may be the Keycloak server root (https://host) or the admin root
// (https://host/admin); NewClient normalizes both to the server root by
// trimming a trailing slash and a trailing /admin, so the per-operation paths
// (which all begin /admin/realms/{realm}) never double up.
//
// If httpClient is nil, a client with a finite default timeout is used so a
// stalled Keycloak connection cannot pin a reconciler worker indefinitely;
// callers that need different transport or timeout behavior pass their own.
func NewClient(baseURL, realm string, creds Credentials, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	base := strings.TrimRight(baseURL, "/")
	base = strings.TrimSuffix(base, adminPathPrefix)
	base = strings.TrimRight(base, "/")
	return &Client{
		baseURL:    base,
		realm:      realm,
		creds:      creds,
		httpClient: httpClient,
	}
}

// ValidateCABundle reports whether a non-empty caBundle contains at least one
// parseable x509 certificate, so a caller (e.g. a reconciler) can reject an
// invalid spec.caBundle up front rather than discovering it only when building a
// client. An empty bundle is valid (it means "use system trust unchanged"). The
// error message mirrors the one NewClientWithCABundle would return.
func ValidateCABundle(caBundle []byte) error {
	if len(caBundle) == 0 {
		return nil
	}
	if !x509.NewCertPool().AppendCertsFromPEM(caBundle) {
		return fmt.Errorf("keycloak: no valid certificates found in caBundle")
	}
	return nil
}

// NewClientWithCABundle returns a Client that, in addition to the system trust
// store, trusts the x509 CA certificates in the PEM-encoded caBundle when
// establishing TLS to Keycloak. It is the constructor the holos-controller
// reconcilers use to reach the in-cluster Keycloak, whose serving certificate is
// signed by the platform's local CA rather than a public root.
//
// It builds an *http.Client (with the same finite default timeout NewClient uses
// for a nil client) whose transport's TLSClientConfig.RootCAs is the system pool
// with caBundle appended, then delegates to NewClient for base-URL
// normalization. When caBundle is empty it passes a nil *http.Client so behavior
// is identical to NewClient(baseURL, realm, creds, nil) — system trust only, no
// custom transport. An error is returned only when caBundle is non-empty but
// contains no parseable certificate.
func NewClientWithCABundle(baseURL, realm string, creds Credentials, caBundle []byte) (*Client, error) {
	httpClient, err := httpClientForCABundle(caBundle)
	if err != nil {
		return nil, err
	}
	return NewClient(baseURL, realm, creds, httpClient), nil
}

// httpClientForCABundle builds the *http.Client NewClientWithCABundle uses. When
// caBundle is empty it returns nil so the caller falls back to NewClient's
// default (system trust, default transport). Otherwise it clones
// http.DefaultTransport and overrides only TLSClientConfig.RootCAs, so the
// resulting client keeps Go's default transport behavior — HTTP_PROXY/NO_PROXY
// honoring, connection pooling, dial/TLS handshake timeouts, HTTP/2 — and merely
// trusts the extra roots. The RootCAs pool is the system pool (falling back to
// an empty pool when x509.SystemCertPool errors or returns nil, e.g. on a
// scratch image) with caBundle appended via AppendCertsFromPEM. A non-empty
// bundle that yields no certificates is an error rather than a silent no-op.
func httpClientForCABundle(caBundle []byte) (*http.Client, error) {
	if len(caBundle) == 0 {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("keycloak: no valid certificates found in caBundle")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.RootCAs = pool
	return &http.Client{Timeout: defaultTimeout, Transport: transport}, nil
}

// tokenURL returns the OAuth2 token endpoint for the client_credentials grant:
// the credential's explicit TokenURL when set, otherwise the conventional
// {baseURL}/realms/{realm}/protocol/openid-connect/token.
func (c *Client) tokenURL() string {
	if c.creds.TokenURL != "" {
		return c.creds.TokenURL
	}
	return c.baseURL + "/realms/" + url.PathEscape(c.realm) + "/protocol/openid-connect/token"
}

// tokenResponse is the OAuth2 token endpoint's JSON response. Only the access
// token and its lifetime are read.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// accessToken returns a valid bearer token, fetching a fresh one via the
// client_credentials grant when none is cached or the cached one is within
// tokenExpiryLeeway of expiry. Concurrent callers serialize on the mutex; the
// common path (a still-valid cached token) returns without a network call.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}
	tok, err := c.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	c.token = tok.AccessToken
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	c.tokenExpiry = time.Now().Add(lifetime - tokenExpiryLeeway)
	return c.token, nil
}

// fetchToken performs the OAuth2 client_credentials grant against the token
// endpoint and returns the parsed response. A non-2xx response is surfaced as an
// *APIError so callers can distinguish, e.g., bad credentials (401) from a
// transport failure.
func (c *Client) fetchToken(ctx context.Context) (*tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.creds.ClientID},
		"client_secret": {c.creds.ClientSecret},
	}
	endpoint := c.tokenURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("keycloak: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("keycloak: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("keycloak: reading token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(http.MethodPost, "token", resp.StatusCode, body)
	}
	tok := &tokenResponse{}
	if err := json.Unmarshal(body, tok); err != nil {
		return nil, fmt.Errorf("keycloak: decoding token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("keycloak: token response carried no access_token")
	}
	return tok, nil
}

// adminPath joins segment(s) under the realm's admin root,
// /admin/realms/{realm}, with the realm escaped. The suffix is appended
// verbatim; callers that interpolate IDs escape them with url.PathEscape.
func (c *Client) adminPath(suffix string) string {
	return adminPathPrefix + "/realms/" + url.PathEscape(c.realm) + suffix
}

// doJSON sends an authenticated HTTP request to path (joined to the client's
// base URL) with an optional JSON-marshaled body, and decodes a JSON response
// body into out when out is non-nil.
//
// It obtains (and refreshes) the bearer token, sets Authorization: Bearer
// <token> on every request, and, when a body is present, Content-Type:
// application/json. Any non-2xx response is returned as an *APIError carrying
// the status code and Keycloak's raw error body. A 2xx with an empty body and a
// non-nil out leaves out unmodified.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}

	var reqBody io.Reader
	hasBody := body != nil
	if hasBody {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("keycloak: marshaling request body for %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("keycloak: building request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("keycloak: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("keycloak: reading response body for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(method, path, resp.StatusCode, respBody)
	}

	if out != nil && len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("keycloak: decoding response body for %s %s: %w", method, path, err)
		}
	}
	return nil
}

// doCreate sends a POST whose success Keycloak signals with a 201 and a Location
// header naming the new resource (the Admin API convention for group/user/role
// creation). It returns the trailing path segment of the Location header — the
// created resource's id — or an empty string when no Location is present. A
// non-2xx response is an *APIError, so callers branch on IsConflict for the
// already-exists case.
func (c *Client) doCreate(ctx context.Context, path string, body any) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("keycloak: marshaling request body for POST %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("keycloak: building request POST %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak: POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("keycloak: reading response body for POST %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newAPIError(http.MethodPost, path, resp.StatusCode, respBody)
	}

	return idFromLocation(resp.Header.Get("Location")), nil
}

// doCreateReturningBody sends a POST whose success Keycloak signals with a 2xx
// and the created resource's representation in the JSON body — the Authorization
// Services endpoints (resource/policy/permission), which return the object
// (carrying its id or _id) rather than a Location header. It returns the new
// resource's id, preferring the body's id, then _id, then a Location header if
// one is also present. A non-2xx response is an *APIError so callers branch on
// IsConflict.
func (c *Client) doCreateReturningBody(ctx context.Context, path string, body any) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("keycloak: marshaling request body for POST %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("keycloak: building request POST %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak: POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("keycloak: reading response body for POST %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newAPIError(http.MethodPost, path, resp.StatusCode, respBody)
	}

	if id := idFromCreateBody(respBody); id != "" {
		return id, nil
	}
	return idFromLocation(resp.Header.Get("Location")), nil
}

// idFromCreateBody extracts the created resource id from an Authorization
// Services create response body, preferring id then _id (the resource endpoint
// uses _id; policy/permission use id). It returns "" when the body is empty or
// carries neither.
func idFromCreateBody(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var parsed struct {
		ID    string `json:"id"`
		Under string `json:"_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if parsed.ID != "" {
		return parsed.ID
	}
	return parsed.Under
}

// idFromLocation extracts the trailing path segment (the created resource id)
// from a Keycloak Admin API Location header. It returns "" when location is
// empty or unparseable.
func idFromLocation(location string) string {
	if location == "" {
		return ""
	}
	trimmed := strings.TrimRight(location, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return trimmed
	}
	return trimmed[idx+1:]
}

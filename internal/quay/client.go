// Package quay is a focused REST client for the Quay 3.17.3+ API, covering
// exactly the organization, repository, and repo_push webhook-notification
// operations the holos-controller Organization and Repository reconcilers
// consume.
//
// The client authenticates with a superuser OAuth-Application Bearer token
// (acting as svc-quay-resource-controller) and targets the standard
// (non-/superuser/) org/repo endpoints: the creating user owns the orgs it
// creates, so ordinary endpoints suffice (see the Quay resource controller
// credentials runbook, case 1). It targets Quay 3.17.3+ only — no version
// negotiation or backwards-compat shims.
//
// This package deliberately imports neither controller-runtime nor the
// quay.holos.run API types: it is a plain HTTP client so it stays unit-testable
// without a cluster, and the single seam both reconcilers call.
//
// The client does not retry requests; the controller reconciler requeue is the
// retry loop.
package quay

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// defaultTimeout bounds requests made by the client constructed with a nil
// *http.Client. A finite timeout keeps a stalled Quay connection from pinning a
// reconciler worker indefinitely even when a caller forgets to set a context
// deadline.
const defaultTimeout = 30 * time.Second

const (
	maxResponseBodyBytes = 1 << 20
	maxAPIErrorBodyBytes = 4 << 10
	truncationMarker     = "...[truncated]"
)

// apiPathPrefix is the Quay REST API root. Every operation path the client
// builds already starts with it, so a base URL that already ends in it is
// trimmed in NewClient to avoid a doubled .../api/v1/api/v1/... path.
const apiPathPrefix = "/api/v1"

// Client is a typed Quay REST API client. Construct it with NewClient. It is
// safe for concurrent use as long as the underlying *http.Client is.
type Client struct {
	// baseURL is the Quay instance root (e.g. https://quay.example.com),
	// stored without a trailing slash.
	baseURL string
	// token is the superuser OAuth-Application Bearer token.
	token string
	// httpClient performs requests. Never nil after NewClient.
	httpClient *http.Client
}

// NewClient returns a Client targeting baseURL, authenticating every request
// with the given OAuth-Application Bearer token.
//
// Credential resolution owns validation of empty baseURL and token values;
// NewClient only normalizes and stores the supplied values.
//
// baseURL may be either the Quay instance root (https://host) or the API root
// (https://host/api/v1) — the conventional value of the credential Secret's url
// key. NewClient normalizes both to the instance root by trimming a trailing
// slash and a trailing /api/v1, so the per-operation paths (which all begin
// /api/v1) never double up into .../api/v1/api/v1/....
//
// If httpClient is nil, a client with a finite default timeout is used so a
// stalled Quay connection cannot pin a reconciler worker indefinitely; callers
// that need different transport or timeout behavior pass their own.
func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	base := strings.TrimRight(baseURL, "/")
	base, _ = strings.CutSuffix(base, apiPathPrefix)
	base = strings.TrimRight(base, "/")
	return &Client{
		baseURL:    base,
		token:      token,
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
		return fmt.Errorf("quay: no valid certificates found in caBundle")
	}
	return nil
}

// NewClientWithCABundle returns a Client that, in addition to the system trust
// store, trusts the x509 CA certificates in the PEM-encoded caBundle when
// establishing TLS to the Quay API. It is the constructor the holos-controller
// reconcilers use to reach the in-cluster Quay registry, whose serving
// certificate is signed by the platform's local CA rather than a public root.
//
// It builds an *http.Client (with the same finite default timeout NewClient
// uses for a nil client) whose transport's TLSClientConfig.RootCAs is the
// system pool with caBundle appended, then delegates to NewClient for base-URL
// normalization. When caBundle is empty it passes a nil *http.Client so behavior
// is identical to NewClient(baseURL, token, nil) — system trust only, no custom
// transport. An error is returned only when caBundle is non-empty but contains
// no parseable certificate.
func NewClientWithCABundle(baseURL, token string, caBundle []byte) (*Client, error) {
	httpClient, err := httpClientForCABundle(caBundle)
	if err != nil {
		return nil, err
	}
	return NewClient(baseURL, token, httpClient), nil
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
		return nil, fmt.Errorf("quay: no valid certificates found in caBundle")
	}
	// Clone the default transport so proxy, pooling, timeout, and HTTP/2
	// behavior is preserved; only the trust roots change. http.DefaultTransport
	// is always an *http.Transport.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.RootCAs = pool
	return &http.Client{Timeout: defaultTimeout, Transport: transport}, nil
}

// doJSON sends an HTTP request to path (joined to the client's base URL) with an
// optional JSON-marshaled body, and decodes a JSON response body into out when
// out is non-nil.
//
// It sets Authorization: Bearer <token> on every request and, when a body is
// present, Content-Type: application/json. Any non-2xx response is returned as
// an *APIError carrying the status code and Quay's raw error body. A 2xx with
// an empty body and a non-nil out leaves out unmodified.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	hasBody := body != nil
	if hasBody {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("quay %s: marshaling request body for %s %s: %w", c.host(), method, path, err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("quay %s: building request %s %s: %w", c.host(), method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("quay %s: %s %s: %w", c.host(), method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, oversized, err := readBounded(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return fmt.Errorf("quay %s: reading response body for %s %s: %w", c.host(), method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(c.host(), method, path, resp.StatusCode, respBody)
	}
	if oversized {
		return fmt.Errorf("quay %s: response body for %s %s exceeded %d bytes", c.host(), method, path, maxResponseBodyBytes)
	}

	if out != nil && len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("quay %s: decoding response body for %s %s: %w", c.host(), method, path, err)
		}
	}
	return nil
}

// newAPIError builds an *APIError from a non-2xx response, extracting Quay's
// human-readable message from the body when it parses as JSON. Quay error
// bodies vary by endpoint; it reports an error_message, message, error, or
// detail field, whichever is present.
func newAPIError(host, method, path string, status int, body []byte) *APIError {
	e := &APIError{
		StatusCode: status,
		Host:       host,
		Method:     method,
		Path:       path,
		Body:       truncateString(string(body), maxAPIErrorBodyBytes),
	}
	var parsed struct {
		ErrorMessage string `json:"error_message"`
		Message      string `json:"message"`
		Error        string `json:"error"`
		Detail       string `json:"detail"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch {
		case parsed.ErrorMessage != "":
			e.Message = truncateString(parsed.ErrorMessage, maxAPIErrorBodyBytes)
		case parsed.Message != "":
			e.Message = truncateString(parsed.Message, maxAPIErrorBodyBytes)
		case parsed.Error != "":
			e.Message = truncateString(parsed.Error, maxAPIErrorBodyBytes)
		case parsed.Detail != "":
			e.Message = truncateString(parsed.Detail, maxAPIErrorBodyBytes)
		}
	}
	return e
}

func (c *Client) host() string {
	u, err := neturl.Parse(c.baseURL)
	if err == nil && u.Host != "" {
		return u.Host
	}
	if c.baseURL != "" {
		return c.baseURL
	}
	return "<empty-host>"
}

func readBounded(r io.Reader, limit int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

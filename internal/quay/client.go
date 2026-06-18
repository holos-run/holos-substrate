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
package quay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Repository visibility values accepted by the Quay API.
const (
	// VisibilityPublic makes a repository world-readable.
	VisibilityPublic = "public"
	// VisibilityPrivate restricts a repository to authorized users.
	VisibilityPrivate = "private"
)

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
// with the given OAuth-Application Bearer token. If httpClient is nil,
// http.DefaultClient is used. A trailing slash on baseURL is trimmed so request
// paths join cleanly.
func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: httpClient,
	}
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
			return fmt.Errorf("quay: marshaling request body for %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("quay: building request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("quay: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("quay: reading response body for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(method, path, resp.StatusCode, respBody)
	}

	if out != nil && len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("quay: decoding response body for %s %s: %w", method, path, err)
		}
	}
	return nil
}

// newAPIError builds an *APIError from a non-2xx response, extracting Quay's
// human-readable message from the body when it parses as JSON. Quay error
// bodies vary by endpoint; it reports an error_message, message, error, or
// detail field, whichever is present.
func newAPIError(method, path string, status int, body []byte) *APIError {
	e := &APIError{
		StatusCode: status,
		Method:     method,
		Path:       path,
		Body:       string(body),
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
			e.Message = parsed.ErrorMessage
		case parsed.Message != "":
			e.Message = parsed.Message
		case parsed.Error != "":
			e.Message = parsed.Error
		case parsed.Detail != "":
			e.Message = parsed.Detail
		}
	}
	return e
}

package keycloak

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// APIError is returned for any non-2xx response from the Keycloak Admin REST
// API. It carries the HTTP status code and the raw error body Keycloak returned
// so callers can both branch on the status (create-vs-update, idempotent
// re-create) and surface Keycloak's own diagnostic message.
//
// Reconcilers branch on IsNotFound (404) to decide create-vs-update and on
// IsConflict (409) to treat an already-exists response as success, which keeps
// create operations idempotent across re-runs.
type APIError struct {
	// StatusCode is the HTTP status code of the response.
	StatusCode int
	// Method is the HTTP method of the request that failed.
	Method string
	// Path is the request path that failed.
	Path string
	// Message is the human-readable error message parsed from Keycloak's error
	// body, when one is present.
	Message string
	// Body is the raw, untruncated response body Keycloak returned.
	Body string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	return fmt.Sprintf("keycloak: %s %s: unexpected status %d: %s", e.Method, e.Path, e.StatusCode, msg)
}

// IsNotFound reports whether the API responded 404 Not Found. Reconcilers use
// this to branch between create and update.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// IsConflict reports whether the API responded 409 Conflict. Keycloak returns
// 409 from the create endpoints (groups, users, client roles) when the resource
// already exists, so create operations can be treated as idempotent.
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}

// IsNotFound reports whether err is an *APIError describing a 404 response. It
// unwraps the error chain, so it works on wrapped errors.
func IsNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.IsNotFound()
	}
	return false
}

// IsConflict reports whether err is an *APIError describing an already-exists
// response (409 Conflict). It unwraps the error chain.
func IsConflict(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.IsConflict()
	}
	return false
}

// newAPIError builds an *APIError from a non-2xx response, extracting Keycloak's
// human-readable message from the body when it parses as JSON. Keycloak error
// bodies vary by endpoint; it reports an errorMessage, error_description, error,
// or message field, whichever is present (errorMessage is the Admin REST API's
// usual shape; error/error_description are the OAuth2 token endpoint's).
func newAPIError(method, path string, status int, body []byte) *APIError {
	e := &APIError{
		StatusCode: status,
		Method:     method,
		Path:       path,
		Body:       string(body),
	}
	var parsed struct {
		ErrorMessage     string `json:"errorMessage"`
		ErrorDescription string `json:"error_description"`
		Error            string `json:"error"`
		Message          string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch {
		case parsed.ErrorMessage != "":
			e.Message = parsed.ErrorMessage
		case parsed.ErrorDescription != "":
			e.Message = parsed.ErrorDescription
		case parsed.Error != "":
			e.Message = parsed.Error
		case parsed.Message != "":
			e.Message = parsed.Message
		}
	}
	return e
}

package quay

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError is returned for any non-2xx response from the Quay REST API. It
// carries the HTTP status code and the raw error body Quay returned so callers
// can both branch on the status (create-vs-update, idempotent re-create) and
// surface Quay's own diagnostic message.
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
	// Message is the human-readable error message parsed from Quay's error
	// body, when one is present.
	Message string
	// Body is the raw, untruncated response body Quay returned.
	Body string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	return fmt.Sprintf("quay: %s %s: unexpected status %d: %s", e.Method, e.Path, e.StatusCode, msg)
}

// IsNotFound reports whether the API responded 404 Not Found. Reconcilers use
// this to branch between create and update.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// IsConflict reports whether the API responded with an already-exists status.
// Quay returns 409 Conflict (and, for some endpoints, 400 with a duplicate
// message) when the resource already exists; both map to a conflict so create
// operations can be treated as idempotent.
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}

// IsNotFound reports whether err is an *APIError describing a 404 response.
// It unwraps the error chain, so it works on wrapped errors.
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

// mapDuplicateToConflict normalizes Quay's already-exists signaling. Some
// endpoints (notably organization creation) report a duplicate not as 409 but
// as 400 with a message naming the conflict; this rewrites such an *APIError to
// StatusConflict so IsConflict and the idempotent create wrappers detect it
// uniformly. Any other error (including a genuine 409) is returned unchanged.
func mapDuplicateToConflict(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	if ae.StatusCode == http.StatusBadRequest && isDuplicateMessage(ae.Message, ae.Body) {
		ae.StatusCode = http.StatusConflict
	}
	return err
}

// isAbsentNotification reports whether err is an *APIError describing a Quay
// delete-notification response for a UUID that is already gone. Quay does not
// uniformly return 404 here: an unknown notification UUID can come back as a
// 400 InvalidRequest. This recognizes that 400 form so DeleteNotificationIfExists
// stays idempotent. A genuine 404 is handled separately by IsNotFound.
func isAbsentNotification(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode != http.StatusBadRequest {
		return false
	}
	hay := strings.ToLower(ae.Message + " " + ae.Body)
	return strings.Contains(hay, "invalidrequest") ||
		strings.Contains(hay, "invalid request") ||
		strings.Contains(hay, "no notification") ||
		strings.Contains(hay, "notification not found") ||
		strings.Contains(hay, "could not find notification")
}

// isAbsentTeam reports whether err is an *APIError describing a Quay
// delete-team response for a team that is already gone. Quay's remove_team
// raises InvalidTeamException for a missing team, a DataModelException that
// Quay 3.17.3 does not uniformly surface as 404 — it commonly arrives as a 400.
// This recognizes that absent-team 400 (by its message) so DeleteTeamIfExists
// stays idempotent; a genuine 404 is handled separately by IsNotFound.
func isAbsentTeam(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode != http.StatusBadRequest {
		return false
	}
	hay := strings.ToLower(ae.Message + " " + ae.Body)
	return strings.Contains(hay, "not a team") ||
		strings.Contains(hay, "invalid team") ||
		strings.Contains(hay, "team not found") ||
		strings.Contains(hay, "could not find team") ||
		strings.Contains(hay, "unknown team")
}

// isDuplicateMessage reports whether a Quay error message or body unambiguously
// indicates an already-exists conflict. Quay phrases these inconsistently across
// endpoints: organization and repository creation both say "already
// exists"/"already taken"/"already in use".
//
// It deliberately does NOT match Quay's repository-create "Could not create
// repository" 400, which is a *generic* create failure (the repo may well be
// missing), not a reliable duplicate signal. CreateRepositoryIfNotExists proves
// existence for that ambiguous case with a GET fallback instead of swallowing it
// here — otherwise a real failure would be silently reported as success and
// reconciliation would stop retrying while the repo is still absent.
func isDuplicateMessage(message, body string) bool {
	hay := strings.ToLower(message + " " + body)
	return strings.Contains(hay, "already exists") ||
		strings.Contains(hay, "already taken") ||
		strings.Contains(hay, "already in use") ||
		strings.Contains(hay, "already a member")
}

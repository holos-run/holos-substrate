package quay

import (
	"context"
	"net/http"
	"net/url"
)

// Quay notification event and method values used by this controller. The
// reconcilers wire exactly one shape: a repo_push event delivered by webhook.
const (
	// EventRepoPush is the Quay notification event fired on a repository push.
	EventRepoPush = "repo_push"
	// MethodWebhook delivers a notification by POSTing to a webhook URL.
	MethodWebhook = "webhook"
)

// Notification is a Quay repository notification (webhook). Only the fields the
// reconciler reads are decoded.
type Notification struct {
	// UUID is Quay's identifier for the notification, needed to delete it.
	UUID string `json:"uuid"`
	// Event is the triggering event, e.g. repo_push.
	Event string `json:"event"`
	// Method is the delivery method, e.g. webhook.
	Method string `json:"method"`
	// Title is the human-readable notification title.
	Title string `json:"title,omitempty"`
	// Config holds method-specific configuration; for a webhook it carries the
	// target url.
	Config NotificationConfig `json:"config"`
}

// NotificationConfig is the webhook delivery configuration: the target URL the
// repo_push event is POSTed to.
type NotificationConfig struct {
	// URL is the webhook target the notification is delivered to.
	URL string `json:"url"`
}

// createNotificationRequest is the POST .../notification/ body. eventConfig is
// required by Quay (empty for repo_push) and title labels the notification.
type createNotificationRequest struct {
	Event       string             `json:"event"`
	Method      string             `json:"method"`
	Config      NotificationConfig `json:"config"`
	EventConfig map[string]any     `json:"eventConfig"`
	Title       string             `json:"title,omitempty"`
}

// listNotificationsResponse is the GET .../notification/ envelope.
type listNotificationsResponse struct {
	Notifications []Notification `json:"notifications"`
}

// CreateNotification creates a repo_push webhook notification on repository
// ns/repo delivering to url, labeled title, via
// POST /api/v1/repository/{ns}/{repo}/notification/. It returns the created
// Notification (including the UUID Quay assigns).
func (c *Client) CreateNotification(ctx context.Context, ns, repo, url, title string) (*Notification, error) {
	req := createNotificationRequest{
		Event:       EventRepoPush,
		Method:      MethodWebhook,
		Config:      NotificationConfig{URL: url},
		EventConfig: map[string]any{},
		Title:       title,
	}
	out := &Notification{}
	if err := c.doJSON(ctx, http.MethodPost, notificationsPath(ns, repo), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListNotifications returns all notifications configured on repository ns/repo
// via GET /api/v1/repository/{ns}/{repo}/notification/. Reconcilers use it to
// find an existing repo_push webhook (matching on URL) before creating one, so
// re-runs do not pile up duplicates.
func (c *Client) ListNotifications(ctx context.Context, ns, repo string) ([]Notification, error) {
	out := &listNotificationsResponse{}
	if err := c.doJSON(ctx, http.MethodGet, notificationsPath(ns, repo), nil, out); err != nil {
		return nil, err
	}
	return out.Notifications, nil
}

// DeleteNotification deletes the notification identified by uuid on repository
// ns/repo via DELETE /api/v1/repository/{ns}/{repo}/notification/{uuid}. A
// missing notification is returned as an *APIError reporting IsNotFound; use
// DeleteNotificationIfExists to treat that as success.
func (c *Client) DeleteNotification(ctx context.Context, ns, repo, uuid string) error {
	path := notificationsPath(ns, repo) + url.PathEscape(uuid)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteNotificationIfExists deletes the notification and returns nil when it is
// already absent, so the call is idempotent.
func (c *Client) DeleteNotificationIfExists(ctx context.Context, ns, repo, uuid string) error {
	err := c.DeleteNotification(ctx, ns, repo, uuid)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// notificationsPath builds the trailing-slash /api/v1/repository/{ns}/{repo}/notification/
// collection path; callers append a UUID for item operations.
func notificationsPath(ns, repo string) string {
	return repositoryPath(ns, repo) + "/notification/"
}

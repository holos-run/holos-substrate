package quay

import (
	"context"
	"net/http"
	"sync"

	"github.com/holos-run/holos-paas/internal/quay"
)

// fakeRepoStore is the in-memory state a fakeRepoClient maintains for one
// repository: its visibility/description and its notifications. The reconciler
// reads these back to detect drift and reconcile the webhook.
type fakeRepoStore struct {
	isPublic    bool
	description string
	// notifications maps UUID → notification, so a delete is keyed and a list is
	// stable enough for the reconcile assertions.
	notifications map[string]quay.Notification
}

// fakeRepoClient is a recording, in-memory stand-in for the Quay repository and
// notification API the Repository reconciler drives. It satisfies RepoClient so a
// test injects it via the reconciler's RepoClientFactory, exercising the full
// reconcile loop without HTTP or a live Quay. The owning organization is resolved
// from the Organization CR by the reconciler, not from this fake, so the fake
// models only repositories and notifications.
type fakeRepoClient struct {
	mu sync.Mutex

	// repos maps "ns/repo" → its in-memory state. A repository absent from the
	// map 404s on GetRepository.
	repos map[string]*fakeRepoStore

	// createRepoErr, when non-nil, is returned by CreateRepository.
	createRepoErr error
	// createRepoRace, when true, makes CreateRepository simulate a duplicate
	// response after materializing the repository, exercising
	// CreateRepositoryIfNotExists's GET-confirmed success path.
	createRepoRace bool
	// updateVisibilityErr/updateDescriptionErr, when non-nil, are returned by
	// the corresponding repository update operation.
	updateVisibilityErr  error
	updateDescriptionErr error
	// createNotifErr, when non-nil, is returned by CreateNotification — used to
	// simulate a Quay error whose body echoes the submitted (secret) webhook URL.
	createNotifErr error

	// nextUUID counts created notifications so each gets a unique UUID.
	nextUUID int
	// calls records every method call, in order, e.g. "GetRepository:acme/web".
	calls []string

	// gotCABundle records the caBundle the reconciler's RepoClientFactory was
	// last invoked with, so a test asserts the spec's CABundle is threaded
	// through to the client factory.
	gotCABundle []byte
}

// newFakeRepoClient returns a fake with no repositories.
func newFakeRepoClient() *fakeRepoClient {
	return &fakeRepoClient{repos: map[string]*fakeRepoStore{}}
}

func (f *fakeRepoClient) record(call string) { f.calls = append(f.calls, call) }

func repoKey(ns, repo string) string { return ns + "/" + repo }

func notFoundRepoError(ns, repo string) error {
	return &quay.APIError{
		StatusCode: http.StatusNotFound,
		Method:     http.MethodGet,
		Path:       "/api/v1/repository/" + ns + "/" + repo,
		Message:    "not found",
	}
}

func conflictRepoError(ns, repo string) error {
	return &quay.APIError{
		StatusCode: http.StatusConflict,
		Method:     http.MethodPost,
		Path:       "/api/v1/repository",
		Message:    "repository " + ns + "/" + repo + " already exists",
	}
}

func (f *fakeRepoClient) GetRepository(ctx context.Context, ns, repo string) (*quay.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetRepository:" + repoKey(ns, repo))
	st, ok := f.repos[repoKey(ns, repo)]
	if !ok {
		return nil, notFoundRepoError(ns, repo)
	}
	return &quay.Repository{
		Namespace:   ns,
		Name:        repo,
		Description: st.description,
		IsPublic:    st.isPublic,
	}, nil
}

func (f *fakeRepoClient) CreateRepository(ctx context.Context, ns, repo, visibility, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateRepository:" + repoKey(ns, repo))
	if f.createRepoErr != nil {
		return f.createRepoErr
	}
	if _, ok := f.repos[repoKey(ns, repo)]; ok {
		return conflictRepoError(ns, repo)
	}
	f.repos[repoKey(ns, repo)] = &fakeRepoStore{
		isPublic:      visibility == "public",
		description:   description,
		notifications: map[string]quay.Notification{},
	}
	if f.createRepoRace {
		return &quay.APIError{StatusCode: http.StatusBadRequest, Method: http.MethodPost, Path: "/api/v1/repository", Message: "Could not create repository"}
	}
	return nil
}

func (f *fakeRepoClient) CreateRepositoryIfNotExists(ctx context.Context, ns, repo, visibility, description string) error {
	err := f.CreateRepository(ctx, ns, repo, visibility, description)
	if err == nil || quay.IsConflict(err) {
		return nil
	}
	if _, getErr := f.GetRepository(ctx, ns, repo); getErr == nil {
		return nil
	}
	return err
}

func (f *fakeRepoClient) UpdateRepositoryVisibility(ctx context.Context, ns, repo, visibility string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateRepositoryVisibility:" + repoKey(ns, repo))
	if f.updateVisibilityErr != nil {
		return f.updateVisibilityErr
	}
	if st, ok := f.repos[repoKey(ns, repo)]; ok {
		st.isPublic = visibility == "public"
	}
	return nil
}

func (f *fakeRepoClient) UpdateRepositoryDescription(ctx context.Context, ns, repo, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateRepositoryDescription:" + repoKey(ns, repo))
	if f.updateDescriptionErr != nil {
		return f.updateDescriptionErr
	}
	if st, ok := f.repos[repoKey(ns, repo)]; ok {
		st.description = description
	}
	return nil
}

func (f *fakeRepoClient) DeleteRepositoryIfExists(ctx context.Context, ns, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteRepository:" + repoKey(ns, repo))
	delete(f.repos, repoKey(ns, repo))
	return nil
}

func (f *fakeRepoClient) ListNotifications(ctx context.Context, ns, repo string) ([]quay.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListNotifications:" + repoKey(ns, repo))
	st, ok := f.repos[repoKey(ns, repo)]
	if !ok {
		return nil, notFoundRepoError(ns, repo)
	}
	out := make([]quay.Notification, 0, len(st.notifications))
	for _, n := range st.notifications {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeRepoClient) CreateNotification(ctx context.Context, ns, repo, url, title string) (*quay.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateNotification:" + repoKey(ns, repo) + ":" + url)
	if f.createNotifErr != nil {
		return nil, f.createNotifErr
	}
	st, ok := f.repos[repoKey(ns, repo)]
	if !ok {
		return nil, notFoundRepoError(ns, repo)
	}
	f.nextUUID++
	n := quay.Notification{
		UUID:   uuidFor(f.nextUUID),
		Event:  quay.EventRepoPush,
		Method: quay.MethodWebhook,
		Title:  title,
		Config: quay.NotificationConfig{URL: url},
	}
	st.notifications[n.UUID] = n
	return &n, nil
}

func (f *fakeRepoClient) DeleteNotificationIfExists(ctx context.Context, ns, repo, uuid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteNotification:" + repoKey(ns, repo) + ":" + uuid)
	if st, ok := f.repos[repoKey(ns, repo)]; ok {
		delete(st.notifications, uuid)
	}
	return nil
}

// uuidFor builds a deterministic fake UUID from a counter.
func uuidFor(n int) string {
	return "uuid-" + string(rune('a'+n-1))
}

// callsContain reports whether the recorded calls include call.
func (f *fakeRepoClient) callsContain(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

// repoExists reports whether ns/repo currently exists in the fake.
func (f *fakeRepoClient) repoExists(ns, repo string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[repoKey(ns, repo)]
	return ok
}

// webhookURLs returns the URLs of the repo_push webhook notifications on ns/repo.
func (f *fakeRepoClient) webhookURLs(ns, repo string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.repos[repoKey(ns, repo)]
	if !ok {
		return nil
	}
	var urls []string
	for _, n := range st.notifications {
		if n.Event == quay.EventRepoPush && n.Method == quay.MethodWebhook {
			urls = append(urls, n.Config.URL)
		}
	}
	return urls
}

// seedNotification injects a pre-existing repo_push webhook notification on
// ns/repo with the given title and url so tests can exercise the
// URL-change-replaces-notification path and the manual-webhook-preservation path
// (title without this resource's UID-bearing marker).
func (f *fakeRepoClient) seedNotification(ns, repo, title, url string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.repos[repoKey(ns, repo)]
	if !ok {
		st = &fakeRepoStore{notifications: map[string]quay.Notification{}}
		f.repos[repoKey(ns, repo)] = st
	}
	f.nextUUID++
	uuid := uuidFor(f.nextUUID)
	st.notifications[uuid] = quay.Notification{
		UUID:   uuid,
		Event:  quay.EventRepoPush,
		Method: quay.MethodWebhook,
		Title:  title,
		Config: quay.NotificationConfig{URL: url},
	}
	return uuid
}

// compile-time assertion that the fake satisfies the reconciler's seam.
var _ RepoClient = (*fakeRepoClient)(nil)

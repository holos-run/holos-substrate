package quay

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"github.com/holos-run/holos-paas/internal/quay"
)

// newRepoReconciler builds a RepositoryReconciler wired to the envtest client and
// a recording event recorder, injecting the supplied fake Quay client.
func newRepoReconciler(fake *fakeRepoClient, namespace string) (*RepositoryReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	r := &RepositoryReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Namespace: namespace,
		NewClient: func(cred *quayCredential) RepoClient { return fake },
	}
	return r, recorder
}

// reconcileRepo runs a single Reconcile pass for the named Repository.
func reconcileRepo(ctx context.Context, r *RepositoryReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// reconcileRepoUntilStable drives Reconcile repeatedly (the first pass adds the
// finalizer and requeues) until it returns without requeueing or errors, bounded
// to a few iterations.
func reconcileRepoUntilStable(ctx context.Context, t *testing.T, r *RepositoryReconciler, key client.ObjectKey) error {
	t.Helper()
	var lastErr error
	for i := 0; i < 5; i++ {
		res, err := reconcileRepo(ctx, r, key)
		lastErr = err
		if err != nil {
			return err
		}
		if !res.Requeue {
			return nil
		}
	}
	return lastErr
}

// repoOpts configures makeRepo.
type repoOpts struct {
	visibility  quayv1alpha1.RepositoryVisibility
	description string
	webhook     *quayv1alpha1.RepositoryWebhook
}

// makeRepo creates a Repository CR named repoName in namespace ns owned by org
// orgRef, with a credential Secret of the default name, and returns its key.
func makeRepo(ctx context.Context, t *testing.T, ns, orgRef, repoName string, opts repoOpts) client.ObjectKey {
	t.Helper()
	vis := opts.visibility
	if vis == "" {
		vis = quayv1alpha1.RepositoryVisibilityPrivate
	}
	repo := &quayv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: repoName},
		Spec: quayv1alpha1.RepositorySpec{
			OrganizationRef: orgRef,
			Name:            repoName,
			Visibility:      vis,
			Description:     opts.description,
			Webhook:         opts.webhook,
		},
	}
	if err := shared.k8sClient.Create(ctx, repo); err != nil {
		t.Fatalf("creating Repository: %v", err)
	}
	return client.ObjectKeyFromObject(repo)
}

// getRepo fetches the current Repository state.
func getRepo(ctx context.Context, t *testing.T, key client.ObjectKey) *quayv1alpha1.Repository {
	t.Helper()
	repo := &quayv1alpha1.Repository{}
	if err := shared.k8sClient.Get(ctx, key, repo); err != nil {
		t.Fatalf("getting Repository: %v", err)
	}
	return repo
}

// repoConditionStatus returns the status of the named condition on the Repository.
func repoConditionStatus(repo *quayv1alpha1.Repository, condType string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(repo.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Status
}

// repoConditionReason returns the reason of the named condition on the Repository.
func repoConditionReason(repo *quayv1alpha1.Repository, condType string) string {
	c := meta.FindStatusCondition(repo.Status.Conditions, condType)
	if c == nil {
		return ""
	}
	return c.Reason
}

func ptr(s string) *string { return &s }

func TestRepositoryCreatesWithInlineWebhook(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	const hook = "https://kargo.example.test/webhook/abc"
	key := makeRepo(ctx, t, ns, "acme", "web", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{Url: ptr(hook)},
	})

	fake := newFakeRepoClient("acme") // org exists
	r, recorder := newRepoReconciler(fake, ns)

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !fake.repoExists("acme", "web") {
		t.Error("expected acme/web to exist in Quay")
	}
	if urls := fake.webhookURLs("acme", "web"); len(urls) != 1 || urls[0] != hook {
		t.Errorf("webhook URLs = %v, want exactly [%s]", urls, hook)
	}

	repo := getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := repoConditionStatus(repo, ConditionWebhookConfigured); got != metav1.ConditionTrue {
		t.Errorf("WebhookConfigured = %q, want True", got)
	}
	if got := repoConditionReason(repo, ConditionWebhookConfigured); got != ReasonWebhookConfigured {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, ReasonWebhookConfigured)
	}
	if repo.Status.ObservedGeneration != repo.Generation {
		t.Errorf("observedGeneration = %d, want %d", repo.Status.ObservedGeneration, repo.Generation)
	}
	assertEvent(t, recorder, ReasonReconciled)
}

func TestRepositoryCreatesWithSecretRefWebhook(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	const hook = "https://kargo.example.test/webhook/secret"
	hookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kargo-receiver"},
		Data:       map[string][]byte{"url": []byte(hook)},
	}
	if err := shared.k8sClient.Create(ctx, hookSecret); err != nil {
		t.Fatalf("creating webhook secret: %v", err)
	}
	key := makeRepo(ctx, t, ns, "acme", "api", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{
			UrlSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
		},
	})

	fake := newFakeRepoClient("acme")
	r, _ := newRepoReconciler(fake, ns)

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if urls := fake.webhookURLs("acme", "api"); len(urls) != 1 || urls[0] != hook {
		t.Errorf("webhook URLs = %v, want exactly [%s]", urls, hook)
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, ConditionWebhookConfigured); got != metav1.ConditionTrue {
		t.Errorf("WebhookConfigured = %q, want True", got)
	}
}

func TestRepositorySecretRefMissingKeySetsConditionAndRequeues(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// The webhook Secret exists but lacks the referenced key.
	hookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kargo-receiver"},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	if err := shared.k8sClient.Create(ctx, hookSecret); err != nil {
		t.Fatalf("creating webhook secret: %v", err)
	}
	key := makeRepo(ctx, t, ns, "acme", "missingkey", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{
			UrlSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
		},
	})

	fake := newFakeRepoClient("acme")
	r, recorder := newRepoReconciler(fake, ns)

	// First pass adds the finalizer; the next hits the missing key.
	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	_, err := reconcileRepo(ctx, r, key)
	if err == nil {
		t.Fatal("expected reconcile to return an error so it requeues for the missing key")
	}

	// The repo was created (org + GetRepository + CreateRepository ran) but the
	// webhook was not configured; no CreateNotification call happened.
	if fake.callsContain("CreateNotification:acme/missingkey:") {
		t.Errorf("expected no webhook creation when the key is missing; calls were %v", fake.calls)
	}
	if len(fake.webhookURLs("acme", "missingkey")) != 0 {
		t.Errorf("expected no webhook configured, got %v", fake.webhookURLs("acme", "missingkey"))
	}

	repo := getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, ConditionWebhookConfigured); got != metav1.ConditionFalse {
		t.Errorf("WebhookConfigured = %q, want False", got)
	}
	if got := repoConditionReason(repo, ConditionWebhookConfigured); got != ReasonWebhookURLNotFound {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, ReasonWebhookURLNotFound)
	}
	assertEvent(t, recorder, ReasonWebhookURLNotFound)
}

func TestRepositoryCorrectsVisibilityAndDescriptionDrift(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeRepo(ctx, t, ns, "acme", "drift", repoOpts{
		visibility:  quayv1alpha1.RepositoryVisibilityPublic,
		description: "the desired description",
	})

	fake := newFakeRepoClient("acme")
	// Pre-create the repo with the WRONG visibility and description, so reconcile
	// must correct both.
	fake.repos[repoKey("acme", "drift")] = &fakeRepoStore{
		isPublic:      false,
		description:   "stale description",
		notifications: map[string]quay.Notification{},
	}

	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !fake.callsContain("UpdateRepositoryVisibility:acme/drift") {
		t.Errorf("expected a visibility update, calls were %v", fake.calls)
	}
	if !fake.callsContain("UpdateRepositoryDescription:acme/drift") {
		t.Errorf("expected a description update, calls were %v", fake.calls)
	}
	st := fake.repos[repoKey("acme", "drift")]
	if !st.isPublic {
		t.Error("expected visibility corrected to public")
	}
	if st.description != "the desired description" {
		t.Errorf("description = %q, want corrected", st.description)
	}
}

func TestRepositoryWebhookURLChangeReplacesNotification(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	const newHook = "https://kargo.example.test/webhook/new"
	key := makeRepo(ctx, t, ns, "acme", "rehook", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{Url: ptr(newHook)},
	})

	fake := newFakeRepoClient("acme")
	// Pre-create the repo with a stale repo_push webhook pointing elsewhere.
	fake.repos[repoKey("acme", "rehook")] = &fakeRepoStore{
		notifications: map[string]quay.Notification{},
	}
	fake.seedNotification("acme", "rehook", "https://old.example.test/stale")

	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	urls := fake.webhookURLs("acme", "rehook")
	if len(urls) != 1 || urls[0] != newHook {
		t.Errorf("webhook URLs = %v, want exactly [%s] after replacement", urls, newHook)
	}
}

func TestRepositoryOrganizationNotReadyRequeues(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeRepo(ctx, t, ns, "missing-org", "web", repoOpts{})

	fake := newFakeRepoClient() // no orgs exist
	r, recorder := newRepoReconciler(fake, ns)

	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	_, err := reconcileRepo(ctx, r, key)
	if err == nil {
		t.Fatal("expected reconcile to requeue while the org is not ready")
	}

	// No repository was created (AC #9: the reconciler never creates the org and
	// must not create a repo before the org exists).
	if fake.callsContain("CreateRepository:missing-org/web") {
		t.Errorf("must not create a repo before the org exists; calls were %v", fake.calls)
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionReason(repo, ConditionReady); got != ReasonOrganizationNotReady {
		t.Errorf("Ready reason = %q, want %q", got, ReasonOrganizationNotReady)
	}
	assertEvent(t, recorder, ReasonOrganizationNotReady)
}

func TestRepositoryMissingCredentialSetsConditionAndNoQuayCall(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	// Deliberately do NOT create the credential Secret.
	key := makeRepo(ctx, t, ns, "acme", "nocreds", repoOpts{})

	fake := newFakeRepoClient("acme")
	r, recorder := newRepoReconciler(fake, ns)

	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	_, err := reconcileRepo(ctx, r, key)
	if err == nil {
		t.Fatal("expected reconcile to requeue for the missing credential Secret")
	}

	if len(fake.calls) != 0 {
		t.Errorf("expected no Quay calls when the credential Secret is missing, got %v", fake.calls)
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionReason(repo, ConditionReady); got != ReasonCredentialsNotFound {
		t.Errorf("Ready reason = %q, want %q", got, ReasonCredentialsNotFound)
	}
	assertEvent(t, recorder, ReasonCredentialsNotFound)
}

func TestRepositoryNoWebhookIsWebhookless(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeRepo(ctx, t, ns, "acme", "nohook", repoOpts{})

	fake := newFakeRepoClient("acme")
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo := getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := repoConditionStatus(repo, ConditionWebhookConfigured); got != metav1.ConditionFalse {
		t.Errorf("WebhookConfigured = %q, want False (webhookless)", got)
	}
	if got := repoConditionReason(repo, ConditionWebhookConfigured); got != ReasonWebhookNotConfigured {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, ReasonWebhookNotConfigured)
	}
}

func TestRepositoryDeleteRemovesFinalizerAfterQuayDelete(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	key := makeRepo(ctx, t, ns, "acme", "doomed", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{Url: ptr("https://kargo.example.test/webhook/x")},
	})

	fake := newFakeRepoClient("acme")
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if !fake.repoExists("acme", "doomed") {
		t.Fatal("expected acme/doomed to exist before delete")
	}

	repo := getRepo(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if !fake.callsContain("DeleteRepository:acme/doomed") {
		t.Errorf("expected a Delete call for acme/doomed, calls were %v", fake.calls)
	}
	if fake.repoExists("acme", "doomed") {
		t.Error("expected acme/doomed removed from Quay after delete")
	}
	if err := shared.k8sClient.Get(ctx, key, &quayv1alpha1.Repository{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Repository deleted, get returned %v", err)
	}
}

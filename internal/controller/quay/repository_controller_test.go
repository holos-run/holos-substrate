package quay

import (
	"context"
	"strings"
	"testing"
	"time"

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
		NewClient: func(cred *quayCredential, caBundle []byte) RepoClient {
			fake.gotCABundle = caBundle
			return fake
		},
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
		// The first pass requeues immediately (RequeueAfter == requeueImmediately)
		// after adding the finalizer; keep looping only for that. A steady-state
		// success with a urlSecretRef webhook legitimately sets a long resync
		// RequeueAfter (webhookSecretResyncInterval) and must count as stable, so
		// stop on anything other than the immediate-requeue sentinel.
		if res.RequeueAfter != requeueImmediately {
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
	caBundle    []byte
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
			CABundle:        opts.caBundle,
		},
	}
	if err := shared.k8sClient.Create(ctx, repo); err != nil {
		t.Fatalf("creating Repository: %v", err)
	}
	return client.ObjectKeyFromObject(repo)
}

// makeReadyOrg creates an Organization CR named orgName (its spec.name, the Quay
// org, is the same string for test simplicity) in namespace ns and marks it
// Ready=True, so the Repository reconciler resolves it and proceeds. It returns
// the Quay org name (== orgName here).
func makeReadyOrg(ctx context.Context, t *testing.T, ns, orgName string) string {
	t.Helper()
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: orgName},
		Spec: quayv1alpha1.OrganizationSpec{
			Name:  orgName,
			Email: orgName + "@example.test",
		},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	meta.SetStatusCondition(&org.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonCreated,
		Message:            "ready for test",
		ObservedGeneration: org.Generation,
	})
	org.Status.Created = true
	if err := shared.k8sClient.Status().Update(ctx, org); err != nil {
		t.Fatalf("setting Organization Ready: %v", err)
	}
	return orgName
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

// containsString reports whether xs contains want.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestRepositoryCreatesWithInlineWebhook(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const hook = "https://kargo.example.test/webhook/abc"
	key := makeRepo(ctx, t, ns, "acme", "web", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{Url: ptr(hook)},
	})

	fake := newFakeRepoClient()
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
	if repo.Status.LastValidatedTime == nil {
		t.Errorf("lastValidatedTime not set on successful reconcile")
	}
	if repo.Status.LastMutatedTime == nil || repo.Status.LastMutationReason != quayv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want time with %q", repo.Status.LastMutatedTime, repo.Status.LastMutationReason, quayv1alpha1.MutationReasonSpecChange)
	}
	firstValidated := repo.Status.LastValidatedTime.DeepCopy()
	firstMutated := repo.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	result, err := reconcileRepo(ctx, r, key)
	if err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if result.RequeueAfter != quayExternalResourceResync {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, quayExternalResourceResync)
	}
	repo = getRepo(ctx, t, key)
	if !repo.Status.LastValidatedTime.After(firstValidated.Time) {
		t.Errorf("lastValidatedTime did not advance: first=%v second=%v", firstValidated, repo.Status.LastValidatedTime)
	}
	if !repo.Status.LastMutatedTime.Equal(firstMutated) {
		t.Errorf("lastMutatedTime changed on steady validation: first=%v second=%v", firstMutated, repo.Status.LastMutatedTime)
	}
	assertEvent(t, recorder, ReasonReconciled)
}

func TestRepositoryThreadsCABundleToClientFactory(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	caBundle := validTestCABundle(t)
	key := makeRepo(ctx, t, ns, "acme", "web", repoOpts{caBundle: caBundle})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if string(fake.gotCABundle) != string(caBundle) {
		t.Errorf("RepoClientFactory received caBundle %q, want the spec's %q", fake.gotCABundle, caBundle)
	}
}

func TestRepositoryInvalidCABundleFailsWithoutQuayCall(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "web", repoOpts{caBundle: []byte("not a pem block")})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)

	if err := reconcileRepoUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail for an invalid caBundle")
	}

	if len(fake.calls) != 0 {
		t.Errorf("expected no Quay calls for an invalid caBundle, calls were %v", fake.calls)
	}
	got := getRepo(ctx, t, key)
	if s := repoConditionStatus(got, ConditionReady); s != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False for an invalid caBundle", s)
	}
	if reason := repoConditionReason(got, ConditionReady); reason != ReasonQuayError {
		t.Errorf("Ready reason = %q, want %q", reason, ReasonQuayError)
	}
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
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "api", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{
			UrlSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
		},
	})

	fake := newFakeRepoClient()
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
	// The secret-backed webhook URL is sensitive; it must NOT appear in status.
	if c := meta.FindStatusCondition(repo.Status.Conditions, ConditionWebhookConfigured); c != nil && strings.Contains(c.Message, hook) {
		t.Errorf("WebhookConfigured message leaked the secret URL: %q", c.Message)
	}

	// A urlSecretRef-backed repo requeues on an interval (no Secret watch) so a
	// later Secret value change is eventually re-pushed.
	res, err := reconcileRepo(ctx, r, key)
	if err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	if res.RequeueAfter != webhookSecretResyncInterval {
		t.Errorf("RequeueAfter = %v, want %v for a secretRef-backed webhook", res.RequeueAfter, webhookSecretResyncInterval)
	}
}

func TestRepositorySecretRefWebhookErrorDoesNotLeakURL(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	const secretURL = "https://kargo.example.test/webhook/super-secret-token"
	hookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kargo-receiver"},
		Data:       map[string][]byte{"url": []byte(secretURL)},
	}
	if err := shared.k8sClient.Create(ctx, hookSecret); err != nil {
		t.Fatalf("creating webhook secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "leaky", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{
			UrlSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
		},
	})

	fake := newFakeRepoClient()
	// Simulate a Quay error whose body echoes the submitted (secret) webhook URL.
	fake.createNotifErr = &quay.APIError{
		StatusCode: 400,
		Method:     "POST",
		Path:       "/api/v1/repository/acme/leaky/notification/",
		Message:    "invalid webhook config for url " + secretURL,
	}
	r, _ := newRepoReconciler(fake, ns)

	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	if _, err := reconcileRepo(ctx, r, key); err == nil {
		t.Fatal("expected reconcile to error so it requeues on the webhook failure")
	}

	repo := getRepo(ctx, t, key)
	for _, c := range repo.Status.Conditions {
		if strings.Contains(c.Message, secretURL) {
			t.Errorf("condition %q message leaked the secret URL: %q", c.Type, c.Message)
		}
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
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "missingkey", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{
			UrlSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
		},
	})

	fake := newFakeRepoClient()
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
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "drift", repoOpts{
		visibility:  quayv1alpha1.RepositoryVisibilityPublic,
		description: "the desired description",
	})

	fake := newFakeRepoClient()
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
	makeReadyOrg(ctx, t, ns, "acme")
	const newHook = "https://kargo.example.test/webhook/new"
	key := makeRepo(ctx, t, ns, "acme", "rehook", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{Url: ptr(newHook)},
	})

	fake := newFakeRepoClient()
	// Pre-create the repo with a stale controller-owned repo_push webhook pointing
	// elsewhere (same webhookTitle, so the reconciler owns and replaces it) plus a
	// manually-created webhook with a different title that must be preserved.
	fake.repos[repoKey("acme", "rehook")] = &fakeRepoStore{
		notifications: map[string]quay.Notification{},
	}
	fake.seedNotification("acme", "rehook", webhookTitle, "https://old.example.test/stale")
	fake.seedNotification("acme", "rehook", "manual webhook", "https://manual.example.test/keep")

	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	urls := fake.webhookURLs("acme", "rehook")
	// The stale controller-owned webhook is replaced by newHook; the manual one
	// survives.
	if !containsString(urls, newHook) {
		t.Errorf("webhook URLs = %v, want to contain the new controller URL %s", urls, newHook)
	}
	if containsString(urls, "https://old.example.test/stale") {
		t.Errorf("stale controller webhook should have been replaced; URLs = %v", urls)
	}
	if !containsString(urls, "https://manual.example.test/keep") {
		t.Errorf("manually-created webhook must be preserved; URLs = %v", urls)
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
	// Deliberately do NOT create the credential Secret. The credential is
	// resolved before the org, so no Organization CR is needed here.
	key := makeRepo(ctx, t, ns, "acme", "nocreds", repoOpts{})

	fake := newFakeRepoClient()
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
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "nohook", repoOpts{})

	fake := newFakeRepoClient()
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
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "doomed", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{Url: ptr("https://kargo.example.test/webhook/x")},
	})

	fake := newFakeRepoClient()
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

func TestRepositoryResolvesQuayOrgThroughOrganizationSpecName(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// The Organization CR is named "team-acme" but its Quay org (spec.name) is
	// "acme-quay-org". The Repository references the CR name; the reconciler must
	// write into the spec.name org, not the ref string.
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "team-acme"},
		Spec:       quayv1alpha1.OrganizationSpec{Name: "acme-quay-org", Email: "a@example.test"},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	meta.SetStatusCondition(&org.Status.Conditions, metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonCreated, Message: "ready",
	})
	if err := shared.k8sClient.Status().Update(ctx, org); err != nil {
		t.Fatalf("setting Organization Ready: %v", err)
	}

	key := makeRepo(ctx, t, ns, "team-acme", "web", repoOpts{})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !fake.repoExists("acme-quay-org", "web") {
		t.Errorf("expected repo created in the Organization spec.name org acme-quay-org; calls were %v", fake.calls)
	}
	if fake.repoExists("team-acme", "web") {
		t.Error("must not create the repo in an org named by the ref string")
	}
	repo := getRepo(ctx, t, key)
	if got := repo.Status.QuayRepository; got != "acme-quay-org/web" {
		t.Errorf("status.QuayRepository = %q, want acme-quay-org/web", got)
	}
}

func TestRepositoryOrganizationExistsButNotReadyRequeues(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	// The Organization CR exists but has no Ready=True condition.
	org := &quayv1alpha1.Organization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pending-org"},
		Spec:       quayv1alpha1.OrganizationSpec{Name: "pending-org", Email: "p@example.test"},
	}
	if err := shared.k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("creating Organization: %v", err)
	}
	key := makeRepo(ctx, t, ns, "pending-org", "web", repoOpts{})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)

	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	_, err := reconcileRepo(ctx, r, key)
	if err == nil {
		t.Fatal("expected reconcile to requeue while the org is not Ready")
	}

	if len(fake.calls) != 0 {
		t.Errorf("expected no Quay calls while the org is not Ready, got %v", fake.calls)
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionReason(repo, ConditionReady); got != ReasonOrganizationNotReady {
		t.Errorf("Ready reason = %q, want %q", got, ReasonOrganizationNotReady)
	}
}

func TestRepositoryDeleteFallsBackToSpecWhenStatusEmpty(t *testing.T) {
	ctx := context.Background()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "orphan", repoOpts{})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if !fake.repoExists("acme", "orphan") {
		t.Fatal("expected acme/orphan to exist before delete")
	}

	// Simulate a crash that created the Quay repo but never persisted
	// status.QuayRepository: clear it so the finalizer must fall back to the spec +
	// Organization CR to find the repo to delete.
	repo := getRepo(ctx, t, key)
	repo.Status.QuayRepository = ""
	if err := shared.k8sClient.Status().Update(ctx, repo); err != nil {
		t.Fatalf("clearing status.QuayRepository: %v", err)
	}

	repo = getRepo(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	// Despite the empty status, the fallback resolved acme/orphan and deleted it.
	if !fake.callsContain("DeleteRepository:acme/orphan") {
		t.Errorf("expected fallback delete of acme/orphan, calls were %v", fake.calls)
	}
	if fake.repoExists("acme", "orphan") {
		t.Error("expected acme/orphan removed from Quay via the spec fallback")
	}
}

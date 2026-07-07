package quay

import (
	"context"
	"errors"
	"net/http"
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
	visibility     quayv1alpha1.RepositoryVisibility
	description    string
	adopt          bool
	deletionPolicy quayv1alpha1.DeletionPolicy
	webhook        *quayv1alpha1.RepositoryWebhook
	caBundle       []byte
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
			Adopt:           opts.adopt,
			DeletionPolicy:  opts.deletionPolicy,
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
	if err := shared.k8sClient.Get(ctx, client.ObjectKeyFromObject(org), org); err != nil {
		t.Fatalf("getting created Organization: %v", err)
	}
	meta.SetStatusCondition(&org.Status.Conditions, metav1.Condition{
		Type:               quayv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             quayv1alpha1.ReasonCreated,
		Message:            "ready for test",
		ObservedGeneration: org.Generation,
	})
	org.Status.ObservedGeneration = org.Generation
	setStatusCreated(org, true)
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

type webhookSecretFailReader struct {
	client.Reader
	namespace string
	name      string
	err       error
}

func (r webhookSecretFailReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Namespace == r.namespace && key.Name == r.name {
		if _, ok := obj.(*corev1.Secret); ok {
			return r.err
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

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
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const hook = "https://kargo.example.test/webhook/abc"
	key := makeRepo(ctx, t, ns, "acme", "web", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(hook)},
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
	if repo.Status.WebhookNotificationUUID == "" {
		t.Fatal("status.webhookNotificationUUID is empty, want recorded controller webhook UUID")
	}
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionWebhookConfigured); got != metav1.ConditionTrue {
		t.Errorf("WebhookConfigured = %q, want True", got)
	}
	if got := repoConditionReason(repo, quayv1alpha1.ConditionWebhookConfigured); got != quayv1alpha1.ReasonWebhookConfigured {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, quayv1alpha1.ReasonWebhookConfigured)
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
	if repo.Status.Created == nil || !*repo.Status.Created {
		t.Errorf("status.created = %v, want true for a created repository", repo.Status.Created)
	}
	firstValidated := repo.Status.LastValidatedTime.DeepCopy()
	firstMutated := repo.Status.LastMutatedTime.DeepCopy()
	assertEvent(t, recorder, quayv1alpha1.ReasonReconciled)

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
	assertNoEvent(t, recorder, quayv1alpha1.ReasonReconciled)
}

func TestRepositoryThreadsCABundleToClientFactory(t *testing.T) {
	ctx := t.Context()
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
	ctx := t.Context()
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
	if s := repoConditionStatus(got, quayv1alpha1.ConditionReady); s != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False for an invalid caBundle", s)
	}
	if reason := repoConditionReason(got, quayv1alpha1.ConditionReady); reason != quayv1alpha1.ReasonQuayError {
		t.Errorf("Ready reason = %q, want %q", reason, quayv1alpha1.ReasonQuayError)
	}
}

func TestRepositoryDescriptionValidationReservesOwnershipMarkerLength(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)

	accepted := &quayv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "accepted-description"},
		Spec: quayv1alpha1.RepositorySpec{
			OrganizationRef: "acme",
			Name:            "accepted-description",
			Description:     strings.Repeat("x", quayv1alpha1.RepositoryDescriptionMaxLength),
		},
	}
	if err := shared.k8sClient.Create(ctx, accepted); err != nil {
		t.Fatalf("creating Repository with max reserved description length: %v", err)
	}

	rejected := &quayv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rejected-description"},
		Spec: quayv1alpha1.RepositorySpec{
			OrganizationRef: "acme",
			Name:            "rejected-description",
			Description:     strings.Repeat("x", quayv1alpha1.RepositoryDescriptionMaxLength+1),
		},
	}
	if err := shared.k8sClient.Create(ctx, rejected); err == nil {
		t.Fatalf("expected Repository description longer than %d chars to be rejected", quayv1alpha1.RepositoryDescriptionMaxLength)
	} else if !apierrors.IsInvalid(err) {
		t.Fatalf("creating over-length Repository returned %T %v, want Invalid", err, err)
	}
}

func TestRepositoryMarkedDescriptionMaxLengthFitsQuay(t *testing.T) {
	repo := &quayv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			UID: "123e4567-e89b-12d3-a456-426614174000",
		},
		Spec: quayv1alpha1.RepositorySpec{
			Description: strings.Repeat("x", quayv1alpha1.RepositoryDescriptionMaxLength),
		},
	}

	if got := len(repositoryMarkedDescription(repo, true)); got != 4096 {
		t.Errorf("created marked description length = %d, want Quay limit 4096", got)
	}
	if got := len(repositoryMarkedDescription(repo, false)); got != 4096 {
		t.Errorf("adopted marked description length = %d, want Quay limit 4096", got)
	}
}

func TestRepositoryCreatesWithSecretRefWebhook(t *testing.T) {
	ctx := t.Context()
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
			URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
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
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionWebhookConfigured); got != metav1.ConditionTrue {
		t.Errorf("WebhookConfigured = %q, want True", got)
	}
	// The secret-backed webhook URL is sensitive; it must NOT appear in status.
	if c := meta.FindStatusCondition(repo.Status.Conditions, quayv1alpha1.ConditionWebhookConfigured); c != nil && strings.Contains(c.Message, hook) {
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
	ctx := t.Context()
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
			URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
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

func TestRedactWebhookURLRedactsTruncatedSecretURLPrefix(t *testing.T) {
	const secretURL = "https://kargo.example.test/webhook/super-secret-token"
	repo := &quayv1alpha1.Repository{
		Spec: quayv1alpha1.RepositorySpec{
			Webhook: &quayv1alpha1.RepositoryWebhook{
				URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
			},
		},
	}
	partialURL := secretURL[:len(secretURL)-6]
	err := redactWebhookURL(
		errors.New("invalid webhook config for url "+partialURL+"...[truncated]"),
		repo,
		secretURL,
	)
	if strings.Contains(err.Error(), partialURL) {
		t.Fatalf("redacted error leaked partial secret URL: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("redacted error = %q, want redaction marker", err.Error())
	}
}

func TestRepositoryWebhookSecretReadErrorPersistsPriorMutation(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "readerror", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{
			URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "hook-url", Key: "url"},
		},
	})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	r.APIReader = webhookSecretFailReader{
		Reader:    shared.k8sClient,
		namespace: ns,
		name:      "hook-url",
		err:       errors.New("temporary apiserver read failure"),
	}

	if err := reconcileRepoUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail after repository create and webhook Secret read error")
	}

	if !fake.repoExists("acme", "readerror") {
		t.Fatal("expected repository create to complete before webhook Secret read failed")
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False after webhook Secret read error", got)
	}
	if got := repoConditionReason(repo, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonWebhookURLReadError {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonWebhookURLReadError)
	}
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionWebhookConfigured); got != metav1.ConditionFalse {
		t.Errorf("WebhookConfigured = %q, want False after webhook Secret read error", got)
	}
	if got := repoConditionReason(repo, quayv1alpha1.ConditionWebhookConfigured); got != quayv1alpha1.ReasonWebhookURLReadError {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, quayv1alpha1.ReasonWebhookURLReadError)
	}
	if repo.Status.LastMutatedTime == nil || repo.Status.LastMutationReason != quayv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want stamped SpecChange after repository create", repo.Status.LastMutatedTime, repo.Status.LastMutationReason)
	}
	if repo.Status.LastValidatedTime != nil {
		t.Errorf("lastValidatedTime = %v, want nil on failed validation", repo.Status.LastValidatedTime)
	}
}

func TestRepositorySecretRefMissingKeySetsConditionAndRequeues(t *testing.T) {
	ctx := t.Context()
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
			URLSecretRef: &quayv1alpha1.WebhookURLSecretRef{Name: "kargo-receiver", Key: "url"},
		},
	})

	fake := newFakeRepoClient()
	r, recorder := newRepoReconciler(fake, ns)

	// First pass adds the finalizer; the next hits the missing key.
	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	result, err := reconcileRepo(ctx, r, key)
	if err != nil {
		t.Fatalf("expected nil error while waiting for webhook URL Secret key, got %v", err)
	}
	if result.RequeueAfter != requeueDependency {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, requeueDependency)
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
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionWebhookConfigured); got != metav1.ConditionFalse {
		t.Errorf("WebhookConfigured = %q, want False", got)
	}
	if got := repoConditionReason(repo, quayv1alpha1.ConditionWebhookConfigured); got != quayv1alpha1.ReasonWebhookURLNotFound {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, quayv1alpha1.ReasonWebhookURLNotFound)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonWebhookURLNotFound)
}

func TestRepositoryCorrectsVisibilityAndDescriptionDrift(t *testing.T) {
	ctx := t.Context()
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
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	fake.mu.Lock()
	st := fake.repos[repoKey("acme", "drift")]
	st.isPublic = false
	st.description = "stale description\n\n" + repositoryOwnerMarker(getRepo(ctx, t, key), true)
	fake.mu.Unlock()

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !fake.callsContain("UpdateRepositoryVisibility:acme/drift") {
		t.Errorf("expected a visibility update, calls were %v", fake.calls)
	}
	if !fake.callsContain("UpdateRepositoryDescription:acme/drift") {
		t.Errorf("expected a description update, calls were %v", fake.calls)
	}
	st = fake.repos[repoKey("acme", "drift")]
	if !st.isPublic {
		t.Error("expected visibility corrected to public")
	}
	if got := repositoryDescriptionWithoutOwner(st.description); got != "the desired description" {
		t.Errorf("description = %q, want corrected", st.description)
	}
}

func TestRepositoryExistingUnclaimedWithoutAdoptIsConflict(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "foreign", repoOpts{})

	fake := newFakeRepoClient()
	fake.repos[repoKey("acme", "foreign")] = &fakeRepoStore{
		isPublic:      false,
		description:   "human repo",
		notifications: map[string]quay.Notification{},
	}
	r, recorder := newRepoReconciler(fake, ns)

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("conflict reconcile should not requeue as an error: %v", err)
	}

	repo := getRepo(ctx, t, key)
	if got := repoConditionReason(repo, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonConflict {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonConflict)
	}
	if repo.Status.Created != nil && *repo.Status.Created {
		t.Errorf("status.created = %v, want unset/false on conflict", repo.Status.Created)
	}
	if st := fake.repos[repoKey("acme", "foreign")]; st.description != "human repo" {
		t.Errorf("description mutated on conflict: %q", st.description)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonConflict)
}

func TestRepositoryAdoptsExistingRepository(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "adopted", repoOpts{
		adopt:       true,
		description: "managed description",
	})

	fake := newFakeRepoClient()
	fake.repos[repoKey("acme", "adopted")] = &fakeRepoStore{
		isPublic:      false,
		description:   "human repo",
		notifications: map[string]quay.Notification{},
	}
	r, _ := newRepoReconciler(fake, ns)

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo := getRepo(ctx, t, key)
	if repo.Status.Created == nil || *repo.Status.Created {
		t.Errorf("status.created = %v, want false for adopted repository", repo.Status.Created)
	}
	if repo.Status.LastMutatedTime == nil || repo.Status.LastMutationReason != quayv1alpha1.MutationReasonSpecChange {
		t.Errorf("mutation status = (%v, %q), want adoption mutation stamped", repo.Status.LastMutatedTime, repo.Status.LastMutationReason)
	}
	st := fake.repos[repoKey("acme", "adopted")]
	if got := repositoryDescriptionOwner(st.description); got != repositoryOwnerToken(repo) {
		t.Errorf("owner marker = %q, want this Repository UID %q", got, repositoryOwnerToken(repo))
	}
	if ownership, ok := repositoryDescriptionOwnership(st.description); !ok || ownership.Created {
		t.Errorf("ownership marker = (%+v, %v), want adopted marker", ownership, ok)
	}
	if got := repositoryDescriptionWithoutOwner(st.description); got != "managed description" {
		t.Errorf("description without marker = %q, want managed description", got)
	}
}

func TestRepositoryAdoptionReusesExistingUIDTitledWebhook(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const hook = "https://kargo.example.test/webhook/adopt-existing"
	key := makeRepo(ctx, t, ns, "acme", "adopthook", repoOpts{
		adopt:   true,
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(hook)},
	})

	fake := newFakeRepoClient()
	fake.repos[repoKey("acme", "adopthook")] = &fakeRepoStore{
		isPublic:      false,
		description:   "human repo",
		notifications: map[string]quay.Notification{},
	}
	repo := getRepo(ctx, t, key)
	existingUUID := fake.seedNotification("acme", "adopthook", repositoryWebhookTitle(repo), hook)
	fake.seedNotification("acme", "adopthook", "holos-controller repo_push created:foreign", hook)

	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo = getRepo(ctx, t, key)
	if got := repo.Status.WebhookNotificationUUID; got != existingUUID {
		t.Fatalf("status.webhookNotificationUUID = %q, want existing UUID %q", got, existingUUID)
	}
	if fake.callsContain("CreateNotification:acme/adopthook:" + hook) {
		t.Errorf("adoption should reuse existing UID-titled webhook instead of creating a duplicate; calls were %v", fake.calls)
	}
	if urls := fake.webhookURLs("acme", "adopthook"); len(urls) != 2 {
		t.Errorf("webhook URLs = %v, want existing owned plus foreign hook preserved", urls)
	}
}

func TestRepositoryAdoptStatusLossDoesNotBecomeCreated(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "adoptlost", repoOpts{
		adopt:       true,
		description: "survives",
	})

	fake := newFakeRepoClient()
	fake.repos[repoKey("acme", "adoptlost")] = &fakeRepoStore{
		isPublic:      false,
		description:   "pre-existing",
		notifications: map[string]quay.Notification{},
	}
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile adopt: %v", err)
	}

	repo := getRepo(ctx, t, key)
	repo.Status = quayv1alpha1.RepositoryStatus{}
	if err := shared.k8sClient.Status().Update(ctx, repo); err != nil {
		t.Fatalf("clearing status after adopt: %v", err)
	}

	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile after lost status: %v", err)
	}
	repo = getRepo(ctx, t, key)
	if repo.Status.Created == nil || *repo.Status.Created {
		t.Fatalf("status.created = %v, want false restored from adopted marker", repo.Status.Created)
	}

	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if fake.callsContain("DeleteRepository:acme/adoptlost") {
		t.Errorf("adopted repository with lost status must be released, not deleted; calls were %v", fake.calls)
	}
	if !fake.repoExists("acme", "adoptlost") {
		t.Fatal("expected adopted repository to remain in Quay")
	}
}

func TestRepositoryStampsPartialMutationWhenDescriptionUpdateFails(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "partial", repoOpts{
		visibility:  quayv1alpha1.RepositoryVisibilityPrivate,
		description: "desired description",
	})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	before := getRepo(ctx, t, key)
	firstValidated := before.Status.LastValidatedTime.DeepCopy()
	firstMutated := before.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	fake.mu.Lock()
	st := fake.repos[repoKey("acme", "partial")]
	st.isPublic = true
	st.description = "remote drift\n\n" + repositoryOwnerMarker(getRepo(ctx, t, key), true)
	fake.updateDescriptionErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "description boom"}
	fake.mu.Unlock()

	if err := reconcileRepoUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail after visibility update and description failure")
	}

	repo := getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False after partial repository update failure", got)
	}
	if repo.Status.LastValidatedTime == nil || !repo.Status.LastValidatedTime.Equal(firstValidated) {
		t.Errorf("lastValidatedTime changed on failed reconcile: before=%v after=%v", firstValidated, repo.Status.LastValidatedTime)
	}
	if repo.Status.LastMutatedTime == nil || !repo.Status.LastMutatedTime.After(firstMutated.Time) {
		t.Errorf("lastMutatedTime did not advance after partial repository update: before=%v after=%v", firstMutated, repo.Status.LastMutatedTime)
	}
	if got := repo.Status.LastMutationReason; got != quayv1alpha1.MutationReasonDriftRemediation {
		t.Errorf("lastMutationReason = %q, want %q", got, quayv1alpha1.MutationReasonDriftRemediation)
	}
	if repo.Status.LastDriftTime == nil || !repo.Status.LastDriftTime.Equal(repo.Status.LastMutatedTime) {
		t.Errorf("lastDriftTime = %v, want same instant as lastMutatedTime %v", repo.Status.LastDriftTime, repo.Status.LastMutatedTime)
	}
}

func TestRepositoryWebhookURLChangeReplacesNotification(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const newHook = "https://kargo.example.test/webhook/new"
	key := makeRepo(ctx, t, ns, "acme", "rehook", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(newHook)},
	})

	fake := newFakeRepoClient()
	// Pre-create the repo with a stale recorded repo_push webhook pointing
	// elsewhere plus a manually-created webhook with a different title that must be
	// preserved.
	repo := getRepo(ctx, t, key)
	fake.repos[repoKey("acme", "rehook")] = &fakeRepoStore{
		description:   repositoryOwnerMarker(repo, true),
		notifications: map[string]quay.Notification{},
	}
	repo.Status.WebhookNotificationUUID = fake.seedNotification("acme", "rehook", repositoryWebhookTitle(repo), "https://old.example.test/stale")
	if err := shared.k8sClient.Status().Update(ctx, repo); err != nil {
		t.Fatalf("recording stale webhook UUID in status: %v", err)
	}
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

func TestRepositoryWebhookStatusLossRecoversUUIDWithoutDuplicate(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const hook = "https://kargo.example.test/webhook/recover"
	key := makeRepo(ctx, t, ns, "acme", "recoverhook", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(hook)},
	})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	repo := getRepo(ctx, t, key)
	if repo.Status.WebhookNotificationUUID == "" {
		t.Fatal("status.webhookNotificationUUID is empty after initial reconcile")
	}

	repo.Status.WebhookNotificationUUID = ""
	if err := shared.k8sClient.Status().Update(ctx, repo); err != nil {
		t.Fatalf("clearing webhook UUID status: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile after status loss: %v", err)
	}

	repo = getRepo(ctx, t, key)
	if repo.Status.WebhookNotificationUUID == "" {
		t.Fatal("status.webhookNotificationUUID was not recovered")
	}
	if urls := fake.webhookURLs("acme", "recoverhook"); len(urls) != 1 || urls[0] != hook {
		t.Errorf("webhook URLs after status recovery = %v, want exactly [%s]", urls, hook)
	}
}

func TestRepositoryWebhookDuplicateCleanupKeepsRecordedUUID(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const hook = "https://kargo.example.test/webhook/dedupe"
	key := makeRepo(ctx, t, ns, "acme", "dedupehook", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(hook)},
	})

	fake := newFakeRepoClient()
	repo := getRepo(ctx, t, key)
	fake.repos[repoKey("acme", "dedupehook")] = &fakeRepoStore{
		description:   repositoryOwnerMarker(repo, true),
		notifications: map[string]quay.Notification{},
	}
	keptUUID := fake.seedNotification("acme", "dedupehook", repositoryWebhookTitle(repo), hook)
	duplicateUUID := fake.seedNotification("acme", "dedupehook", repositoryWebhookTitle(repo), hook)

	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile duplicate cleanup: %v", err)
	}

	repo = getRepo(ctx, t, key)
	if got := repo.Status.WebhookNotificationUUID; got != keptUUID {
		t.Fatalf("status.webhookNotificationUUID = %q, want kept UUID %q", got, keptUUID)
	}
	if urls := fake.webhookURLs("acme", "dedupehook"); len(urls) != 1 || urls[0] != hook {
		t.Errorf("webhook URLs after duplicate cleanup = %v, want exactly [%s]", urls, hook)
	}
	if !fake.callsContain("DeleteNotification:acme/dedupehook:" + duplicateUUID) {
		t.Errorf("expected duplicate webhook %q to be deleted; calls were %v", duplicateUUID, fake.calls)
	}
}

func TestRepositoryStampsWebhookDeleteBeforeFailedCreate(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	oldHook := "https://kargo.example.test/webhook/old"
	key := makeRepo(ctx, t, ns, "acme", "webhookpartial", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(oldHook)},
	})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	before := getRepo(ctx, t, key)
	firstValidated := before.Status.LastValidatedTime.DeepCopy()
	firstMutated := before.Status.LastMutatedTime.DeepCopy()

	time.Sleep(time.Second + 100*time.Millisecond)
	repo := getRepo(ctx, t, key)
	newHook := "https://kargo.example.test/webhook/new"
	repo.Spec.Webhook.URL = ptr(newHook)
	if err := shared.k8sClient.Update(ctx, repo); err != nil {
		t.Fatalf("updating webhook URL: %v", err)
	}
	fake.createNotifErr = &quay.APIError{StatusCode: http.StatusInternalServerError, Message: "create notification boom"}

	if err := reconcileRepoUntilStable(ctx, t, r, key); err == nil {
		t.Fatal("expected reconcile to fail after deleting stale webhook and failing create")
	}

	urls := fake.webhookURLs("acme", "webhookpartial")
	if containsString(urls, oldHook) {
		t.Errorf("stale webhook URL should have been deleted before create failed; URLs = %v", urls)
	}
	if containsString(urls, newHook) {
		t.Errorf("new webhook URL should not exist because create failed; URLs = %v", urls)
	}

	repo = getRepo(ctx, t, key)
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionReady); got != metav1.ConditionFalse {
		t.Errorf("Ready = %q, want False after webhook create failure", got)
	}
	if repo.Status.LastValidatedTime == nil || !repo.Status.LastValidatedTime.Equal(firstValidated) {
		t.Errorf("lastValidatedTime changed on failed reconcile: before=%v after=%v", firstValidated, repo.Status.LastValidatedTime)
	}
	if repo.Status.LastMutatedTime == nil || !repo.Status.LastMutatedTime.After(firstMutated.Time) {
		t.Errorf("lastMutatedTime did not advance after webhook partial mutation: before=%v after=%v", firstMutated, repo.Status.LastMutatedTime)
	}
	if got := repo.Status.LastMutationReason; got != quayv1alpha1.MutationReasonSpecChange {
		t.Errorf("lastMutationReason = %q, want %q", got, quayv1alpha1.MutationReasonSpecChange)
	}
	if repo.Status.LastDriftTime != nil {
		t.Errorf("lastDriftTime = %v, want nil for a pure spec-driven webhook URL update", repo.Status.LastDriftTime)
	}
}

func TestRepositoryOrganizationNotReadyRequeues(t *testing.T) {
	ctx := t.Context()
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
	result, err := reconcileRepo(ctx, r, key)
	if err != nil {
		t.Fatalf("expected nil error while waiting for Organization, got %v", err)
	}
	if result.RequeueAfter != requeueDependency {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, requeueDependency)
	}

	// No repository was created: the reconciler never creates the org and must not
	// create a repo before the org exists.
	if fake.callsContain("CreateRepository:missing-org/web") {
		t.Errorf("must not create a repo before the org exists; calls were %v", fake.calls)
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionReason(repo, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonOrganizationNotReady {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonOrganizationNotReady)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonOrganizationNotReady)
}

func TestRepositoryMissingCredentialSetsConditionAndNoQuayCall(t *testing.T) {
	ctx := t.Context()
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
	if got := repoConditionReason(repo, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonCredentialsNotFound {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonCredentialsNotFound)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonCredentialsNotFound)
}

func TestRepositoryNoWebhookIsWebhookless(t *testing.T) {
	ctx := t.Context()
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
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True", got)
	}
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionWebhookConfigured); got != metav1.ConditionFalse {
		t.Errorf("WebhookConfigured = %q, want False (webhookless)", got)
	}
	if got := repoConditionReason(repo, quayv1alpha1.ConditionWebhookConfigured); got != quayv1alpha1.ReasonWebhookNotConfigured {
		t.Errorf("WebhookConfigured reason = %q, want %q", got, quayv1alpha1.ReasonWebhookNotConfigured)
	}
}

func TestRepositoryDeleteRemovesFinalizerAfterQuayDelete(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "doomed", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr("https://kargo.example.test/webhook/x")},
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

func TestRepositoryDeleteReleasesAdoptedRepositoryWithoutDeleting(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const adoptedHook = "https://kargo.example.test/webhook/adopted"
	const manualHook = "https://example.test/manual"
	key := makeRepo(ctx, t, ns, "acme", "adoptdelete", repoOpts{
		adopt:       true,
		description: "survives",
		webhook:     &quayv1alpha1.RepositoryWebhook{URL: ptr(adoptedHook)},
	})

	fake := newFakeRepoClient()
	fake.repos[repoKey("acme", "adoptdelete")] = &fakeRepoStore{
		isPublic:      false,
		description:   "pre-existing",
		notifications: map[string]quay.Notification{},
	}
	r, recorder := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile adopt: %v", err)
	}
	repo := getRepo(ctx, t, key)
	if repo.Status.Created == nil || *repo.Status.Created {
		t.Fatalf("status.created = %v, want false after adopt", repo.Status.Created)
	}
	if repo.Status.WebhookNotificationUUID == "" {
		t.Fatal("status.webhookNotificationUUID is empty, want recorded controller webhook UUID")
	}
	fake.seedNotification("acme", "adoptdelete", webhookTitlePrefix, manualHook)
	if urls := fake.webhookURLs("acme", "adoptdelete"); len(urls) != 2 || !containsString(urls, adoptedHook) || !containsString(urls, manualHook) {
		t.Fatalf("webhook URLs before delete = %v, want controller and manual hooks", urls)
	}

	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("DeleteRepository:acme/adoptdelete") {
		t.Errorf("adopted repository must be released, not deleted; calls were %v", fake.calls)
	}
	if !fake.repoExists("acme", "adoptdelete") {
		t.Fatal("expected adopted repository to remain in Quay")
	}
	st := fake.repos[repoKey("acme", "adoptdelete")]
	if owner := repositoryDescriptionOwner(st.description); owner != "" {
		t.Errorf("owner marker = %q, want cleared on release", owner)
	}
	if st.description != "survives" {
		t.Errorf("description after release = %q, want survives", st.description)
	}
	if urls := fake.webhookURLs("acme", "adoptdelete"); len(urls) != 1 || urls[0] != manualHook {
		t.Errorf("webhook URLs after release = %v, want only manual hook", urls)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonReleased)
}

func TestRepositoryDeleteOrphansCreatedRepository(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const hook = "https://kargo.example.test/webhook/orphan"
	key := makeRepo(ctx, t, ns, "acme", "orphanrepo", repoOpts{
		description:    "kept",
		deletionPolicy: quayv1alpha1.DeletionPolicyOrphan,
		webhook:        &quayv1alpha1.RepositoryWebhook{URL: ptr(hook)},
	})

	fake := newFakeRepoClient()
	r, recorder := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	repo := getRepo(ctx, t, key)
	managedUUID := repo.Status.WebhookNotificationUUID
	if managedUUID == "" {
		t.Fatal("status.webhookNotificationUUID is empty, want recorded webhook")
	}

	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("DeleteRepository:acme/orphanrepo") {
		t.Errorf("orphan policy must not delete repository; calls were %v", fake.calls)
	}
	if fake.callsContain("DeleteNotification:acme/orphanrepo:" + managedUUID) {
		t.Errorf("orphan policy must not remove recorded webhook; calls were %v", fake.calls)
	}
	if !fake.repoExists("acme", "orphanrepo") {
		t.Fatal("expected orphaned repository to remain in Quay")
	}
	st := fake.repos[repoKey("acme", "orphanrepo")]
	if got := st.description; got != "kept" {
		t.Errorf("description after orphan = %q, want marker stripped to kept", got)
	}
	if urls := fake.webhookURLs("acme", "orphanrepo"); len(urls) != 1 || urls[0] != hook {
		t.Errorf("webhook URLs after orphan = %v, want recorded webhook preserved", urls)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonReleased)
}

func TestRepositoryDeleteDeletesAdoptedRepositoryWhenPolicyDeleteAndOwned(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "deleteadopted", repoOpts{
		adopt:          true,
		deletionPolicy: quayv1alpha1.DeletionPolicyDelete,
	})

	fake := newFakeRepoClient()
	fake.repos[repoKey("acme", "deleteadopted")] = &fakeRepoStore{
		isPublic:      false,
		description:   "pre-existing",
		notifications: map[string]quay.Notification{},
	}
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile adopt: %v", err)
	}

	repo := getRepo(ctx, t, key)
	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if !fake.callsContain("DeleteRepository:acme/deleteadopted") {
		t.Errorf("expected explicit Delete policy to delete owned adopted repository; calls were %v", fake.calls)
	}
	if fake.repoExists("acme", "deleteadopted") {
		t.Fatal("expected adopted repository to be deleted")
	}
}

func TestRepositoryDeletePolicyDeleteReleasesForeignMarker(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "deleteforeign", repoOpts{
		deletionPolicy: quayv1alpha1.DeletionPolicyDelete,
		webhook:        &quayv1alpha1.RepositoryWebhook{URL: ptr("https://kargo.example.test/webhook/foreign")},
	})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	repo := getRepo(ctx, t, key)
	fake.repos[repoKey("acme", "deleteforeign")].description = "foreign\n\n" + repositoryOwnerMarkerPrefix + repositoryOwnerMarkerCreated + ":foreign-token"

	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("DeleteRepository:acme/deleteforeign") {
		t.Errorf("foreign marker must release, not delete; calls were %v", fake.calls)
	}
	if !fake.repoExists("acme", "deleteforeign") {
		t.Fatal("expected foreign-owned repository to remain in Quay")
	}
}

func TestRepositoryDeleteWithForeignMarkerRemovesManagedWebhook(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	const managedHook = "https://kargo.example.test/webhook/managed"
	const manualHook = "https://example.test/manual"
	key := makeRepo(ctx, t, ns, "acme", "foreignmarker", repoOpts{
		webhook: &quayv1alpha1.RepositoryWebhook{URL: ptr(managedHook)},
	})

	fake := newFakeRepoClient()
	r, recorder := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	repo := getRepo(ctx, t, key)
	managedUUID := repo.Status.WebhookNotificationUUID
	if managedUUID == "" {
		t.Fatal("status.webhookNotificationUUID is empty, want recorded controller webhook UUID")
	}
	foreignDescription := "taken over\n\n" + repositoryOwnerMarkerPrefix + repositoryOwnerMarkerCreated + ":foreign-token"
	fake.repos[repoKey("acme", "foreignmarker")].description = foreignDescription
	fake.seedNotification("acme", "foreignmarker", webhookTitlePrefix, manualHook)
	if urls := fake.webhookURLs("acme", "foreignmarker"); len(urls) != 2 || !containsString(urls, managedHook) || !containsString(urls, manualHook) {
		t.Fatalf("webhook URLs before delete = %v, want controller and manual hooks", urls)
	}

	if err := shared.k8sClient.Delete(ctx, repo); err != nil {
		t.Fatalf("deleting Repository: %v", err)
	}
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if fake.callsContain("DeleteRepository:acme/foreignmarker") {
		t.Errorf("foreign-owned repository must be released, not deleted; calls were %v", fake.calls)
	}
	if !fake.repoExists("acme", "foreignmarker") {
		t.Fatal("expected foreign-owned repository to remain in Quay")
	}
	if !fake.callsContain("DeleteNotification:acme/foreignmarker:" + managedUUID) {
		t.Errorf("expected managed webhook %q to be deleted; calls were %v", managedUUID, fake.calls)
	}
	if urls := fake.webhookURLs("acme", "foreignmarker"); len(urls) != 1 || urls[0] != manualHook {
		t.Errorf("webhook URLs after release = %v, want only manual hook", urls)
	}
	if got := fake.repos[repoKey("acme", "foreignmarker")].description; got != foreignDescription {
		t.Errorf("description after release = %q, want foreign marker preserved as %q", got, foreignDescription)
	}
	assertEvent(t, recorder, quayv1alpha1.ReasonReleased)
}

func TestRepositoryCreateRaceConfirmedByGet(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	makeReadyOrg(ctx, t, ns, "acme")
	key := makeRepo(ctx, t, ns, "acme", "race", repoOpts{})

	fake := newFakeRepoClient()
	fake.createRepoRace = true
	r, _ := newRepoReconciler(fake, ns)
	if err := reconcileRepoUntilStable(ctx, t, r, key); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo := getRepo(ctx, t, key)
	if repo.Status.Created == nil || !*repo.Status.Created {
		t.Errorf("status.created = %v, want true when GET confirms our create", repo.Status.Created)
	}
	if got := repoConditionStatus(repo, quayv1alpha1.ConditionReady); got != metav1.ConditionTrue {
		t.Errorf("Ready = %q, want True after confirmed create race", got)
	}
}

func TestRepositoryResolvesQuayOrgThroughOrganizationSpecName(t *testing.T) {
	ctx := t.Context()
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
	if err := shared.k8sClient.Get(ctx, client.ObjectKeyFromObject(org), org); err != nil {
		t.Fatalf("getting created Organization: %v", err)
	}
	meta.SetStatusCondition(&org.Status.Conditions, metav1.Condition{
		Type: quayv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: quayv1alpha1.ReasonCreated, Message: "ready", ObservedGeneration: org.Generation,
	})
	org.Status.ObservedGeneration = org.Generation
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
	ctx := t.Context()
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
	result, err := reconcileRepo(ctx, r, key)
	if err != nil {
		t.Fatalf("expected nil error while waiting for Organization readiness, got %v", err)
	}
	if result.RequeueAfter != requeueDependency {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, requeueDependency)
	}

	if len(fake.calls) != 0 {
		t.Errorf("expected no Quay calls while the org is not Ready, got %v", fake.calls)
	}
	repo := getRepo(ctx, t, key)
	if got := repoConditionReason(repo, quayv1alpha1.ConditionReady); got != quayv1alpha1.ReasonOrganizationNotReady {
		t.Errorf("Ready reason = %q, want %q", got, quayv1alpha1.ReasonOrganizationNotReady)
	}
}

func TestRepositoryOrganizationReadyMustBeCurrentGeneration(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	if err := shared.k8sClient.Create(ctx, newCredentialSecret(ns, "holos-controller-quay-creds")); err != nil {
		t.Fatalf("creating credential secret: %v", err)
	}
	orgName := makeReadyOrg(ctx, t, ns, "stale-org")
	org := &quayv1alpha1.Organization{}
	if err := shared.k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "stale-org"}, org); err != nil {
		t.Fatalf("getting Organization: %v", err)
	}
	org.Spec.Email = "new@example.test"
	if err := shared.k8sClient.Update(ctx, org); err != nil {
		t.Fatalf("updating Organization spec: %v", err)
	}
	key := makeRepo(ctx, t, ns, "stale-org", "web", repoOpts{})

	fake := newFakeRepoClient()
	r, _ := newRepoReconciler(fake, ns)

	if _, err := reconcileRepo(ctx, r, key); err != nil {
		t.Fatalf("first reconcile (finalizer): %v", err)
	}
	result, err := reconcileRepo(ctx, r, key)
	if err != nil {
		t.Fatalf("expected nil error for stale Organization Ready wait, got %v", err)
	}
	if result.RequeueAfter != requeueDependency {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, requeueDependency)
	}
	if fake.repoExists(orgName, "web") {
		t.Fatalf("must not create a Repository while Organization Ready is stale; calls were %v", fake.calls)
	}
}

func TestRepositoriesForOrganizationMapsDependents(t *testing.T) {
	ctx := t.Context()
	ns := makeNamespace(ctx, t)
	matching := makeRepo(ctx, t, ns, "mapped-org", "web", repoOpts{})
	other := makeRepo(ctx, t, ns, "other-org", "api", repoOpts{})
	_ = other
	org := &quayv1alpha1.Organization{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mapped-org"}}
	r, _ := newRepoReconciler(newFakeRepoClient(), ns)

	requests := r.repositoriesForOrganization(ctx, org)
	if len(requests) != 1 {
		t.Fatalf("mapped requests = %v, want one request", requests)
	}
	if requests[0].NamespacedName != matching {
		t.Fatalf("mapped request = %s, want %s", requests[0].NamespacedName, matching)
	}
}

func TestRepositoryDeleteUsesCreatedMarkerWhenStatusEmpty(t *testing.T) {
	ctx := t.Context()
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

	// Simulate a crash that created the Quay repo but never persisted status:
	// clear status.QuayRepository and status.created so the finalizer must fall
	// back to the spec + Organization CR for the path and the remote marker for
	// created-vs-adopted ownership.
	repo := getRepo(ctx, t, key)
	repo.Status = quayv1alpha1.RepositoryStatus{}
	if err := shared.k8sClient.Status().Update(ctx, repo); err != nil {
		t.Fatalf("clearing status: %v", err)
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

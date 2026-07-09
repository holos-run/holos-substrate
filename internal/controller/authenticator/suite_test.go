package authenticator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	authenticatorv1alpha1 "github.com/holos-run/holos-substrate/api/authenticator/v1alpha1"
	"github.com/holos-run/holos-substrate/internal/authenticator"
)

// testEnv holds the shared envtest control plane and client each test reuses.
// Standing up kube-apiserver + etcd is expensive, so it is set up once per
// package run by TestMain and torn down afterward, mirroring the quay suite.
type testEnv struct {
	cfg       *rest.Config
	k8sClient client.Client
	env       *envtest.Environment
}

// shared is the package-level envtest fixture. It is nil when KUBEBUILDER_ASSETS
// is unset (TestMain skips setup in that case).
var shared *testEnv

// TestMain stands up a single envtest control plane for the whole package: it
// installs the authenticator.holos.run CRDs from
// config/crd/holos-authenticator/bases, registers the scheme, and builds a
// client. When KUBEBUILDER_ASSETS is unset the package is skipped cleanly (exit
// 0 with no tests run) so the repo-wide `go test ./...` stays green — the
// envtest-backed reconcile tests run under `make authenticator-test` with the
// control-plane binaries provisioned.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		os.Exit(0)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "holos-authenticator", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		panic("starting envtest (is KUBEBUILDER_ASSETS set? run via `make authenticator-test`): " + err.Error())
	}

	if err := authenticatorv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic("adding authenticator scheme: " + err.Error())
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("building client: " + err.Error())
	}

	shared = &testEnv{cfg: cfg, k8sClient: k8sClient, env: env}

	code := m.Run()

	if err := env.Stop(); err != nil {
		panic("stopping envtest: " + err.Error())
	}

	os.Exit(code)
}

// newReconciler builds a BackendReconciler wired to the envtest client, a
// recording event recorder, a fresh Store, and the supplied discovery func so
// tests inject a fake verifier without a live issuer.
func newReconciler(discover authenticator.DiscoverFunc) (*BackendReconciler, *authenticator.Store, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	store := authenticator.NewStore()
	r := &BackendReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Store:     store,
		Discover:  discover,
	}
	return r, store, recorder
}

// reconcile runs a single Reconcile pass for the named Backend.
func reconcile(ctx context.Context, r *BackendReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// stubVerifier is a TokenVerifier that does nothing; the reconcile tests only
// need the verifier to be constructed (discovery succeeded), not exercised.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (*authenticator.VerifiedToken, error) {
	return &authenticator.VerifiedToken{Claims: map[string]any{}}, nil
}

// discoverOK is a DiscoverFunc that always succeeds, returning a stub verifier —
// the success path for reconcile tests that should reach Ready.
func discoverOK(context.Context, string, string, []byte) (authenticator.TokenVerifier, error) {
	return stubVerifier{}, nil
}

// makeBackend returns a Backend fixture in namespace with the given host and
// issuer, ready to k8sClient.Create.
func makeBackend(namespace, name, host string) *authenticatorv1alpha1.Backend {
	return &authenticatorv1alpha1.Backend{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: authenticatorv1alpha1.BackendSpec{
			Host: host,
			Server: authenticatorv1alpha1.ServerConfig{
				URL: "https://api.example.test:6443",
			},
			OIDC: authenticatorv1alpha1.OIDCConfig{
				IssuerURL:     "https://issuer.example.test/realms/holos",
				ClientID:      "holos-authenticator",
				UsernameClaim: "sub",
				GroupsClaim:   "groups",
			},
		},
	}
}

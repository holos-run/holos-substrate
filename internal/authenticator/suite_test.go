package authenticator

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
)

// sharedEnv holds the package-level envtest control plane and writable client the
// TokenManager tests reuse. Standing up kube-apiserver + etcd is expensive, so it
// is set up once per package run by TestMain and torn down afterward, mirroring the
// controller suite (internal/controller/authenticator/suite_test.go).
type sharedEnv struct {
	cfg       *rest.Config
	k8sClient client.Client
	env       *envtest.Environment
}

// envtestShared is the package-level envtest fixture. It is nil when
// KUBEBUILDER_ASSETS is unset (TestMain skips setup), in which case the
// envtest-backed TokenManager tests skip cleanly while the rest of the package's
// tests (server, store, mapping, oidc) still run.
var envtestShared *sharedEnv

// TestMain stands up a single envtest control plane for the whole package so the
// TokenManager tests can mint real ServiceAccount tokens via the TokenRequest API.
// When KUBEBUILDER_ASSETS is unset it skips control-plane setup (leaving
// envtestShared nil) so the repo-wide `go test ./...` stays green — the
// envtest-backed tests run under `make authenticator-test`, which provisions the
// control-plane binaries. The non-envtest tests in this package run regardless.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		os.Exit(m.Run())
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
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

	envtestShared = &sharedEnv{cfg: cfg, k8sClient: k8sClient, env: env}

	code := m.Run()

	if err := env.Stop(); err != nil {
		panic("stopping envtest: " + err.Error())
	}

	os.Exit(code)
}

package quay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	quayv1alpha1 "github.com/holos-run/holos-substrate/api/quay/v1alpha1"
)

// validTestCABundle generates a real, parseable PEM-encoded x509 certificate so
// reconciler tests can set a valid spec.caBundle that survives the controller's
// up-front quay.ValidateCABundle check. (A placeholder string like "MIIB...test"
// is not a valid certificate and is rejected before any Quay call.)
func validTestCABundle(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "holos-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// testEnv holds the shared envtest control plane and clients each test reuses.
// It is set up once per package run by TestMain and torn down afterward, because
// standing up kube-apiserver + etcd is expensive.
type testEnv struct {
	cfg       *rest.Config
	k8sClient client.Client
	env       *envtest.Environment
}

// shared is the package-level envtest fixture. It is nil when KUBEBUILDER_ASSETS
// is unset (TestMain skips setup in that case, see below).
var shared *testEnv

// TestMain stands up a single envtest control plane for the whole package: it
// installs the quay.holos.run CRDs from config/crd/holos-controller/bases,
// registers the scheme, and builds a client. setup-envtest must have provisioned
// the control-plane binaries and KUBEBUILDER_ASSETS must point at them — the
// controller-test make target does this, and so does the dedicated CI step.
//
// When KUBEBUILDER_ASSETS is unset the package is skipped rather than failed, so
// the repo-wide `go test ./...` (the CI Go job, which does not provision
// envtest) stays green: it cannot stand up a kube-apiserver and is not the job
// that owns these tests. The envtest-backed reconcile tests run under
// `make controller-test`, which both the developer workflow and CI invoke with
// the assets present.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// No control-plane binaries available: skip the package cleanly. Running
		// m.Run() with zero registered tests would still try TestMain's setup,
		// so return before Start(). The package reports ok with no tests run.
		os.Exit(0)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "holos-controller", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		panic("starting envtest (is KUBEBUILDER_ASSETS set? run via `make controller-test`): " + err.Error())
	}

	if err := quayv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic("adding quay scheme: " + err.Error())
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

// newReconciler builds an OrganizationReconciler wired to the envtest client and
// a recording event recorder, injecting the supplied fake Quay client via a
// factory. namespace is the controller's own namespace where credential Secrets
// are resolved.
func newReconciler(fake *fakeOrgClient, namespace string) (*OrganizationReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	r := &OrganizationReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Namespace: namespace,
		NewClient: func(cred *quayCredential, caBundle []byte) OrgClient {
			fake.gotCABundle = caBundle
			return fake
		},
	}
	return r, recorder
}

// reconcile runs a single Reconcile pass for the named Organization.
func reconcile(ctx context.Context, r *OrganizationReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// newCredentialSecret returns a credential Secret in the controller namespace
// carrying url and token (and username), ready to k8sClient.Create.
func newCredentialSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data: map[string][]byte{
			credentialKeyURL:      []byte("https://quay.example.test"),
			credentialKeyToken:    []byte("super-secret-token"),
			credentialKeyUsername: []byte("svc-quay-resource-controller"),
		},
	}
}

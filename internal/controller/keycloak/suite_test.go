package keycloak

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

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
	securityv1alpha1 "github.com/holos-run/holos-paas/api/security/v1alpha1"
)

// validTestCABundle generates a real, parseable PEM-encoded x509 certificate so
// reconciler tests can set a valid spec.caBundle that survives the controller's
// up-front keycloak.ValidateCABundle check.
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

// testEnv holds the shared envtest control plane and clients each test reuses. It
// is set up once per package run by TestMain and torn down afterward, because
// standing up kube-apiserver + etcd is expensive.
type testEnv struct {
	cfg       *rest.Config
	k8sClient client.Client
	env       *envtest.Environment
}

// shared is the package-level envtest fixture. It is nil when KUBEBUILDER_ASSETS
// is unset (TestMain skips setup in that case).
var shared *testEnv

// TestMain stands up a single envtest control plane for the whole package: it
// installs the keycloak.holos.run and security.holos.run CRDs from
// config/crd/bases, registers the scheme, and builds a client.
//
// When KUBEBUILDER_ASSETS is unset the package is skipped rather than failed, so
// the repo-wide `go test ./...` (which does not provision envtest) stays green.
// The envtest-backed reconcile tests run under `make controller-test`.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		os.Exit(0)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		panic("starting envtest (is KUBEBUILDER_ASSETS set? run via `make controller-test`): " + err.Error())
	}

	if err := keycloakv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic("adding keycloak scheme: " + err.Error())
	}
	if err := securityv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic("adding security scheme: " + err.Error())
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

// newInstanceReconciler builds an InstanceReconciler wired to the envtest client
// and a recording event recorder, injecting the supplied fake via a factory.
func newInstanceReconciler(fake *fakeKeycloakClient, namespace string) (*InstanceReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	r := &InstanceReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Namespace: namespace,
		NewClient: func(cred *keycloakCredential, url, realm string, caBundle []byte) InstanceClient {
			fake.gotCABundle = caBundle
			return fake
		},
	}
	return r, recorder
}

// newGroupReconciler builds a GroupReconciler wired to the envtest client and a
// recording event recorder, injecting the supplied fake via a factory.
func newGroupReconciler(fake *fakeKeycloakClient, namespace string) (*GroupReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	r := &GroupReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Namespace: namespace,
		NewClient: func(cred *keycloakCredential, url, realm string, caBundle []byte) GroupClient {
			fake.gotCABundle = caBundle
			return fake
		},
	}
	return r, recorder
}

// newUserReconciler builds a UserReconciler wired to the envtest client and a
// recording event recorder, injecting the supplied fake via a factory.
func newUserReconciler(fake *fakeKeycloakClient, namespace string) (*UserReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	r := &UserReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Namespace: namespace,
		NewClient: func(cred *keycloakCredential, url, realm string, caBundle []byte) UserClient {
			fake.gotCABundle = caBundle
			return fake
		},
	}
	return r, recorder
}

// newClientReconciler builds a ClientReconciler wired to the envtest client and a
// recording event recorder, injecting the supplied fake via a factory.
func newClientReconciler(fake *fakeKeycloakClient, namespace string) (*ClientReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(64)
	r := &ClientReconciler{
		Client:    shared.k8sClient,
		APIReader: shared.k8sClient,
		Recorder:  recorder,
		Namespace: namespace,
		NewClient: func(cred *keycloakCredential, url, realm string, caBundle []byte) ClientClient {
			fake.gotCABundle = caBundle
			return fake
		},
	}
	return r, recorder
}

// reconcileInstance runs a single Reconcile pass for the named KeycloakInstance.
func reconcileInstance(ctx context.Context, r *InstanceReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// reconcileGroup runs a single Reconcile pass for the named KeycloakGroup.
func reconcileGroup(ctx context.Context, r *GroupReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// reconcileUser runs a single Reconcile pass for the named KeycloakUser.
func reconcileUser(ctx context.Context, r *UserReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// reconcileClient runs a single Reconcile pass for the named KeycloakClient.
func reconcileClient(ctx context.Context, r *ClientReconciler, key client.ObjectKey) (ctrl.Result, error) {
	return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

// newCredentialSecret returns a Keycloak admin credential Secret in the controller
// namespace carrying clientId and clientSecret, ready to k8sClient.Create.
func newCredentialSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data: map[string][]byte{
			credentialKeyClientID:     []byte("holos-controller"),
			credentialKeyClientSecret: []byte("super-secret"),
		},
	}
}

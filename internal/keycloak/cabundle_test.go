package keycloak

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// selfSignedCert generates a fresh self-signed CA-style serving certificate for
// "127.0.0.1" entirely in-test (no cluster, no fixture files). It returns the
// tls.Certificate to install on an httptest TLS server and the PEM encoding of
// the certificate, which doubles as the CA bundle a client must trust to reach
// that server.
func selfSignedCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "keycloak-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return cert, certPEM
}

// newTLSTestServer starts an httptest TLS server that answers the token endpoint
// and a minimal GetGroup response, using the supplied serving certificate.
func newTLSTestServer(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			writeToken(w)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"g1","name":"owner"}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestNewClientWithCABundleTrustsSuppliedPEM(t *testing.T) {
	cert, caPEM := selfSignedCert(t)
	srv := newTLSTestServer(t, cert)

	c, err := NewClientWithCABundle(srv.URL, "holos", testCreds(), caPEM)
	if err != nil {
		t.Fatalf("NewClientWithCABundle: %v", err)
	}
	if _, err := c.GetGroup(context.Background(), "g1"); err != nil {
		t.Fatalf("GetGroup with trusted CA bundle should succeed, got %v", err)
	}
}

func TestNewClientWithCABundleRejectsUnrelatedCA(t *testing.T) {
	cert, _ := selfSignedCert(t)
	srv := newTLSTestServer(t, cert)

	// A client built trusting a *different* CA must reject the server's cert.
	_, unrelatedPEM := selfSignedCert(t)
	c, err := NewClientWithCABundle(srv.URL, "holos", testCreds(), unrelatedPEM)
	if err != nil {
		t.Fatalf("NewClientWithCABundle: %v", err)
	}
	if _, err := c.GetGroup(context.Background(), "g1"); err == nil {
		t.Fatal("expected a TLS trust error when the CA bundle does not include the server cert")
	}
}

func TestNewClientWithCABundleEmptyUsesSystemTrust(t *testing.T) {
	c, err := NewClientWithCABundle("https://kc.example.com", "holos", testCreds(), nil)
	if err != nil {
		t.Fatalf("NewClientWithCABundle empty: %v", err)
	}
	if c.httpClient.Transport != nil {
		t.Error("empty caBundle must not install a custom transport")
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("empty caBundle client Timeout = %v, want %v", c.httpClient.Timeout, defaultTimeout)
	}

	c2, err := NewClientWithCABundle("https://kc.example.com", "holos", testCreds(), []byte{})
	if err != nil {
		t.Fatalf("NewClientWithCABundle empty slice: %v", err)
	}
	if c2.httpClient.Transport != nil {
		t.Error("empty (non-nil) caBundle must not install a custom transport")
	}
}

func TestNewClientWithCABundleInvalidPEM(t *testing.T) {
	_, err := NewClientWithCABundle("https://kc.example.com", "holos", testCreds(), []byte("not a pem block"))
	if err == nil {
		t.Fatal("expected an error for a caBundle with no valid certificates")
	}
}

func TestValidateCABundle(t *testing.T) {
	_, caPEM := selfSignedCert(t)
	if err := ValidateCABundle(nil); err != nil {
		t.Errorf("empty bundle must be valid, got %v", err)
	}
	if err := ValidateCABundle([]byte{}); err != nil {
		t.Errorf("empty (non-nil) bundle must be valid, got %v", err)
	}
	if err := ValidateCABundle(caPEM); err != nil {
		t.Errorf("a real PEM cert must be valid, got %v", err)
	}
	if err := ValidateCABundle([]byte("not a pem block")); err == nil {
		t.Error("a non-empty bundle with no parseable cert must be invalid")
	}
}

func TestNewClientWithCABundlePreservesProxyAwareTransport(t *testing.T) {
	_, caPEM := selfSignedCert(t)
	c, err := NewClientWithCABundle("https://kc.example.com", "holos", testCreds(), caPEM)
	if err != nil {
		t.Fatalf("NewClientWithCABundle: %v", err)
	}
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if tr.Proxy == nil {
		t.Error("transport must inherit the default Proxy func (HTTP_PROXY/NO_PROXY honoring), got nil")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Error("transport must set TLSClientConfig.RootCAs to the trust pool")
	}
}

func TestHTTPClientForCABundleEmptyIsNil(t *testing.T) {
	hc, err := httpClientForCABundle(nil)
	if err != nil {
		t.Fatalf("httpClientForCABundle(nil): %v", err)
	}
	if hc != nil {
		t.Error("httpClientForCABundle(nil) must return a nil *http.Client so NewClient applies its default")
	}
}

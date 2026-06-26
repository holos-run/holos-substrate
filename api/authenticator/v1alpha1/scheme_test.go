package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestAddToSchemeRegistersKinds verifies that AddToScheme registers the Backend
// kind (and its List type) under the authenticator.holos.run/v1alpha1
// group-version. This is the scaffold's smoke test: it catches an unregistered
// type before any authorizer depends on the scheme.
func TestAddToSchemeRegistersKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{"Backend", "BackendList"} {
		gvk := schema.GroupVersionKind{Group: "authenticator.holos.run", Version: "v1alpha1", Kind: kind}
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}

// TestGroupVersion pins the group and version so an accidental rename is caught.
func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "authenticator.holos.run" {
		t.Errorf("group = %q, want authenticator.holos.run", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Errorf("version = %q, want v1alpha1", GroupVersion.Version)
	}
}

// TestDeepCopyRoundTrip exercises the generated DeepCopy methods on a populated
// Backend — a non-trivial smoke test that the generated code copies the nested
// spec structs and the []byte CA bundles independently.
func TestDeepCopyRoundTrip(t *testing.T) {
	backend := &Backend{
		Spec: BackendSpec{
			Host: "api.example.com",
			Server: ServerConfig{
				URL:      "https://api.example.com:6443",
				CABundle: []byte("-----BEGIN CERTIFICATE-----\nserver\n-----END CERTIFICATE-----\n"),
			},
			OIDC: OIDCConfig{
				IssuerURL:     "https://keycloak.holos.internal/realms/holos",
				ClientID:      "holos-authenticator",
				CABundle:      []byte("-----BEGIN CERTIFICATE-----\noidc\n-----END CERTIFICATE-----\n"),
				JWKS:          []byte(`{"keys":[{"kty":"RSA","kid":"k1"}]}`),
				UsernameClaim: "sub",
				GroupsClaim:   "groups",
			},
			GroupMapping:         GroupMapping{CELExpression: "claims.groups"},
			CredentialsSecretRef: &SecretReference{Name: "custom-creds", Key: "token"},
		},
	}
	clone := backend.DeepCopy()
	if clone == backend {
		t.Fatal("DeepCopy returned the same pointer")
	}
	if clone.Spec.CredentialsSecretRef == backend.Spec.CredentialsSecretRef {
		t.Error("DeepCopy did not clone the CredentialsSecretRef pointer")
	}
	if clone.Spec.CredentialsSecretRef.Name != "custom-creds" {
		t.Errorf("cloned CredentialsSecretRef.Name = %q, want custom-creds", clone.Spec.CredentialsSecretRef.Name)
	}
	if &clone.Spec.Server.CABundle == &backend.Spec.Server.CABundle {
		t.Error("DeepCopy did not clone the Server.CABundle slice")
	}
	if &clone.Spec.OIDC.CABundle == &backend.Spec.OIDC.CABundle {
		t.Error("DeepCopy did not clone the OIDC.CABundle slice")
	}
	if &clone.Spec.OIDC.JWKS == &backend.Spec.OIDC.JWKS {
		t.Error("DeepCopy did not clone the OIDC.JWKS slice")
	}
	clone.Spec.Server.CABundle[0] = 'X'
	if backend.Spec.Server.CABundle[0] == 'X' {
		t.Error("mutating the clone's Server.CABundle changed the original (shared backing array)")
	}
	clone.Spec.OIDC.CABundle[0] = 'Y'
	if backend.Spec.OIDC.CABundle[0] == 'Y' {
		t.Error("mutating the clone's OIDC.CABundle changed the original (shared backing array)")
	}
	clone.Spec.OIDC.JWKS[0] = 'Z'
	if backend.Spec.OIDC.JWKS[0] == 'Z' {
		t.Error("mutating the clone's OIDC.JWKS changed the original (shared backing array)")
	}
	if clone.Spec.OIDC.ClientID != "holos-authenticator" {
		t.Errorf("cloned OIDC.ClientID = %q, want holos-authenticator", clone.Spec.OIDC.ClientID)
	}
}

// TestDefaultCredentialsSecretName pins the documented default Secret name so the
// API doc comment, the kubebuilder default marker, and consumers stay in sync.
func TestDefaultCredentialsSecretName(t *testing.T) {
	if DefaultCredentialsSecretName != "holos-authenticator-backend-creds" {
		t.Errorf("DefaultCredentialsSecretName = %q, want holos-authenticator-backend-creds", DefaultCredentialsSecretName)
	}
}

// TestDefaultImpersonatorServiceAccountName pins the documented default SA name so
// the API doc comment, the kubebuilder default marker on
// ServiceAccountReference.Name, and consumers stay in sync.
func TestDefaultImpersonatorServiceAccountName(t *testing.T) {
	if DefaultImpersonatorServiceAccountName != "holos-authenticator-impersonator" {
		t.Errorf("DefaultImpersonatorServiceAccountName = %q, want holos-authenticator-impersonator", DefaultImpersonatorServiceAccountName)
	}
}

// TestServiceAccountReferenceDeepCopy exercises the generated DeepCopy on a
// ServiceAccountReference, asserting the *int64 ExpirationSeconds is cloned into
// an independent backing value rather than shared.
func TestServiceAccountReferenceDeepCopy(t *testing.T) {
	exp := int64(1800)
	ref := &ServiceAccountReference{Name: "impersonator", Audience: "api", ExpirationSeconds: &exp}
	clone := ref.DeepCopy()
	if clone == ref {
		t.Fatal("DeepCopy returned the same pointer")
	}
	if clone.ExpirationSeconds == ref.ExpirationSeconds {
		t.Error("DeepCopy did not clone the ExpirationSeconds pointer")
	}
	*clone.ExpirationSeconds = 600
	if *ref.ExpirationSeconds != 1800 {
		t.Error("mutating the clone's ExpirationSeconds changed the original (shared pointer)")
	}
}

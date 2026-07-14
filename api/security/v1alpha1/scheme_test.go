package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestAddToSchemeRegistersKinds verifies that AddToScheme registers the
// ReferenceGrant kind (and its List type) under the security.holos.run/v1alpha1
// group-version. It catches an unregistered type before any consumer depends on
// the scheme.
func TestAddToSchemeRegistersKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{"ReferenceGrant", "ReferenceGrantList"} {
		gvk := schema.GroupVersionKind{Group: "security.holos.run", Version: "v1alpha1", Kind: kind}
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}

// TestGroupVersion pins the group and version so an accidental rename is caught.
func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "security.holos.run" {
		t.Errorf("group = %q, want security.holos.run", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Errorf("version = %q, want v1alpha1", GroupVersion.Version)
	}
}

// TestDeepCopyRoundTrip exercises the generated DeepCopy methods on a populated
// ReferenceGrant, confirming the generated code clones the From/To slices and
// the optional To.Name pointer independently.
func TestDeepCopyRoundTrip(t *testing.T) {
	name := "kc-creds"
	rg := &ReferenceGrant{
		Spec: ReferenceGrantSpec{
			From: []ReferenceGrantFrom{{Group: "keycloak.holos.run", Kind: "Instance", Namespace: "team-a"}},
			To:   []ReferenceGrantTo{{Group: "", Kind: "Secret", Name: &name}},
		},
	}
	clone := rg.DeepCopy()
	if &clone.Spec.From[0] == &rg.Spec.From[0] {
		t.Error("DeepCopy did not clone the From slice")
	}
	if &clone.Spec.To[0] == &rg.Spec.To[0] {
		t.Error("DeepCopy did not clone the To slice")
	}
	if clone.Spec.To[0].Name == rg.Spec.To[0].Name {
		t.Error("DeepCopy did not clone the To.Name pointer")
	}
	if *clone.Spec.To[0].Name != name {
		t.Errorf("cloned To.Name = %q, want %q", *clone.Spec.To[0].Name, name)
	}
}

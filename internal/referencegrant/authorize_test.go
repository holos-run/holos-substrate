package referencegrant

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/holos-run/holos-substrate/api/security/v1alpha1"
)

// testScheme builds a scheme with the security.holos.run types registered so the
// fake client can serve ReferenceGrant lists.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := securityv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// grant is a small constructor for a ReferenceGrant with a single From/To pair.
func grant(name, ns string, from securityv1alpha1.ReferenceGrantFrom, to securityv1alpha1.ReferenceGrantTo) *securityv1alpha1.ReferenceGrant {
	return &securityv1alpha1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: securityv1alpha1.ReferenceGrantSpec{
			From: []securityv1alpha1.ReferenceGrantFrom{from},
			To:   []securityv1alpha1.ReferenceGrantTo{to},
		},
	}
}

func ptr(s string) *string { return &s }

// The referrer and target used across the truth-table cases.
var (
	defaultFrom = FromRef{Group: "keycloak.holos.run", Kind: "KeycloakInstance", Namespace: "team-a"}
	defaultTo   = ToRef{Group: "", Kind: "Secret", Namespace: "team-b", Name: "kc-creds"}
)

func TestAllowed(t *testing.T) {
	matchingFrom := securityv1alpha1.ReferenceGrantFrom{
		Group: "keycloak.holos.run", Kind: "KeycloakInstance", Namespace: "team-a",
	}
	unconstrainedTo := securityv1alpha1.ReferenceGrantTo{Group: "", Kind: "Secret"}

	tests := []struct {
		name   string
		grants []*securityv1alpha1.ReferenceGrant
		from   FromRef
		to     ToRef
		want   bool
	}{
		{
			name:   "matching grant (name-unconstrained) allows",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b", matchingFrom, unconstrainedTo)},
			from:   defaultFrom,
			to:     defaultTo,
			want:   true,
		},
		{
			name:   "no grant denies",
			grants: nil,
			from:   defaultFrom,
			to:     defaultTo,
			want:   false,
		},
		{
			name: "wrong from-namespace denies",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b",
				securityv1alpha1.ReferenceGrantFrom{Group: "keycloak.holos.run", Kind: "KeycloakInstance", Namespace: "other"},
				unconstrainedTo)},
			from: defaultFrom,
			to:   defaultTo,
			want: false,
		},
		{
			name: "wrong to-kind denies",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b", matchingFrom,
				securityv1alpha1.ReferenceGrantTo{Group: "", Kind: "ConfigMap"})},
			from: defaultFrom,
			to:   defaultTo,
			want: false,
		},
		{
			name: "wrong from-kind denies",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b",
				securityv1alpha1.ReferenceGrantFrom{Group: "keycloak.holos.run", Kind: "OtherKind", Namespace: "team-a"},
				unconstrainedTo)},
			from: defaultFrom,
			to:   defaultTo,
			want: false,
		},
		{
			name: "name-constrained grant hit allows",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b", matchingFrom,
				securityv1alpha1.ReferenceGrantTo{Group: "", Kind: "Secret", Name: ptr("kc-creds")})},
			from: defaultFrom,
			to:   defaultTo,
			want: true,
		},
		{
			name: "name-constrained grant miss denies",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b", matchingFrom,
				securityv1alpha1.ReferenceGrantTo{Group: "", Kind: "Secret", Name: ptr("other-secret")})},
			from: defaultFrom,
			to:   defaultTo,
			want: false,
		},
		{
			name:   "grant in a different namespace is not consulted",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-c", matchingFrom, unconstrainedTo)},
			from:   defaultFrom,
			to:     defaultTo,
			want:   false,
		},
		{
			name:   "empty target namespace fails closed (no cluster-wide list)",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b", matchingFrom, unconstrainedTo)},
			from:   defaultFrom,
			to:     ToRef{Group: "", Kind: "Secret", Namespace: "", Name: "kc-creds"},
			want:   false,
		},
		{
			name:   "empty referrer namespace fails closed",
			grants: []*securityv1alpha1.ReferenceGrant{grant("g", "team-b", matchingFrom, unconstrainedTo)},
			from:   FromRef{Group: "keycloak.holos.run", Kind: "KeycloakInstance", Namespace: ""},
			to:     defaultTo,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tt.grants))
			for _, g := range tt.grants {
				objs = append(objs, g)
			}
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(objs...).
				Build()

			got, err := Allowed(context.Background(), c, tt.from, tt.to)
			if err != nil {
				t.Fatalf("Allowed: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAllowedMatchAcrossSeparateEntries verifies that a From in one grant and a
// To in another do not combine: a single grant must contain both a matching
// From and a matching To. Here grant g1 matches only From and g2 matches only
// To, so the reference is denied.
func TestAllowedMatchAcrossSeparateEntries(t *testing.T) {
	g1 := grant("g1", "team-b",
		securityv1alpha1.ReferenceGrantFrom{Group: "keycloak.holos.run", Kind: "KeycloakInstance", Namespace: "team-a"},
		securityv1alpha1.ReferenceGrantTo{Group: "", Kind: "ConfigMap"})
	g2 := grant("g2", "team-b",
		securityv1alpha1.ReferenceGrantFrom{Group: "keycloak.holos.run", Kind: "KeycloakInstance", Namespace: "other"},
		securityv1alpha1.ReferenceGrantTo{Group: "", Kind: "Secret"})

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(g1, g2).
		Build()

	got, err := Allowed(context.Background(), c, defaultFrom, defaultTo)
	if err != nil {
		t.Fatalf("Allowed: unexpected error: %v", err)
	}
	if got {
		t.Error("Allowed = true, want false: From and To from separate grants must not combine")
	}
}

// errReader is a client.Reader whose List always fails, exercising the error
// propagation path of Allowed.
type errReader struct{ client.Reader }

func (errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("boom")
}

func TestAllowedPropagatesListError(t *testing.T) {
	_, err := Allowed(context.Background(), errReader{}, defaultFrom, defaultTo)
	if err == nil {
		t.Fatal("Allowed: expected an error from the failing List, got nil")
	}
}

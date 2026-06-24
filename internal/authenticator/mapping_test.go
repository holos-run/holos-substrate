package authenticator

import (
	"reflect"
	"testing"
)

// TestDefaultGroupExpression checks the default expression construction for the
// conventional and an empty groups claim.
func TestDefaultGroupExpression(t *testing.T) {
	if got, want := DefaultGroupExpression("groups"), `claims["groups"]`; got != want {
		t.Errorf("DefaultGroupExpression(groups) = %q, want %q", got, want)
	}
	if got, want := DefaultGroupExpression(""), `claims["groups"]`; got != want {
		t.Errorf("DefaultGroupExpression(\"\") = %q, want %q", got, want)
	}
	if got, want := DefaultGroupExpression("roles"), `claims["roles"]`; got != want {
		t.Errorf("DefaultGroupExpression(roles) = %q, want %q", got, want)
	}
}

// TestGroupMapperGroups exercises compile + evaluate for the default mapping, a
// custom expression, and missing-claim handling.
func TestGroupMapperGroups(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		claims map[string]any
		want   []string
	}{
		{
			name:   "default groups claim present",
			expr:   DefaultGroupExpression("groups"),
			claims: map[string]any{"groups": []any{"dev", "ops"}},
			want:   []string{"dev", "ops"},
		},
		{
			name:   "default groups claim missing yields no groups",
			expr:   DefaultGroupExpression("groups"),
			claims: map[string]any{"sub": "alice"},
			want:   nil,
		},
		{
			name:   "custom groups claim name",
			expr:   DefaultGroupExpression("roles"),
			claims: map[string]any{"roles": []any{"admin"}},
			want:   []string{"admin"},
		},
		{
			name:   "custom expression prefixing groups",
			expr:   `claims.groups.map(g, "oidc:" + g)`,
			claims: map[string]any{"groups": []any{"dev", "ops"}},
			want:   []string{"oidc:dev", "oidc:ops"},
		},
		{
			name:   "custom expression with literal list",
			expr:   `["everyone"]`,
			claims: map[string]any{},
			want:   []string{"everyone"},
		},
		{
			name:   "custom expression indexing missing claim yields no groups",
			expr:   `claims.teams`,
			claims: map[string]any{"groups": []any{"dev"}},
			want:   nil,
		},
		{
			// The SA-virtual-groups expression documented for KSA / static-JWKS
			// backends (ADR-23 Rev 3, the runbook's "KSA / static-JWKS backends"
			// section, and the holos-authenticator component's remote-cluster-a
			// example Backend). A projected service-account ID token carries a
			// nested kubernetes.io claim whose namespace field reproduces the SA's
			// three Kubernetes virtual groups. This case proves the documented
			// expression compiles and evaluates so the docs cannot ship an
			// expression that does not run.
			name: "KSA SA-virtual-groups expression",
			expr: `["system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + claims["kubernetes.io"].namespace]`,
			claims: map[string]any{
				"sub":           "system:serviceaccount:remote-ns:remote-sa",
				"kubernetes.io": map[string]any{"namespace": "remote-ns"},
			},
			want: []string{
				"system:authenticated",
				"system:serviceaccounts",
				"system:serviceaccounts:remote-ns",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewGroupMapper(tc.expr)
			if err != nil {
				t.Fatalf("NewGroupMapper(%q): %v", tc.expr, err)
			}
			got, err := m.Groups(tc.claims)
			if err != nil {
				t.Fatalf("Groups(%v): %v", tc.claims, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Groups = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewGroupMapperRejectsBadExpressions asserts compile-time rejection of a
// syntactically invalid expression, an expression referencing an unknown
// variable, and an expression whose result type is not a list.
func TestNewGroupMapperRejectsBadExpressions(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{name: "empty expression", expr: ""},
		{name: "syntax error", expr: `claims.groups[`},
		{name: "unknown variable", expr: `token.groups`},
		{name: "non-list result (string literal)", expr: `"a-group"`},
		{name: "non-list result (int literal)", expr: `42`},
		{name: "non-list result (bool comparison)", expr: `claims.sub == "x"`},
		{name: "non-list result (map)", expr: `claims`},
		{name: "concrete list(int)", expr: `[1]`},
		{name: "concrete list(bool)", expr: `[true]`},
		{name: "concrete list(int) multi", expr: `[1, 2, 3]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewGroupMapper(tc.expr); err == nil {
				t.Fatalf("NewGroupMapper(%q) = nil error, want a compile error", tc.expr)
			}
		})
	}
}

// TestGroupMapperRejectsNonListAtRuntime asserts that a dyn-typed expression
// (accepted at compile time) whose runtime value is not a list is reported as an
// evaluation error rather than silently coerced. The default groups mapping over
// a token whose groups claim is a bare string is the realistic case.
func TestGroupMapperRejectsNonListAtRuntime(t *testing.T) {
	m, err := NewGroupMapper(DefaultGroupExpression("groups"))
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	if _, err := m.Groups(map[string]any{"groups": "not-a-list"}); err == nil {
		t.Fatalf("Groups over a non-list groups claim = nil error, want an evaluation error")
	}
}

// TestGroupMapperRejectsNonStringElements confirms a list containing a non-string
// element is rejected (not coerced): the API contract is list(string), so a
// numeric or boolean claim value must not silently become a Kubernetes group name.
func TestGroupMapperRejectsNonStringElements(t *testing.T) {
	m, err := NewGroupMapper(`claims.groups`)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	if _, err := m.Groups(map[string]any{"groups": []any{"a", int64(7), true}}); err == nil {
		t.Fatalf("Groups over a list with non-string elements = nil error, want rejection")
	}
}

// TestGroupMapperAllStringElements confirms an all-string list passes through.
func TestGroupMapperAllStringElements(t *testing.T) {
	m, err := NewGroupMapper(`claims.groups`)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	got, err := m.Groups(map[string]any{"groups": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Groups = %v, want %v", got, want)
	}
}

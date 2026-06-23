package authenticator

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// claimsVar is the name of the CEL input variable carrying the validated token
// claims. A group-mapping expression references it as e.g. `claims.groups`.
const claimsVar = "claims"

// GroupMapper compiles and evaluates a per-Backend group-mapping CEL expression.
// The expression takes a single input variable `claims` (a map(string, dyn) of
// the validated token claims) and returns the list(string) of Kubernetes groups
// to impersonate.
//
// The zero value is not usable; construct one with NewGroupMapper, which both
// compiles the expression (so a malformed expression is rejected at reconcile
// time, surfacing as Accepted=False) and verifies the result type is
// list(string). The compiled program is cached and safe for concurrent
// evaluation.
type GroupMapper struct {
	// expression is the source CEL text, retained for diagnostics.
	expression string
	// program is the compiled, type-checked CEL program. Evaluation is
	// concurrency-safe.
	program cel.Program
}

// newCELEnv builds the CEL environment shared by compile and (implicitly)
// evaluate: a single `claims` variable of type map(string, dyn). Declaring the
// value type as dyn lets an expression index into arbitrarily-shaped claims
// (nested maps, lists, scalars) while still type-checking the overall result.
func newCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable(claimsVar, cel.MapType(cel.StringType, cel.DynType)),
	)
}

// NewGroupMapper compiles expr into a GroupMapper. The expression must type-check
// to list(string); any other result type (e.g. a bare string or an int) is
// rejected so a misconfigured mapping fails at reconcile time rather than
// producing nonsense groups at request time.
//
// An empty expr is a programming error here: callers default the expression to
// the groups-claim mapping (DefaultGroupExpression) before calling, so that the
// "no/empty CEL" default behavior is itself a compiled CEL program and shares the
// same evaluation path. Pass DefaultGroupExpression(groupsClaim) for the default.
func NewGroupMapper(expr string) (*GroupMapper, error) {
	if expr == "" {
		return nil, fmt.Errorf("group-mapping CEL expression is empty; default it before compiling")
	}

	env, err := newCELEnv()
	if err != nil {
		return nil, fmt.Errorf("building CEL environment: %w", err)
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compiling group-mapping CEL expression %q: %w", expr, issues.Err())
	}

	// Require the expression to plausibly evaluate to list(string). CEL types a
	// map index (the default `claims["groups"]`) and field selection as `dyn`,
	// deferring the concrete type to runtime, so dyn must be accepted — its
	// list-ness is enforced at evaluation by refValToStringSlice. A concrete
	// list(...) is accepted (elements are coerced to string). Reject only types
	// that can never be a string list: concrete scalars (string/int/bool/...) and
	// maps, so a plainly-wrong expression like `claims.sub == "x"` or `claims`
	// fails at reconcile time rather than producing nonsense at request time.
	if !isStringListType(ast.OutputType()) {
		return nil, fmt.Errorf(
			"group-mapping CEL expression %q must return list(string), got %s",
			expr, ast.OutputType(),
		)
	}

	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("building CEL program for %q: %w", expr, err)
	}

	return &GroupMapper{expression: expr, program: program}, nil
}

// Expression returns the source CEL text the GroupMapper was compiled from.
func (m *GroupMapper) Expression() string { return m.expression }

// Groups evaluates the compiled expression over claims and returns the resolved
// Kubernetes groups. A claim the expression references that is absent yields an
// empty (non-nil-or-nil) group slice rather than an error: the default
// `claims.groups` mapping over a token with no groups claim returns no groups,
// which is the desired "user is in no extra groups" behavior, not a failure.
//
// claims is the validated token's claim set as a map[string]any (the shape
// go-oidc's IDToken.Claims unmarshals into).
func (m *GroupMapper) Groups(claims map[string]any) ([]string, error) {
	out, _, err := m.program.Eval(map[string]any{claimsVar: claims})
	if err != nil {
		// A no-such-key/no-such-field error is the expected "claim absent" case
		// for the default groups mapping and for custom expressions that index a
		// missing claim; treat it as "no groups" rather than a hard failure.
		if isMissingKeyErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("evaluating group-mapping expression %q: %w", m.expression, err)
	}
	return refValToStringSlice(out)
}

// DefaultGroupExpression returns the CEL expression implementing the default
// group mapping: read the named groups claim directly. With the conventional
// groupsClaim of "groups" it is `claims.groups`. Routing the default through a
// compiled expression (rather than a special-case Go branch) means the default
// and custom paths share one evaluator and one set of semantics (missing-claim →
// empty groups).
func DefaultGroupExpression(groupsClaim string) string {
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	// Index syntax (claims["groups"]) rather than field syntax (claims.groups)
	// so a claim name that is not a valid CEL identifier still works.
	return fmt.Sprintf("%s[%q]", claimsVar, groupsClaim)
}

// isStringListType reports whether t is acceptable as a group-mapping result:
// any concrete list(T) (elements are coerced to string at evaluation) or dyn (a
// map index / field selection whose concrete type CEL defers to runtime, where
// refValToStringSlice enforces list-ness). Concrete non-list types (scalars,
// maps) are rejected at compile time.
func isStringListType(t *cel.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind() {
	case types.ListKind, types.DynKind:
		return true
	default:
		return false
	}
}

// refValToStringSlice converts a CEL evaluation result (expected to be a list)
// into a []string, coercing each element to its string form. A non-list result
// is a programming error (the compile-time type check should have rejected it)
// and returns an error defensively.
func refValToStringSlice(val ref.Val) ([]string, error) {
	lister, ok := val.Value().([]ref.Val)
	if !ok {
		// Fall back to the traits.Lister interface for list implementations that
		// do not expose a []ref.Val directly.
		return listerToStringSlice(val)
	}
	groups := make([]string, 0, len(lister))
	for _, el := range lister {
		s, err := elementToString(el)
		if err != nil {
			return nil, err
		}
		groups = append(groups, s)
	}
	return groups, nil
}

// listerToStringSlice handles CEL list values whose backing is a traits.Lister
// rather than a plain []ref.Val.
func listerToStringSlice(val ref.Val) ([]string, error) {
	lister, ok := val.(traits.Lister)
	if !ok {
		return nil, fmt.Errorf("group-mapping result is not a list: %T", val.Value())
	}
	size, ok := lister.Size().Value().(int64)
	if !ok {
		return nil, fmt.Errorf("group-mapping list has a non-integer size")
	}
	groups := make([]string, 0, size)
	for i := int64(0); i < size; i++ {
		s, err := elementToString(lister.Get(types.Int(i)))
		if err != nil {
			return nil, err
		}
		groups = append(groups, s)
	}
	return groups, nil
}

// elementToString returns a single CEL list element as a Go string, requiring it
// to actually be a string. The API contract is that the expression returns
// list(string); a non-string element (a number, a bool, a nested object) is a
// malformed result and is rejected rather than coerced — coercing would turn a
// claim like 7 or true into a real Kubernetes group name ("7"/"true"), silently
// granting access the operator never expressed.
func elementToString(el ref.Val) (string, error) {
	if el == nil {
		return "", fmt.Errorf("group-mapping list element is nil")
	}
	s, ok := el.Value().(string)
	if !ok {
		return "", fmt.Errorf("group-mapping list element is not a string (got %T); the expression must return list(string)", el.Value())
	}
	return s, nil
}

// isMissingKeyErr reports whether err is CEL's "no such key" / "no such field"
// runtime error, which the default and custom mappings treat as "claim absent →
// no groups" rather than a hard failure.
func isMissingKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such key") ||
		strings.Contains(msg, "no such field") ||
		strings.Contains(msg, "no such attribute")
}

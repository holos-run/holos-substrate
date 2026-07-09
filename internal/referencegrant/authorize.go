// Package referencegrant provides the cross-namespace authorization helper that
// consuming reconcilers call to decide whether an object reference that crosses
// a namespace boundary is permitted by a security.holos.run ReferenceGrant
// (ADR-22).
//
// Like upstream Gateway API, ReferenceGrant is statusless declarative policy
// with no dedicated reconciler: the grant lives in the *referent* (target)
// namespace and names the (group, kind, namespace) of referrers it trusts and
// the (group, kind, optional name) of targets they may reach. Allowed lists the
// grants in the target namespace and returns true iff some grant pairs a From
// matching the referrer with a To matching the target. Absent a matching grant,
// the reference is denied.
//
// The package is deliberately dependency-light: it imports only
// controller-runtime's client.Reader and the security/v1alpha1 types, so any
// reconciler can consume it without pulling in unrelated API groups.
package referencegrant

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/holos-run/holos-substrate/api/security/v1alpha1"
)

// FromRef identifies the referrer: the object that holds the cross-namespace
// reference. A reference is authorized only when a grant in the target namespace
// trusts this exact (Group, Kind, Namespace) triple. The empty Group string
// matches the Kubernetes core API group.
type FromRef struct {
	// Group is the API group of the referrer ("" for the core group).
	Group string
	// Kind is the kind of the referrer.
	Kind string
	// Namespace is the namespace the referrer lives in.
	Namespace string
}

// ToRef identifies the target: the object being referenced. A reference is
// authorized only when a grant in Namespace permits this (Group, Kind) and,
// when the grant constrains a name, that name equals Name. The empty Group
// string matches the Kubernetes core API group.
type ToRef struct {
	// Group is the API group of the target ("" for the core group).
	Group string
	// Kind is the kind of the target.
	Kind string
	// Namespace is the namespace the target lives in — the namespace whose
	// ReferenceGrants are consulted.
	Namespace string
	// Name is the name of the target object. A grant whose To entry sets a name
	// authorizes only this name; a grant whose To entry omits the name
	// authorizes every object of the group/kind.
	Name string
}

// Allowed reports whether referrer from may reference target to, per the
// security.holos.run ReferenceGrants in the target's namespace. It lists the
// grants in to.Namespace and returns true iff some grant has a From entry
// matching from (group, kind, and namespace) and a To entry matching to (group,
// kind, and — when the To entry constrains a name — name). With no matching
// grant the reference is not allowed; this is fail-closed policy.
//
// The list is read through the supplied client.Reader, so the caller controls
// whether it is served from a cache or a live read.
//
// Both namespaces must be non-empty: an empty to.Namespace would make
// client.InNamespace("") list ReferenceGrants cluster-wide (a grant in any
// namespace could then authorize the reference), and an empty from.Namespace
// could only match a malformed grant. Allowed fails closed (returns false, nil)
// in either case rather than performing an unscoped list.
func Allowed(ctx context.Context, c client.Reader, from FromRef, to ToRef) (bool, error) {
	if from.Namespace == "" || to.Namespace == "" {
		return false, nil
	}

	var grants securityv1alpha1.ReferenceGrantList
	if err := c.List(ctx, &grants, client.InNamespace(to.Namespace)); err != nil {
		return false, err
	}

	for i := range grants.Items {
		grant := &grants.Items[i]
		if grantMatches(grant, from, to) {
			return true, nil
		}
	}
	return false, nil
}

// grantMatches reports whether a single ReferenceGrant authorizes the from→to
// reference: it must contain both a From entry matching the referrer and a To
// entry matching the target. A grant pairs its From and To lists, so any
// matching From combined with any matching To authorizes the reference.
func grantMatches(grant *securityv1alpha1.ReferenceGrant, from FromRef, to ToRef) bool {
	return fromMatches(grant.Spec.From, from) && toMatches(grant.Spec.To, to)
}

// fromMatches reports whether any From entry trusts the referrer: group, kind,
// and namespace must all match exactly.
func fromMatches(entries []securityv1alpha1.ReferenceGrantFrom, from FromRef) bool {
	for _, e := range entries {
		if e.Group == from.Group && e.Kind == from.Kind && e.Namespace == from.Namespace {
			return true
		}
	}
	return false
}

// toMatches reports whether any To entry permits the target: group and kind
// must match exactly, and — when the entry constrains a name — the name must
// match too. A To entry with no name (nil) permits every object of the
// group/kind.
func toMatches(entries []securityv1alpha1.ReferenceGrantTo, to ToRef) bool {
	for _, e := range entries {
		if e.Group != to.Group || e.Kind != to.Kind {
			continue
		}
		if e.Name != nil && *e.Name != to.Name {
			continue
		}
		return true
	}
	return false
}

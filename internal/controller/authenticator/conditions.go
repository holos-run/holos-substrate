// Package authenticator holds the controller-runtime reconciler for the
// authenticator.holos.run API group (ADR-23): the Backend reconciler that
// validates a backend's OIDC client and upstream API server configuration and
// reports rich status. The reconciler itself lands in a later phase (HOL-1387);
// this file ships the shared status vocabulary ahead of it.
//
// This file is the home of the Gateway-API-style condition types and reasons,
// and the small helpers the reconciler uses to set them with
// apimachinery/pkg/api/meta.SetStatusCondition. Keeping the vocabulary in one
// place mirrors internal/controller/quay/conditions.go so a future Kind in this
// group reuses exactly the same condition types and reasons.
package authenticator

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types surfaced on authenticator.holos.run resource status. They
// mirror the vocabulary declared on the API types (ConditionAccepted /
// ConditionProgrammed / ConditionReady) and follow the Gateway API convention:
//
//   - Accepted   — the spec was understood and claimed by this resource.
//   - Programmed — the desired state was configured (the OIDC client and
//     upstream API server are discoverable and reachable).
//   - Ready      — the backend is fully configured and usable.
//
// Reusing the same string values the API package defines keeps the printer
// columns (which match on type=="Ready") and any client tooling consistent.
const (
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the desired state has been programmed —
	// the backend's OIDC client and upstream API server are configured and
	// discoverable (Gateway-API Programmed).
	ConditionProgrammed = "Programmed"
	// ConditionReady reports whether the backend is fully configured and usable
	// (Gateway-API Ready).
	ConditionReady = "Ready"
)

// Condition reasons. Reasons are stable, CamelCase machine-readable tokens
// (metav1.Condition requires this) describing why a condition holds its current
// status. They are documented here so the reconciler draws from one vocabulary.
const (
	// ReasonReconciled marks a Backend as in steady state — its OIDC client and
	// upstream API server configuration are valid and discoverable.
	ReasonReconciled = "Reconciled"
	// ReasonInvalidSpec marks a condition False because the Backend spec is
	// invalid (e.g. a malformed CEL expression or an unparseable server URL), a
	// defense-in-depth check behind the CRD validation.
	ReasonInvalidSpec = "InvalidSpec"
	// ReasonCredentialsNotFound marks a condition False because the credential
	// Secret (or a required key within it) could not be resolved.
	ReasonCredentialsNotFound = "CredentialsNotFound"
	// ReasonDiscoveryFailed marks a condition False because the OIDC issuer
	// discovery or upstream API server could not be reached or validated.
	ReasonDiscoveryFailed = "DiscoveryFailed"
)

// setCondition sets a single condition on the supplied condition slice using
// apimachinery's meta.SetStatusCondition, stamping observedGeneration so callers
// can tell which generation the condition reflects. It returns true when the
// condition changed (status, reason, message, or observedGeneration), matching
// SetStatusCondition's own changed semantics, so a reconciler can skip a
// redundant status write.
func setCondition(conditions *[]metav1.Condition, condType, status, reason, message string, observedGeneration int64) bool {
	return meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionStatus(status),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}

// markReady sets Accepted, Programmed, and Ready all True with the given reason
// and message and the observed generation. It is the success path the reconciler
// calls once the backend configuration is validated. It returns true when any of
// the three conditions changed, so callers can skip a redundant status write and
// event (avoiding a self-triggered reconcile loop).
func markReady(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	a := setCondition(conditions, ConditionAccepted, string(metav1.ConditionTrue), reason, message, observedGeneration)
	p := setCondition(conditions, ConditionProgrammed, string(metav1.ConditionTrue), reason, message, observedGeneration)
	r := setCondition(conditions, ConditionReady, string(metav1.ConditionTrue), reason, message, observedGeneration)
	return a || p || r
}

// markNotReady sets Programmed and Ready False with the given reason and message
// and the observed generation, leaving Accepted untouched (the spec was still
// understood and accepted; it just could not be programmed). It is the failure
// path for a transient operational error — a credential that has not been created
// yet, or an issuer/upstream that is temporarily unreachable — where the spec
// itself is valid. For an invalid spec the resource was never accepted; use
// markRejected instead so Accepted is also driven False. It returns true when
// either condition changed.
func markNotReady(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	p := setCondition(conditions, ConditionProgrammed, string(metav1.ConditionFalse), reason, message, observedGeneration)
	r := setCondition(conditions, ConditionReady, string(metav1.ConditionFalse), reason, message, observedGeneration)
	return p || r
}

// markRejected sets Accepted, Programmed, and Ready all False with the given
// reason and message and the observed generation. It is the rejection path for an
// invalid spec (e.g. ReasonInvalidSpec): the spec could not be understood or
// claimed, so — unlike markNotReady — Accepted is driven False too rather than
// left lingering True from a previously valid generation. It returns true when
// any of the three conditions changed.
func markRejected(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	a := setCondition(conditions, ConditionAccepted, string(metav1.ConditionFalse), reason, message, observedGeneration)
	p := setCondition(conditions, ConditionProgrammed, string(metav1.ConditionFalse), reason, message, observedGeneration)
	r := setCondition(conditions, ConditionReady, string(metav1.ConditionFalse), reason, message, observedGeneration)
	return a || p || r
}

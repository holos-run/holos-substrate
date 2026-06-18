// Package quay holds the controller-runtime reconcilers for the quay.holos.run
// API group (ADR-19): the Organization reconciler (HOL-1311) and, in a later
// phase, the Repository reconciler (HOL-1312). The reconcilers drive the
// in-cluster Quay registry through the internal/quay REST client, authenticating
// with the superuser OAuth-Application credential named by a resource's
// credentialsSecretRef.
//
// This file is the home of AC #2's status convention: the Gateway-API-style
// condition types and reasons, and the small helpers both reconcilers use to set
// them with apimachinery/pkg/api/meta.SetStatusCondition. Keeping the vocabulary
// in one place means the Repository reconciler reuses exactly the same condition
// types and reasons the Organization reconciler establishes here.
package quay

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types surfaced on quay.holos.run resource status. They mirror the
// vocabulary already declared on the API types (ConditionAccepted /
// ConditionProgrammed / ConditionReady) and follow the Gateway API convention:
//
//   - Accepted   — the spec was understood and claimed by this resource.
//   - Programmed — the desired state was written into Quay.
//   - Ready      — the resource is fully provisioned and usable.
//
// Reusing the same string values the API package defines keeps the printer
// columns (which match on type=="Ready") and any client tooling consistent.
const (
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = "Accepted"
	// ConditionProgrammed reports whether the desired state has been programmed
	// into Quay (Gateway-API Programmed).
	ConditionProgrammed = "Programmed"
	// ConditionReady reports whether the resource has been fully provisioned in
	// Quay (Gateway-API Ready).
	ConditionReady = "Ready"
)

// Condition reasons. Reasons are stable, CamelCase machine-readable tokens
// (metav1.Condition requires this) describing why a condition holds its current
// status. They are documented here so both reconcilers draw from one vocabulary.
const (
	// ReasonCreated marks the resource as newly created in Quay.
	ReasonCreated = "Created"
	// ReasonAdopted marks a pre-existing Quay object adopted by this resource.
	ReasonAdopted = "Adopted"
	// ReasonConflict marks a condition False because a pre-existing,
	// externally-created Quay org of the same name exists and the resource did
	// not opt in to adoption (spec.adopt). The org is never silently seized
	// (ADR-19 claim model).
	ReasonConflict = "Conflict"
	// ReasonReleased marks an adopted Quay org released (finalizer dropped
	// without deleting) on CR removal — adoption is non-destructive.
	ReasonReleased = "Released"
	// ReasonCredentialsNotFound marks a condition False because the credential
	// Secret (or a required key within it) could not be resolved.
	ReasonCredentialsNotFound = "CredentialsNotFound"
	// ReasonQuayError marks a condition False because a Quay API call failed.
	ReasonQuayError = "QuayError"
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
// and message and the observed generation. It is the success path both
// reconcilers call once Quay reflects the desired state. It returns true when any
// of the three conditions changed, so callers can skip a redundant status write
// and event (avoiding a self-triggered reconcile loop).
func markReady(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	a := setCondition(conditions, ConditionAccepted, string(metav1.ConditionTrue), reason, message, observedGeneration)
	p := setCondition(conditions, ConditionProgrammed, string(metav1.ConditionTrue), reason, message, observedGeneration)
	r := setCondition(conditions, ConditionReady, string(metav1.ConditionTrue), reason, message, observedGeneration)
	return a || p || r
}

// markNotReady sets Programmed and Ready False with the given reason and message
// and the observed generation, leaving Accepted untouched (the spec was still
// understood; it just could not be programmed). It is the failure path for a
// credential or Quay error. It returns true when either condition changed.
func markNotReady(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	p := setCondition(conditions, ConditionProgrammed, string(metav1.ConditionFalse), reason, message, observedGeneration)
	r := setCondition(conditions, ConditionReady, string(metav1.ConditionFalse), reason, message, observedGeneration)
	return p || r
}

// setConflict marks Programmed and Ready False with reason Conflict. It returns
// true when either condition changed.
func setConflict(conditions *[]metav1.Condition, message string, observedGeneration int64) bool {
	return markNotReady(conditions, ReasonConflict, message, observedGeneration)
}

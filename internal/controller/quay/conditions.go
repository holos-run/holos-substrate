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
	"time"

	quayv1alpha1 "github.com/holos-run/holos-paas/api/quay/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// requeueImmediately is a negligible RequeueAfter delay used to re-enqueue a
// reconcile right after a write that bumps the resourceVersion (e.g. adding a
// finalizer), so the next pass operates on the fresh object. It replaces the
// deprecated ctrl.Result.Requeue=true (staticcheck SA1019): any positive
// RequeueAfter requeues, and a millisecond is effectively immediate while
// keeping the result non-zero so test helpers can detect the requeue.
const requeueImmediately = time.Millisecond

// quayExternalResourceResync is the steady-state validation cadence for
// Quay-backed external-resource CRs. A Ready=True object still rechecks Quay
// periodically so lastValidatedTime remains actionable and out-of-band drift is
// eventually remediated even without a spec change.
const quayExternalResourceResync = time.Hour

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
	// ConditionAccepted reports whether the spec was accepted for reconciliation.
	ConditionAccepted = quayv1alpha1.ConditionAccepted
	// ConditionProgrammed reports whether the desired state has been written into
	// Quay.
	ConditionProgrammed = quayv1alpha1.ConditionProgrammed
	// ConditionReady reports whether the resource is provisioned and usable.
	ConditionReady = quayv1alpha1.ConditionReady
	// ConditionWebhookConfigured reports whether the Repository's repo_push
	// webhook notification reflects the desired target URL. It is a
	// Repository-only condition surfaced distinctly from Ready so an operator can
	// tell a provisioned-but-webhookless repository (e.g. its urlSecretRef Secret
	// has not been created yet) from a fully-wired one (AC #5/#8).
	ConditionWebhookConfigured = quayv1alpha1.ConditionWebhookConfigured
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
	// externally-created Quay object of the same name exists and the resource did
	// not opt in to adoption (spec.adopt), or because an ownership marker belongs
	// to another resource. The object is never silently seized.
	ReasonConflict = "Conflict"
	// ReasonReleased marks an adopted Quay object released (finalizer dropped
	// without deleting) on CR removal — adoption is non-destructive.
	ReasonReleased = "Released"
	// ReasonCredentialsNotFound marks a condition False because the credential
	// Secret (or a required key within it) could not be resolved.
	ReasonCredentialsNotFound = "CredentialsNotFound"
	// ReasonQuayError marks a condition False because a Quay API call failed.
	ReasonQuayError = "QuayError"
	// ReasonReconciled marks a Repository as in steady state — its Quay repository
	// (and webhook, if configured) reflect the spec.
	ReasonReconciled = "Reconciled"
	// ReasonTeamConflict marks an Organization's Programmed/Ready conditions False
	// because a spec.syncedTeams entry names a Quay team that already exists but was
	// not created by this resource: it is absent from status.managedTeams and its
	// description does not carry this CR's managedTeamMarker (the unforgeable,
	// UID-bearing ownership marker). The team is never silently seized — adoption of
	// a pre-existing team is a reconcile error, mirroring the org-level claim model
	// (ADR-19), even when the team happens to be bound to the entry's oidcGroup. It
	// is distinct from ReasonConflict so an operator can tell an org-name conflict
	// from a team conflict.
	ReasonTeamConflict = "TeamConflict"
	// ReasonOrganizationNotReady marks a Repository's conditions False because the
	// Quay organization named by spec.organizationRef does not yet exist. The
	// Repository reconciler never creates the org (AC #9); it requeues until the
	// Organization reconciler provisions it.
	ReasonOrganizationNotReady = "OrganizationNotReady"
	// ReasonWebhookURLNotFound marks the WebhookConfigured condition False because
	// the webhook urlSecretRef Secret (or its key) could not be resolved. This is
	// recoverable: the reconciler requeues so a later-created Secret takes effect.
	ReasonWebhookURLNotFound = "WebhookURLNotFound"
	// ReasonWebhookURLReadError marks the WebhookConfigured condition False
	// because the Kubernetes API read for the webhook urlSecretRef Secret failed
	// for a transient reason.
	ReasonWebhookURLReadError = "WebhookURLReadError"
	// ReasonInvalidWebhook marks a condition False because spec.webhook violated
	// the mutual-exclusion rule (neither or both of url/urlSecretRef set) at
	// runtime, a defense-in-depth check behind the CRD XValidation.
	ReasonInvalidWebhook = "InvalidWebhook"
	// ReasonWebhookConfigured marks the WebhookConfigured condition True because
	// the repo_push notification reflects the resolved webhook URL.
	ReasonWebhookConfigured = "WebhookConfigured"
	// ReasonWebhookNotConfigured marks the WebhookConfigured condition False (with
	// no error) because spec.webhook is unset — the repository is intentionally
	// webhookless.
	ReasonWebhookNotConfigured = "WebhookNotConfigured"
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

func conditionsTransitioned(before, after []metav1.Condition, types ...string) bool {
	for _, typ := range types {
		old := meta.FindStatusCondition(before, typ)
		cur := meta.FindStatusCondition(after, typ)
		switch {
		case old == nil && cur == nil:
			continue
		case old == nil || cur == nil:
			return true
		case old.Status != cur.Status || old.Reason != cur.Reason || old.ObservedGeneration != cur.ObservedGeneration:
			return true
		}
	}
	return false
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

// setTeamConflict marks Programmed and Ready False with reason TeamConflict — the
// team-level analog of setConflict, used when a spec.syncedTeams entry names a
// pre-existing Quay team this resource did not create. It returns true when either
// condition changed.
func setTeamConflict(conditions *[]metav1.Condition, message string, observedGeneration int64) bool {
	return markNotReady(conditions, ReasonTeamConflict, message, observedGeneration)
}

// setWebhookCondition sets the WebhookConfigured condition to the given status
// with the supplied reason and message, stamped with observedGeneration. It is
// the Repository reconciler's webhook-specific status helper, kept here so the
// Repository draws its condition vocabulary from the same place as Organization.
// It returns true when the condition changed.
func setWebhookCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string, observedGeneration int64) bool {
	return setCondition(conditions, ConditionWebhookConfigured, string(status), reason, message, observedGeneration)
}

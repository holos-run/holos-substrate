// Package keycloak holds the controller-runtime reconcilers for the
// keycloak.holos.run API group (ADR-20): the KeycloakInstance reconciler and the
// KeycloakGroup reconciler (HOL-1346), with the KeycloakUser and KeycloakClient
// reconcilers landing in a later phase (HOL-1347). The reconcilers drive the
// target Keycloak realm through the internal/keycloak Admin REST client,
// authenticating with the admin credential named by a resource's
// credentialsSecretRef and resolved from the controller's own namespace.
//
// The package mirrors internal/controller/quay file-for-file: this conditions.go
// is the home of the Gateway-API-style status convention shared by every
// reconciler in the group, credentials.go resolves the admin credential Secret,
// and metrics.go registers the custom Prometheus collectors. Keeping the
// condition and reason vocabulary in one place means the Phase-5 reconcilers reuse
// exactly the same helpers the instance and group reconcilers establish here.
package keycloak

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keycloakv1alpha1 "github.com/holos-run/holos-paas/api/keycloak/v1alpha1"
)

// requeueImmediately is a negligible RequeueAfter delay used to re-enqueue a
// reconcile right after a write that bumps the resourceVersion (e.g. adding a
// finalizer), so the next pass operates on the fresh object. It replaces the
// deprecated ctrl.Result.Requeue=true (staticcheck SA1019): any positive
// RequeueAfter requeues, and a millisecond is effectively immediate while
// keeping the result non-zero so test helpers can detect the requeue. Mirrors
// internal/controller/quay's constant of the same name.
const requeueImmediately = time.Millisecond

// requeueDependency is the backoff used when a reconcile cannot proceed because a
// declarative dependency is not yet satisfied — the referenced KeycloakInstance is
// absent or not Ready, or a cross-namespace ReferenceGrant is missing. It is a
// modest periodic re-check (not the negligible requeueImmediately, which would
// hot-loop an absent dependency) that backstops the watch-driven recovery
// SetupWithManager wires: a change to the instance or grant re-enqueues the
// dependent group promptly, and this requeue covers anything not watched.
const requeueDependency = 30 * time.Second

// keycloakExternalResourceResync is the steady-state validation cadence for
// Keycloak-backed external-resource CRs. A Ready=True object still rechecks
// Keycloak periodically so lastValidatedTime remains actionable and out-of-band
// drift is eventually remediated even without a spec change.
const keycloakExternalResourceResync = time.Hour

// Condition types surfaced on keycloak.holos.run resource status. They re-export
// the vocabulary the API package declares (keycloakv1alpha1.ConditionAccepted /
// ConditionProgrammed / ConditionReady) so this controller package draws from one
// source of truth and the printer columns (which match type=="Ready") stay
// consistent. They follow the Gateway API convention:
//
//   - Accepted   — the spec was understood and claimed by this resource.
//   - Programmed — the desired state was written into Keycloak.
//   - Ready      — the resource is fully provisioned and usable.
const (
	// ConditionAccepted reports whether the spec was accepted as valid and
	// claimed by this resource (Gateway-API Accepted).
	ConditionAccepted = keycloakv1alpha1.ConditionAccepted
	// ConditionProgrammed reports whether the desired state has been programmed
	// into Keycloak (Gateway-API Programmed).
	ConditionProgrammed = keycloakv1alpha1.ConditionProgrammed
	// ConditionReady reports whether the resource has been fully provisioned in
	// Keycloak (Gateway-API Ready).
	ConditionReady = keycloakv1alpha1.ConditionReady
)

// Condition reasons. They re-export the shared reason vocabulary the API package
// declares, plus the controller-only reasons (Released, InstanceNotReady) the
// reconcilers need but the API surface does not. Reasons are stable, CamelCase
// machine-readable tokens (metav1.Condition requires this) describing why a
// condition holds its current status.
const (
	// ReasonCreated marks the Keycloak object as newly created by this resource.
	ReasonCreated = keycloakv1alpha1.ReasonCreated
	// ReasonAdopted marks a pre-existing Keycloak object adopted by this resource.
	ReasonAdopted = keycloakv1alpha1.ReasonAdopted
	// ReasonConflict marks a condition False because a pre-existing,
	// externally-created Keycloak object of the same identity exists and the
	// resource did not opt in to adoption (spec.adopt). The object is never
	// silently seized (ADR-20 claim model).
	ReasonConflict = keycloakv1alpha1.ReasonConflict
	// ReasonCredentialsNotFound marks a condition False because the credential
	// Secret (or a required key within it) could not be resolved.
	ReasonCredentialsNotFound = keycloakv1alpha1.ReasonCredentialsNotFound
	// ReasonReferenceNotGranted marks a condition False because a cross-namespace
	// keycloak.holos.run reference is not authorized by a security.holos.run
	// ReferenceGrant.
	ReasonReferenceNotGranted = keycloakv1alpha1.ReasonReferenceNotGranted
	// ReasonInstanceMismatch marks a condition False because a membership CR's
	// instanceRef does not match its target group's instanceRef after defaulting.
	ReasonInstanceMismatch = keycloakv1alpha1.ReasonInstanceMismatch
	// ReasonMemberNotFound marks a condition False because a declared membership
	// email did not resolve to an existing Keycloak user.
	ReasonMemberNotFound = keycloakv1alpha1.ReasonMemberNotFound
	// ReasonKeycloakError marks a condition False because a Keycloak admin-API
	// call failed.
	ReasonKeycloakError = keycloakv1alpha1.ReasonKeycloakError
	// ReasonReconciled marks a resource as in steady state — its Keycloak object
	// reflects the spec.
	ReasonReconciled = keycloakv1alpha1.ReasonReconciled
	// ReasonReleased marks an adopted or out-of-band-replaced Keycloak group
	// released on CR removal (the finalizer dropped without deleting) — adoption
	// and the recreate-at-same-path race are both non-destructive. It is a
	// controller-layer reason with no API-package counterpart.
	ReasonReleased = "Released"
	// ReasonInstanceNotReady marks a condition False because the referenced
	// KeycloakInstance does not exist or has not reported Ready. The dependent
	// reconciler requeues until the instance is provisioned, mirroring quay's
	// OrganizationNotReady.
	ReasonInstanceNotReady = "InstanceNotReady"
	// ReasonGroupNotReady marks a condition False because the referenced
	// KeycloakGroup does not exist or has not reported Ready.
	ReasonGroupNotReady = "GroupNotReady"
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
// and message and the observed generation. It is the success path every
// reconciler calls once Keycloak reflects the desired state. It returns true when
// any of the three conditions changed, so callers can skip a redundant status
// write and event (avoiding a self-triggered reconcile loop).
func markReady(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	a := setCondition(conditions, ConditionAccepted, string(metav1.ConditionTrue), reason, message, observedGeneration)
	p := setCondition(conditions, ConditionProgrammed, string(metav1.ConditionTrue), reason, message, observedGeneration)
	r := setCondition(conditions, ConditionReady, string(metav1.ConditionTrue), reason, message, observedGeneration)
	return a || p || r
}

// markNotReady sets Programmed and Ready False with the given reason and message
// and the observed generation, leaving Accepted untouched (the spec was still
// understood; it just could not be programmed). It is the failure path for a
// credential, reference-grant, or Keycloak error. It returns true when either
// condition changed.
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

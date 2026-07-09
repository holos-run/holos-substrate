// Package quay holds the controller-runtime reconcilers for the quay.holos.run
// API group. The reconcilers drive the in-cluster Quay registry through the
// internal/quay REST client, authenticating with the superuser OAuth-Application
// credential named by a resource's credentialsSecretRef.
//
// This file holds the small helpers both reconcilers use to set Gateway-API-
// style conditions with apimachinery/pkg/api/meta.SetStatusCondition. The
// condition vocabulary itself lives in the API package.
package quay

import (
	"time"

	quayv1alpha1 "github.com/holos-run/holos-substrate/api/quay/v1alpha1"
	ctrlshared "github.com/holos-run/holos-substrate/internal/controller/shared"
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

// requeueDependency is the backoff used when a reconcile cannot proceed because
// a declarative dependency is not yet satisfied. Watch-driven recovery handles
// prompt wakeups; this interval is only the periodic backstop.
const requeueDependency = 30 * time.Second

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
	a := setCondition(conditions, quayv1alpha1.ConditionAccepted, string(metav1.ConditionTrue), reason, message, observedGeneration)
	p := setCondition(conditions, quayv1alpha1.ConditionProgrammed, string(metav1.ConditionTrue), reason, message, observedGeneration)
	r := setCondition(conditions, quayv1alpha1.ConditionReady, string(metav1.ConditionTrue), reason, message, observedGeneration)
	return a || p || r
}

func conditionsTransitioned(before, after []metav1.Condition, types ...string) bool {
	return ctrlshared.ConditionsTransitioned(before, after, types...)
}

// markNotReady sets Accepted True and Programmed/Ready False with the given
// reason and message. The spec was understood; it just could not be programmed.
// It returns true when any condition changed.
func markNotReady(conditions *[]metav1.Condition, reason, message string, observedGeneration int64) bool {
	a := setCondition(conditions, quayv1alpha1.ConditionAccepted, string(metav1.ConditionTrue), reason, message, observedGeneration)
	p := setCondition(conditions, quayv1alpha1.ConditionProgrammed, string(metav1.ConditionFalse), reason, message, observedGeneration)
	r := setCondition(conditions, quayv1alpha1.ConditionReady, string(metav1.ConditionFalse), reason, message, observedGeneration)
	return a || p || r
}

// setConflict marks Programmed and Ready False with reason Conflict. It returns
// true when either condition changed.
func setConflict(conditions *[]metav1.Condition, message string, observedGeneration int64) bool {
	return markNotReady(conditions, quayv1alpha1.ReasonConflict, message, observedGeneration)
}

// setTeamConflict marks Programmed and Ready False with reason TeamConflict — the
// team-level analog of setConflict, used when a spec.syncedTeams entry names a
// pre-existing Quay team this resource did not create. It returns true when either
// condition changed.
func setTeamConflict(conditions *[]metav1.Condition, message string, observedGeneration int64) bool {
	return markNotReady(conditions, quayv1alpha1.ReasonTeamConflict, message, observedGeneration)
}

// setWebhookCondition sets the WebhookConfigured condition to the given status
// with the supplied reason and message, stamped with observedGeneration. It is
// the Repository reconciler's webhook-specific status helper, kept here so the
// Repository draws its condition vocabulary from the same place as Organization.
// It returns true when the condition changed.
func setWebhookCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string, observedGeneration int64) bool {
	return setCondition(conditions, quayv1alpha1.ConditionWebhookConfigured, string(status), reason, message, observedGeneration)
}

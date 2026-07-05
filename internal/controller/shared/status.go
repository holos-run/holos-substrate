package shared

import (
	"context"
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func PatchStatus(ctx context.Context, c client.Client, base, obj client.Object, resource string) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), base); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading %s before status patch: %w", resource, err)
	}
	if err := c.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("updating %s status: %w", resource, err)
	}
	return nil
}

func GenerationReady(conditions []v1.Condition, conditionType string, generation int64) bool {
	for _, c := range conditions {
		if c.Type == conditionType {
			return c.Status == v1.ConditionTrue && c.ObservedGeneration == generation
		}
	}
	return false
}

func MutationStamp(observedGeneration, generation int64, ready, healedDrift bool) (v1.Time, string, bool) {
	now := v1.Now()
	reason := "DriftRemediation"
	if observedGeneration != generation || !ready {
		reason = "SpecChange"
	}
	return now, reason, reason == "DriftRemediation" || healedDrift
}

func StringSlicesEqual(a, b []string) bool {
	return slices.Equal(a, b)
}

func ConditionsTransitioned(before, after []v1.Condition, types ...string) bool {
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

func RecordConflict(ctx context.Context, recorder record.EventRecorder, logger logr.Logger, obj client.Object, reason, message, logMessage string, changed bool, update func(context.Context) error, requeueAfter v1.Duration) (ctrl.Result, error) {
	if !changed {
		if requeueAfter.Duration > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter.Duration}, nil
		}
		return ctrl.Result{}, nil
	}
	recorder.Event(obj, corev1.EventTypeWarning, reason, message)
	logger.Info(logMessage, "namespace", obj.GetNamespace(), "name", obj.GetName())
	if err := update(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if requeueAfter.Duration > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter.Duration}, nil
	}
	return ctrl.Result{}, nil
}

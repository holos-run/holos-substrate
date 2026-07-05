package shared

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func PatchStatus(ctx context.Context, c client.Client, base, obj client.Object, resource string) error {
	desiredStatus, err := statusSnapshot(obj)
	if err != nil {
		return fmt.Errorf("reading desired %s status: %w", resource, err)
	}
	key := client.ObjectKeyFromObject(obj)
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := c.Get(ctx, key, base); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("reading %s before status update: %w", resource, err)
		}
		if err := setStatus(base, desiredStatus); err != nil {
			return fmt.Errorf("setting desired %s status: %w", resource, err)
		}
		if err := c.Status().Update(ctx, base); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		obj.SetResourceVersion(base.GetResourceVersion())
		return nil
	}); err != nil {
		return fmt.Errorf("updating %s status: %w", resource, err)
	}
	return nil
}

func statusSnapshot(obj client.Object) (reflect.Value, error) {
	status, err := statusField(obj)
	if err != nil {
		return reflect.Value{}, err
	}
	snapshot := reflect.New(status.Type()).Elem()
	snapshot.Set(status)
	return snapshot, nil
}

func setStatus(obj client.Object, status reflect.Value) error {
	target, err := statusField(obj)
	if err != nil {
		return err
	}
	if !status.Type().AssignableTo(target.Type()) {
		return fmt.Errorf("status type %s is not assignable to %s", status.Type(), target.Type())
	}
	target.Set(status)
	return nil
}

func statusField(obj client.Object) (reflect.Value, error) {
	if obj == nil {
		return reflect.Value{}, fmt.Errorf("object is nil")
	}
	value := reflect.ValueOf(obj)
	if value.Kind() != reflect.Ptr || value.IsNil() {
		return reflect.Value{}, fmt.Errorf("object must be a non-nil pointer")
	}
	elem := value.Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("object must point to a struct")
	}
	status := elem.FieldByName("Status")
	if !status.IsValid() {
		return reflect.Value{}, fmt.Errorf("object has no Status field")
	}
	if !status.CanSet() {
		return reflect.Value{}, fmt.Errorf("status field is not settable")
	}
	return status, nil
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

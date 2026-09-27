package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
)

var workerPoolListGVK = schema.GroupVersionKind{Group: "ate.dev", Version: "v1alpha1", Kind: "WorkerPoolList"}

// fits reports whether some WorkerPool of the template's class, whose labels
// satisfy its worker selector, has per-worker limits at least as large as the
// template's. It is read-only: placement and capacity stay Substrate's job.
func (r *ActorTemplateReconciler) fits(ctx context.Context, at *substratev1alpha1.ActorTemplate) (bool, string, error) {
	var pools unstructured.UnstructuredList
	pools.SetGroupVersionKind(workerPoolListGVK)
	if err := r.List(ctx, &pools); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return true, "WorkerPool API not found; fit not checked", nil
		}
		return false, "", err
	}

	var selector map[string]string
	if at.Spec.WorkerSelector != nil {
		selector = at.Spec.WorkerSelector.MatchLabels
	}
	matched := 0
	for _, p := range pools.Items {
		class, _, _ := unstructured.NestedString(p.Object, "spec", "sandboxClass")
		if class == "" {
			class = string(substratev1alpha1.SandboxClassGVisor)
		}
		if class != string(at.Spec.SandboxClass) {
			continue
		}
		// Substrate registers each worker with its pool's own labels.
		if !subset(selector, p.GetLabels()) {
			continue
		}
		matched++
		limits, _, _ := unstructured.NestedStringMap(p.Object, "spec", "template", "resources", "limits")
		if within(at.Spec.Resources.Limits, limits) {
			return true, fmt.Sprintf("fits WorkerPool %s/%s", p.GetNamespace(), p.GetName()), nil
		}
	}
	if matched == 0 {
		return false, "no WorkerPool matches the sandbox class and worker selector", nil
	}
	return false, "limits exceed every matching WorkerPool's per-worker limits", nil
}

func subset(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// within treats a resource the pool does not limit as unbounded.
func within(want corev1.ResourceList, poolLimits map[string]string) bool {
	for name, q := range want {
		raw, ok := poolLimits[string(name)]
		if !ok {
			continue
		}
		limit, err := resource.ParseQuantity(raw)
		if err != nil || q.Cmp(limit) > 0 {
			return false
		}
	}
	return true
}

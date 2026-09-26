package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
)

type ActorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actors/finalizers,verbs=update

func (r *ActorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *ActorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&substratev1alpha1.Actor{}).
		Named("actor").
		Complete(r)
}

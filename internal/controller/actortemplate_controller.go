package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
)

type ActorTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actortemplates,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actortemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actortemplates/finalizers,verbs=update

func (r *ActorTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *ActorTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&substratev1alpha1.ActorTemplate{}).
		Named("actortemplate").
		Complete(r)
}

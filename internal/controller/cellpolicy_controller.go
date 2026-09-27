package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	cellPolicyName       = "oc-substrate-allow-egress"
	labelEnvironment     = "openchoreo.dev/environment"
	labelComponent       = "openchoreo.dev/component"
	labelPolicyManagedBy = "app.kubernetes.io/managed-by"
)

// CellPolicyReconciler lets Substrate's egress pod into every OpenChoreo cell,
// so actors reach pod components the way pods in their cell do. OpenChoreo's
// per-component policies cannot admit it: the egress pod is in no cell and
// serves every tenant. Visibility for actor callers is enforced at the source
// by each actor's EgressPolicy, so this adds no reach a pod would not have.
// The policy carries no OpenChoreo labels, so OpenChoreo's GC leaves it alone.
type CellPolicyReconciler struct {
	client.Client
	EgressNamespace string
	EgressLabels    map[string]string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch

func (r *CellPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if _, cell := ns.Labels[labelEnvironment]; !cell || !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: cellPolicyName}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = map[string]string{labelPolicyManagedBy: endpointSliceManager}
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: labelComponent, Operator: metav1.LabelSelectorOpExists},
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: r.EgressNamespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: r.EgressLabels},
			}}}},
		}
		return nil
	})
	return ctrl.Result{}, err
}

func (r *CellPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cells := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[labelEnvironment]
		return ok
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(cells)).
		Watches(&networkingv1.NetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []ctrl.Request {
				if o.GetName() != cellPolicyName {
					return nil
				}
				return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: o.GetNamespace()}}}
			})).
		Named("cellpolicy").
		Complete(r)
}

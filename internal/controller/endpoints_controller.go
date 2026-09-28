package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	substratev1alpha1 "github.com/rashadism/ate-oc/api/v1alpha1"
)

const (
	endpointSliceManager = "oc-substrate.substrate.openchoreo.dev"
	actorServiceIndex    = "spec.serviceName"
)

// EndpointsReconciler points each actor's selector-less Service at the front
// door. The slice carries none of the RenderedRelease labels, so OpenChoreo's
// garbage collection leaves it alone; the Service owns it instead.
type EndpointsReconciler struct {
	client.Client
	FrontDoorNamespace string
	FrontDoorSelector  labels.Selector
	FrontDoorPort      int32
}

// +kubebuilder:rbac:groups="",resources=services;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete

func (r *EndpointsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var act substratev1alpha1.Actor
	if err := r.Get(ctx, req.NamespacedName, &act); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if act.Spec.ServiceName == "" || !act.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	var svc corev1.Service
	err := r.Get(ctx, types.NamespacedName{Namespace: act.Namespace, Name: act.Spec.ServiceName}, &svc)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(svc.Spec.Selector) > 0 {
		return ctrl.Result{}, nil
	}

	addrs, err := r.frontDoorAddresses(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: svc.Namespace, Name: svc.Name + "-substrate"}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, slice, func() error {
		slice.Labels = map[string]string{
			discoveryv1.LabelServiceName: svc.Name,
			discoveryv1.LabelManagedBy:   endpointSliceManager,
		}
		slice.AddressType = discoveryv1.AddressTypeIPv4
		slice.Ports = nil
		for _, p := range svc.Spec.Ports {
			slice.Ports = append(slice.Ports, discoveryv1.EndpointPort{
				Name: ptr.To(p.Name), Port: ptr.To(r.FrontDoorPort), Protocol: ptr.To(corev1.ProtocolTCP),
			})
		}
		slice.Endpoints = nil
		for _, a := range addrs {
			slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
				Addresses:  []string{a},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			})
		}
		return controllerutil.SetOwnerReference(&svc, slice, r.Scheme())
	})
	return ctrl.Result{}, err
}

func (r *EndpointsReconciler) frontDoorAddresses(ctx context.Context) ([]string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.FrontDoorNamespace),
		client.MatchingLabelsSelector{Selector: r.FrontDoorSelector}); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pods.Items {
		if p.Status.PodIP == "" || !p.DeletionTimestamp.IsZero() || !podReady(&p) {
			continue
		}
		out = append(out, p.Status.PodIP)
	}
	return out, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// TrimPod keeps the pod fields the operator reads, to bound the cluster-wide
// pod cache: labels and addresses for egress, readiness for front door pods.
func TrimPod(o any) (any, error) {
	p, ok := o.(*corev1.Pod)
	if !ok {
		return o, nil
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels, UID: p.UID,
			ResourceVersion: p.ResourceVersion, DeletionTimestamp: p.DeletionTimestamp},
		Spec: corev1.PodSpec{HostNetwork: p.Spec.HostNetwork},
		Status: corev1.PodStatus{Phase: p.Status.Phase, PodIP: p.Status.PodIP, PodIPs: p.Status.PodIPs,
			Conditions: p.Status.Conditions},
	}, nil
}

func actorServiceName(o client.Object) []string {
	if s := o.(*substratev1alpha1.Actor).Spec.ServiceName; s != "" {
		return []string{s}
	}
	return nil
}

func (r *EndpointsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &substratev1alpha1.Actor{},
		actorServiceIndex, actorServiceName); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&substratev1alpha1.Actor{}).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.forService)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.forFrontDoorPod)).
		Named("endpoints").
		Complete(r)
}

func (r *EndpointsReconciler) forService(ctx context.Context, o client.Object) []ctrl.Request {
	var list substratev1alpha1.ActorList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{actorServiceIndex: o.GetName()}); err != nil {
		return nil
	}
	return requests(list.Items)
}

func (r *EndpointsReconciler) forFrontDoorPod(ctx context.Context, o client.Object) []ctrl.Request {
	if o.GetNamespace() != r.FrontDoorNamespace || !r.FrontDoorSelector.Matches(labels.Set(o.GetLabels())) {
		return nil
	}
	var list substratev1alpha1.ActorList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	return requests(list.Items)
}

func requests(actors []substratev1alpha1.Actor) []ctrl.Request {
	reqs := make([]ctrl.Request, 0, len(actors))
	for _, a := range actors {
		if a.Spec.ServiceName != "" {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&a)})
		}
	}
	return reqs
}

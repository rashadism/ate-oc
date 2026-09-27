package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
)

func frontDoorPod(name, ip string, ready bool) *corev1.Pod {
	s := corev1.ConditionFalse
	if ready {
		s = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "openchoreo-substrate", Name: name,
			Labels: map[string]string{"app.kubernetes.io/name": "oc-substrate-frontdoor"}},
		Status: corev1.PodStatus{PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: s}}},
	}
}

func endpointsEnv(t *testing.T, objs ...client.Object) *EndpointsReconciler {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = discoveryv1.AddToScheme(s)
	_ = substratev1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithIndex(&substratev1alpha1.Actor{}, actorServiceIndex, actorServiceName).Build()
	return &EndpointsReconciler{
		Client:             c,
		FrontDoorNamespace: "openchoreo-substrate",
		FrontDoorSelector:  labels.SelectorFromSet(labels.Set{"app.kubernetes.io/name": "oc-substrate-frontdoor"}),
		FrontDoorPort:      8080,
	}
}

func svcActor() *substratev1alpha1.Actor {
	a := actorCR(id)
	a.Spec.ServiceName = "orders"
	return a
}

func service(selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "orders", UID: "svc-uid"},
		Spec: corev1.ServiceSpec{Selector: selector, Ports: []corev1.ServicePort{
			{Name: "http", Port: 80}, {Name: "admin", Port: 8080},
		}},
	}
}

func TestEndpointSlice(t *testing.T) {
	r := endpointsEnv(t, svcActor(), service(nil),
		frontDoorPod("fd-1", "10.2.0.1", true), frontDoorPod("fd-2", "10.2.0.2", false))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: actorKey}); err != nil {
		t.Fatal(err)
	}
	var slice discoveryv1.EndpointSlice
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "orders-substrate"}, &slice); err != nil {
		t.Fatal(err)
	}
	if slice.Labels[discoveryv1.LabelServiceName] != "orders" || slice.Labels[discoveryv1.LabelManagedBy] != endpointSliceManager {
		t.Fatalf("labels = %v", slice.Labels)
	}
	for k := range slice.Labels {
		if k == "openchoreo.dev/managed-by" {
			t.Fatal("slice must not carry RenderedRelease labels")
		}
	}
	if len(slice.Ports) != 2 || *slice.Ports[0].Name != "http" || *slice.Ports[1].Name != "admin" ||
		*slice.Ports[0].Port != 8080 || *slice.Ports[1].Port != 8080 {
		t.Fatalf("ports = %+v", slice.Ports)
	}
	if len(slice.Endpoints) != 1 || slice.Endpoints[0].Addresses[0] != "10.2.0.1" {
		t.Fatalf("only ready front door pods: %+v", slice.Endpoints)
	}
	if len(slice.OwnerReferences) != 1 || slice.OwnerReferences[0].UID != "svc-uid" {
		t.Fatalf("owner = %+v", slice.OwnerReferences)
	}

	if reqs := r.forFrontDoorPod(context.Background(), frontDoorPod("fd-3", "10.2.0.3", true)); len(reqs) != 1 {
		t.Fatalf("front door changes should requeue actors, got %v", reqs)
	}
	if reqs := r.forService(context.Background(), service(nil)); len(reqs) != 1 || reqs[0].NamespacedName != actorKey {
		t.Fatalf("service changes should requeue its actor, got %v", reqs)
	}
}

func TestEndpointSliceSkipped(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"service with a selector": {svcActor(), service(map[string]string{"app": "x"}), frontDoorPod("fd", "10.2.0.1", true)},
		"no service yet":          {svcActor(), frontDoorPod("fd", "10.2.0.1", true)},
		"actor without a service": {actorCR(id), service(nil), frontDoorPod("fd", "10.2.0.1", true)},
	} {
		t.Run(name, func(t *testing.T) {
			r := endpointsEnv(t, objs...)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: actorKey}); err != nil {
				t.Fatal(err)
			}
			err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "orders-substrate"}, &discoveryv1.EndpointSlice{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("no slice expected, err=%v", err)
			}
		})
	}
}

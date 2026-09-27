package frontdoor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
)

func pod(ns, name, ip string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Status:     corev1.PodStatus{Phase: phase, PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}}},
	}
}

func TestClusterDirectory(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	objs := []client.Object{
		pod("ate-system", "egress", "10.1.0.1", map[string]string{"app": "atenet-egress"}, corev1.PodRunning),
		pod("openchoreo-data-plane", "gw", "10.1.0.2", map[string]string{LabelSystemComponent: "gateway"}, corev1.PodRunning),
		pod(cellA, "app", "10.1.0.3", nil, corev1.PodRunning),
		pod(cellB, "done", "10.1.0.4", nil, corev1.PodSucceeded),
		pod("ate-system", "router", "10.1.0.5", map[string]string{"app": "atenet-router"}, corev1.PodRunning),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cellA, Labels: map[string]string{LabelNamespace: "acme"}}},
		&v1alpha1.Actor{
			ObjectMeta: metav1.ObjectMeta{Namespace: cellA, Name: "orders-dev-1234"},
			Spec: v1alpha1.ActorSpec{ServiceName: "orders", Endpoints: []v1alpha1.Endpoint{
				{Name: "http", Port: 80},
				{Name: "admin", Port: 8080, TargetPort: 9090, Visibility: []v1alpha1.EndpointVisibility{v1alpha1.VisibilityNamespace}},
			}},
			Status: v1alpha1.ActorStatus{Name: "a-0123456789", UID: "u"},
		},
		&v1alpha1.Actor{
			ObjectMeta: metav1.ObjectMeta{Namespace: cellA, Name: "fresh"},
			Spec:       v1alpha1.ActorSpec{ServiceName: "fresh", Endpoints: []v1alpha1.Endpoint{{Name: "http", Port: 80}}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithIndex(&corev1.Pod{}, podIPIndex, func(o client.Object) []string {
			p := o.(*corev1.Pod)
			if p.Status.PodIP == "" {
				return nil
			}
			return []string{p.Status.PodIP}
		}).
		WithIndex(&v1alpha1.Actor{}, actorServiceIndex, func(o client.Object) []string {
			return []string{o.(*v1alpha1.Actor).Spec.ServiceName}
		}).Build()
	d := &ClusterDirectory{Reader: c, EgressNamespace: "ate-system", EgressLabels: map[string]string{"app": "atenet-egress"}}

	callers := map[string]Caller{
		"10.1.0.1": {Kind: CallerEgress, Namespace: "ate-system"},
		"10.1.0.2": {Kind: CallerPod, Namespace: "openchoreo-data-plane", SystemComponent: true},
		"10.1.0.3": {Kind: CallerPod, Namespace: cellA},
		"10.1.0.4": {},
		"10.1.0.5": {Kind: CallerPod, Namespace: "ate-system"},
		"10.9.9.9": {},
	}
	for ip, want := range callers {
		if got := d.Caller(ip); got != want {
			t.Errorf("Caller(%s) = %+v, want %+v", ip, got, want)
		}
	}

	tg, ok := d.Target(cellA, "orders", 8080)
	if !ok || tg.Actor != "a-0123456789" || tg.ActorPort != 9090 || !tg.Ready || len(tg.Visibility) != 1 {
		t.Fatalf("target = %+v ok=%v", tg, ok)
	}
	if tg, _ := d.Target(cellA, "orders", 80); tg.ActorPort != 80 {
		t.Fatalf("targetPort should default to port, got %d", tg.ActorPort)
	}
	if _, ok := d.Target(cellA, "orders", 443); ok {
		t.Fatal("undeclared port must not resolve")
	}
	if tg, ok := d.Target(cellA, "fresh", 80); !ok || tg.Ready {
		t.Fatalf("an actor not created yet is not ready: %+v", tg)
	}
	if d.NamespaceLabels(cellA)[LabelNamespace] != "acme" || d.NamespaceLabels("nope") != nil {
		t.Fatal("namespace labels")
	}

	live := fake.NewClientBuilder().WithScheme(s).WithObjects(pod(cellB, "new", "10.1.0.9", nil, corev1.PodRunning)).
		WithIndex(&corev1.Pod{}, podIPIndex, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Status.PodIP}
		}).Build()
	d.Live = live
	if got := d.Caller("10.1.0.9"); got.Kind != CallerPod || got.Namespace != cellB {
		t.Fatalf("a pod the cache has not seen yet should resolve live, got %+v", got)
	}
}

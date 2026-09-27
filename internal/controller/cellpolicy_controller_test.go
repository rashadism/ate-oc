package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCellPolicy(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dp-acme-shop-dev", Labels: map[string]string{labelEnvironment: "dev"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	).Build()
	r := &CellPolicyReconciler{Client: c, EgressNamespace: "ate-system", EgressLabels: map[string]string{"app": "atenet-egress"}}

	for _, ns := range []string{"dp-acme-shop-dev", "kube-system"} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "dp-acme-shop-dev", Name: cellPolicyName}, &np); err != nil {
		t.Fatal(err)
	}
	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != "ate-system" || peer.PodSelector.MatchLabels["app"] != "atenet-egress" {
		t.Fatalf("peer = %+v", peer)
	}
	if np.Spec.PodSelector.MatchExpressions[0].Key != labelComponent || len(np.Spec.Ingress[0].Ports) != 0 {
		t.Fatalf("policy should admit egress to component pods on every port: %+v", np.Spec)
	}
	for k := range np.Labels {
		if k == "openchoreo.dev/managed-by" {
			t.Fatal("must not carry OpenChoreo's labels")
		}
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "kube-system", Name: cellPolicyName}, &networkingv1.NetworkPolicy{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("non-cell namespaces are left alone, err=%v", err)
	}
}

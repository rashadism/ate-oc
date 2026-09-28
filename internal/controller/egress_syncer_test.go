package controller

import (
	"context"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/rashadism/ate-oc/third_party/ateapipb"
)

func egressObjects() []client.Object {
	org := map[string]string{"openchoreo.dev/namespace": "acme", "openchoreo.dev/environment": "dev"}
	return []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.32.0.0/24"}}},
		&networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"},
			Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"34.118.224.0/20"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: org}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dp-other", Labels: org}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "dp-other", Name: "ledger", Labels: map[string]string{"openchoreo.dev/component": "ledger"}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.32.0.20"}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "dp-other", Name: "db"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.32.0.21"}}},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "dp-other", Name: "openchoreo-ledger"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"openchoreo.dev/component": "ledger"}},
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
			},
		},
	}
}

func (e *env) policyCovers(ip string) (bool, int64) {
	e.t.Helper()
	p, err := e.ate.GetActorEgressPolicy(context.Background(), &pb.GetActorEgressPolicyRequest{Actor: actorRef(ns, id.ActorName())})
	if err != nil {
		e.t.Fatal(err)
	}
	a := netip.MustParseAddr(ip)
	for _, r := range p.GetRules() {
		for _, c := range r.GetCidrs().GetCidrs() {
			if netip.MustParsePrefix(c).Contains(a) {
				return true, p.GetMetadata().GetVersion()
			}
		}
	}
	return false, p.GetMetadata().GetVersion()
}

func TestEgressSyncerMirrorsPodReachability(t *testing.T) {
	e := newEnv(t, true, append(egressObjects(), actorTemplate(), actorCR(id))...)
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	s := &EgressSyncer{Client: e.actors.Client, Ate: e.ate, Blocked: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	for ip, want := range map[string]bool{
		"1.1.1.1":         true,  // internet
		"10.32.0.21":      true,  // resource pod without a policy
		"10.32.0.20":      false, // project-only component in another cell
		"169.254.169.254": false, // metadata server
		"10.32.0.99":      false, // unknown pod address
	} {
		if got, _ := e.policyCovers(ip); got != want {
			t.Errorf("%s reachable = %v, want %v", ip, got, want)
		}
	}
	wantActorCond(t, e.actorCR(), CondEgress, metav1.ConditionTrue, "MirrorsCell")

	_, v1 := e.policyCovers("1.1.1.1")
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, v2 := e.policyCovers("1.1.1.1"); v2 != v1 {
		t.Fatal("an unchanged cluster must not rewrite the policy")
	}

	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dp-other", Name: "cache"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.32.0.99"}}},
	}
	if err := e.actors.Create(context.Background(), newPod); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.policyCovers("10.32.0.99"); !got {
		t.Fatal("a new unprotected pod becomes reachable on the next sync")
	}
}

func TestEgressSyncerFailsClosedWithoutClusterRanges(t *testing.T) {
	e := newEnv(t, true, actorTemplate(), actorCR(id))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	s := &EgressSyncer{Client: e.actors.Client, Ate: e.ate}
	if err := s.Sync(context.Background()); err == nil {
		t.Fatal("want an error when the cluster's ranges are unknown")
	}
	if _, err := e.ate.GetActorEgressPolicy(context.Background(), &pb.GetActorEgressPolicyRequest{
		Actor: actorRef(ns, id.ActorName()),
	}); err == nil {
		t.Fatal("no policy (deny all) until the ranges are known")
	}
}

func TestEgressSyncerSkipsActorsNotCreatedYet(t *testing.T) {
	act := actorCR(id)
	act.Status.Name = id.ActorName()
	e := newEnv(t, false, append(egressObjects(), actorTemplate(), act)...)
	s := &EgressSyncer{Client: e.actors.Client, Ate: e.ate}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("an actor without a Substrate uid is skipped: %v", err)
	}
}

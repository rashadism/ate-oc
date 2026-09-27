package reach

import (
	"net/netip"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	cellA = "dp-acme-shop-dev"
	cellB = "dp-acme-billing-dev"
	cellX = "dp-other-shop-dev"
)

var org = map[string]string{"openchoreo.dev/namespace": "acme", "openchoreo.dev/environment": "dev"}

// componentPolicy mirrors internal/networkpolicy: same cell always, plus the
// org/env namespace rule when an endpoint is namespace-visible.
func componentPolicy(ns, component string, namespaceVisible bool) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "openchoreo-" + component},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"openchoreo.dev/component": component}},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
			},
		},
	}
	if namespaceVisible {
		np.Spec.Ingress = append(np.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: org}}},
		})
	}
	return np
}

func component(ns, name, ip string) Pod {
	return Pod{Namespace: ns, Labels: map[string]string{"openchoreo.dev/component": name}, IPs: []netip.Addr{netip.MustParseAddr(ip)}}
}

func svc(ns, name, ip string, selector map[string]string) Service {
	return Service{Namespace: ns, Name: name, Selector: selector, IPs: []netip.Addr{netip.MustParseAddr(ip)}}
}

func snapshot() *Snapshot {
	return &Snapshot{
		ClusterRanges: []netip.Prefix{netip.MustParsePrefix("10.32.0.0/14"), netip.MustParsePrefix("34.118.224.0/20")},
		Pods: []Pod{
			component(cellA, "web", "10.32.0.10"),
			component(cellB, "invoices", "10.32.1.10"),
			component(cellB, "ledger", "10.32.1.11"),
			{Namespace: cellB, Labels: map[string]string{"app": "postgres"}, IPs: []netip.Addr{netip.MustParseAddr("10.32.1.12")}},
			{Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}, IPs: []netip.Addr{netip.MustParseAddr("10.32.2.2")}},
		},
		Services: []Service{
			svc(cellA, "web", "34.118.224.10", map[string]string{"openchoreo.dev/component": "web"}),
			svc(cellB, "invoices", "34.118.225.10", map[string]string{"openchoreo.dev/component": "invoices"}),
			svc(cellB, "ledger", "34.118.225.11", map[string]string{"openchoreo.dev/component": "ledger"}),
			svc(cellB, "ledger-alias", "34.118.225.12", map[string]string{"openchoreo.dev/component": "ledger"}),
			svc(cellB, "postgres", "34.118.225.13", map[string]string{"app": "postgres"}),
			svc(cellB, "agent", "34.118.225.14", nil),
			svc(cellB, "helper", "34.118.225.15", nil),
			svc("kube-system", "kube-dns", "34.118.224.53", map[string]string{"k8s-app": "kube-dns"}),
		},
		Policies: []*networkingv1.NetworkPolicy{
			componentPolicy(cellA, "web", false),
			componentPolicy(cellB, "invoices", true),
			componentPolicy(cellB, "ledger", false),
			componentPolicy(cellB, "agent", false),
			componentPolicy(cellB, "helper", true),
		},
		NamespaceLabels: map[string]map[string]string{
			cellA: org, cellB: org,
			cellX: {"openchoreo.dev/namespace": "other", "openchoreo.dev/environment": "dev"},
		},
	}
}

func covers(ps []netip.Prefix, ip string) bool {
	a := netip.MustParseAddr(ip)
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func TestAllowed(t *testing.T) {
	s := snapshot()
	fromA, fromX := s.Allowed(cellA), s.Allowed(cellX)
	tests := []struct {
		name  string
		ip    string
		fromA bool
		fromX bool
	}{
		{name: "internet", ip: "8.8.8.8", fromA: true, fromX: true},
		{name: "internet ipv6", ip: "2001:4860:4860::8888", fromA: true, fromX: true},
		{name: "node / peered network outside cluster ranges", ip: "10.148.0.5", fromA: true, fromX: true},
		{name: "metadata server", ip: "169.254.169.254"},
		{name: "own cell component pod", ip: "10.32.0.10", fromA: true},
		{name: "own cell component service", ip: "34.118.224.10", fromA: true},
		{name: "namespace-visible component, same org", ip: "10.32.1.10", fromA: true},
		{name: "namespace-visible service, same org", ip: "34.118.225.10", fromA: true},
		{name: "project-only component in another cell", ip: "10.32.1.11"},
		{name: "project-only service in another cell", ip: "34.118.225.11"},
		{name: "another service selecting a protected pod", ip: "34.118.225.12"},
		{name: "resource pod without a policy", ip: "10.32.1.12", fromA: true, fromX: true},
		{name: "resource service", ip: "34.118.225.13", fromA: true, fromX: true},
		{name: "project-only actor service", ip: "34.118.225.14"},
		{name: "namespace-visible actor service", ip: "34.118.225.15", fromA: true},
		{name: "system dns", ip: "34.118.224.53", fromA: true, fromX: true},
		{name: "unknown in-cluster address fails closed", ip: "10.32.3.99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := covers(fromA, tt.ip); got != tt.fromA {
				t.Errorf("from %s: %v, want %v", cellA, got, tt.fromA)
			}
			if got := covers(fromX, tt.ip); got != tt.fromX {
				t.Errorf("from %s: %v, want %v", cellX, got, tt.fromX)
			}
		})
	}
}

func TestSubtract(t *testing.T) {
	out := subtract(netip.MustParsePrefix("0.0.0.0/0"), []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if len(out) != 8 {
		t.Fatalf("0/0 minus 10/8 should be 8 prefixes, got %v", out)
	}
	for ip, want := range map[string]bool{"9.255.255.255": true, "10.0.0.0": false, "10.255.255.255": false, "11.0.0.0": true, "255.255.255.255": true} {
		if covers(out, ip) != want {
			t.Errorf("%s covered = %v, want %v", ip, !want, want)
		}
	}
	if got := subtract(netip.MustParsePrefix("10.1.0.0/16"), []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}); got != nil {
		t.Fatalf("a covered prefix leaves nothing, got %v", got)
	}
}

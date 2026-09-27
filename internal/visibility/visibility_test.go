package visibility

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// policy has the shape internal/networkpolicy renders for a component with
// endpoints api (8080, namespace), web (80, external) and admin (9000, project).
const policy = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: openchoreo-orders, namespace: dp-acme-shop-dev-11111111}
spec:
  podSelector: {matchLabels: {openchoreo.dev/component: orders}}
  policyTypes: [Ingress]
  ingress:
    - from: [{podSelector: {}}]
      ports: [{protocol: TCP, port: 9000}, {protocol: TCP, port: 8080}, {protocol: TCP, port: 80}]
    - from:
        - namespaceSelector: {matchLabels: {openchoreo.dev/namespace: acme, openchoreo.dev/environment: dev}}
      ports: [{protocol: TCP, port: 8080}]
    - from:
        - namespaceSelector: {}
          podSelector: {matchExpressions: [{key: openchoreo.dev/system-component, operator: Exists}]}
      ports: [{protocol: TCP, port: 80}]
`

func TestAdmits(t *testing.T) {
	var np networkingv1.NetworkPolicy
	if err := yaml.Unmarshal([]byte(policy), &np); err != nil {
		t.Fatal(err)
	}
	sameOrg := map[string]string{"openchoreo.dev/namespace": "acme", "openchoreo.dev/environment": "dev"}
	tests := []struct {
		name   string
		ns     string
		labels map[string]string
		port   int32
		want   bool
	}{
		{name: "same cell, project port", ns: np.Namespace, port: 9000, want: true},
		{name: "same cell, any declared port", ns: np.Namespace, port: 80, want: true},
		{name: "same cell, undeclared port", ns: np.Namespace, port: 5432},
		{name: "same org and env, namespace port", ns: "dp-acme-billing-dev-2", labels: sameOrg, port: 8080, want: true},
		{name: "same org and env, project-only port", ns: "dp-acme-billing-dev-2", labels: sameOrg, port: 9000},
		{name: "same org and env, external port needs a gateway pod", ns: "dp-acme-billing-dev-2", labels: sameOrg, port: 80},
		{name: "other env", ns: "dp-acme-shop-prod-3", port: 8080,
			labels: map[string]string{"openchoreo.dev/namespace": "acme", "openchoreo.dev/environment": "prod"}},
		{name: "other org", ns: "dp-x-shop-dev-4", port: 8080,
			labels: map[string]string{"openchoreo.dev/namespace": "x", "openchoreo.dev/environment": "dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Admits(&np, tt.ns, tt.labels, tt.port); got != tt.want {
				t.Fatalf("Admits = %v, want %v", got, tt.want)
			}
		})
	}

	if !AdmitsCell(&np, np.Namespace, nil) || !AdmitsCell(&np, "dp-acme-billing-dev-2", sameOrg) ||
		AdmitsCell(&np, "dp-x-shop-dev-4", map[string]string{"openchoreo.dev/namespace": "x"}) {
		t.Fatal("AdmitsCell disagrees with the policy")
	}

	deny := networkingv1.NetworkPolicy{}
	deny.Namespace = np.Namespace
	if Admits(&deny, np.Namespace, nil, 80) {
		t.Fatal("a policy without ingress rules denies everything")
	}
}

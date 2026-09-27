// Package visibility decides whether an actor may call a component, by
// evaluating the target's generated openchoreo-<component> NetworkPolicy as
// Kubernetes would for a pod in the actor's cell. Actors are not pods, so a
// peer that needs specific pod labels (the gateway rule) never admits them.
package visibility

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func PolicyName(component string) string { return "openchoreo-" + component }

// Admits reports whether policy lets a caller in callerNamespace (with the
// given namespace labels) reach port over TCP.
func Admits(policy *networkingv1.NetworkPolicy, callerNamespace string, callerNamespaceLabels map[string]string, port int32) bool {
	for _, rule := range policy.Spec.Ingress {
		if !portMatches(rule.Ports, port) {
			continue
		}
		if len(rule.From) == 0 {
			return true
		}
		for _, peer := range rule.From {
			if peerAdmits(peer, policy.Namespace, callerNamespace, callerNamespaceLabels) {
				return true
			}
		}
	}
	return false
}

func portMatches(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Protocol != nil && *p.Protocol != "TCP" {
			continue
		}
		if p.Port == nil {
			return true
		}
		if p.Port.IntValue() == int(port) || (p.EndPort != nil && p.Port.IntValue() <= int(port) && int(port) <= int(*p.EndPort)) {
			return true
		}
	}
	return false
}

func peerAdmits(peer networkingv1.NetworkPolicyPeer, policyNamespace, callerNamespace string, nsLabels map[string]string) bool {
	if peer.IPBlock != nil {
		return false
	}
	if peer.PodSelector != nil && !empty(peer.PodSelector) {
		return false
	}
	if peer.NamespaceSelector == nil {
		return callerNamespace == policyNamespace
	}
	sel, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
	return err == nil && sel.Matches(labels.Set(nsLabels))
}

func empty(s *metav1.LabelSelector) bool {
	return len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
}

// Package reach computes where an actor may send traffic so that it behaves
// like a pod in its cell. OpenChoreo restricts nothing on egress; a pod is
// stopped only by a target's openchoreo-<component> ingress policy. Actor
// traffic leaves through Substrate's shared egress pod, so targets cannot tell
// actors apart, and the same answer is computed at the source instead.
package reach

import (
	"net/netip"
	"slices"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/rashadism/oc-substrate/internal/visibility"
)

// Blocked is never reachable. Link-local covers cloud metadata servers, which
// would answer with the shared egress pod's identity rather than the actor's.
var Blocked = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("::1/128"),
}

type Pod struct {
	Namespace string
	Labels    map[string]string
	IPs       []netip.Addr
}

type Service struct {
	Namespace string
	Name      string
	Selector  map[string]string
	IPs       []netip.Addr
}

type Snapshot struct {
	// ClusterRanges are the pod and Service ranges. Everything outside them
	// (the internet, nodes, peered networks) is open, as for a pod.
	ClusterRanges   []netip.Prefix
	Pods            []Pod
	Services        []Service
	Policies        []*networkingv1.NetworkPolicy
	NamespaceLabels map[string]map[string]string
}

const policyPrefix = "openchoreo-"

// Allowed returns the destinations a pod in cell could reach. Inside the
// cluster ranges it lists what is allowed, so a pod or Service created since
// the snapshot stays unreachable until the next one.
func (s *Snapshot) Allowed(cell string) []netip.Prefix {
	cellLabels := s.NamespaceLabels[cell]
	type policy struct {
		sel    labels.Selector
		admits bool
	}
	byNS := map[string][]policy{}
	named := map[string]bool{}
	for _, np := range s.Policies {
		if !strings.HasPrefix(np.Name, policyPrefix) {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
		if err != nil {
			continue
		}
		admits := visibility.AdmitsCell(np, cell, cellLabels)
		byNS[np.Namespace] = append(byNS[np.Namespace], policy{sel: sel, admits: admits})
		named[np.Namespace+"/"+strings.TrimPrefix(np.Name, policyPrefix)] = admits
	}

	// A pod selected by component policies is reachable only if one admits the cell.
	reachable := func(ns string, podLabels map[string]string) bool {
		selected := false
		for _, p := range byNS[ns] {
			if p.sel.Matches(labels.Set(podLabels)) {
				if p.admits {
					return true
				}
				selected = true
			}
		}
		return !selected
	}

	var inside []netip.Addr
	for _, p := range s.Pods {
		if reachable(p.Namespace, p.Labels) {
			inside = append(inside, p.IPs...)
		}
	}
	for _, svc := range s.Services {
		if admits, ok := named[svc.Namespace+"/"+svc.Name]; ok && !admits {
			continue
		}
		if len(svc.Selector) > 0 && !s.selectsOnlyReachable(svc, reachable) {
			continue
		}
		inside = append(inside, svc.IPs...)
	}

	holes := make([]netip.Prefix, 0, len(s.ClusterRanges)+len(Blocked))
	holes = append(holes, s.ClusterRanges...)
	holes = append(holes, Blocked...)
	out := subtract(netip.MustParsePrefix("0.0.0.0/0"), holes)
	out = append(out, subtract(netip.MustParsePrefix("::/0"), holes)...)
	for _, a := range inside {
		if s.inCluster(a) && !blocked(a) {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return slices.Compact(out)
}

func (s *Snapshot) selectsOnlyReachable(svc Service, reachable func(string, map[string]string) bool) bool {
	sel := labels.SelectorFromSet(svc.Selector)
	for _, p := range s.Pods {
		if p.Namespace == svc.Namespace && sel.Matches(labels.Set(p.Labels)) && !reachable(p.Namespace, p.Labels) {
			return false
		}
	}
	return true
}

func (s *Snapshot) inCluster(a netip.Addr) bool {
	for _, r := range s.ClusterRanges {
		if r.Contains(a) {
			return true
		}
	}
	return false
}

func blocked(a netip.Addr) bool {
	for _, b := range Blocked {
		if b.Contains(a) {
			return true
		}
	}
	return false
}

// subtract returns p minus holes as a minimal set of prefixes.
func subtract(p netip.Prefix, holes []netip.Prefix) []netip.Prefix {
	overlapping := false
	for _, h := range holes {
		if h.Addr().Is4() != p.Addr().Is4() || !h.Overlaps(p) {
			continue
		}
		if h.Bits() <= p.Bits() {
			return nil
		}
		overlapping = true
	}
	if !overlapping {
		return []netip.Prefix{p}
	}
	lo, hi := split(p)
	return append(subtract(lo, holes), subtract(hi, holes)...)
}

func split(p netip.Prefix) (netip.Prefix, netip.Prefix) {
	bits := p.Bits() + 1
	lo := netip.PrefixFrom(p.Addr(), bits)
	b := p.Addr().AsSlice()
	b[(bits-1)/8] |= 0x80 >> ((bits - 1) % 8)
	hiAddr, _ := netip.AddrFromSlice(b)
	return lo, netip.PrefixFrom(hiAddr, bits)
}

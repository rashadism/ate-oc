package compile

import (
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

// Egress compiles an Actor's extra egress into Substrate rules. It returns nil
// when nothing is allowed, since no policy means deny-all. Wildcards must sit
// under a registrable name, and CIDRs must be single public hosts: cluster and
// private destinations are reached through declared dependencies instead.
func Egress(e *v1alpha1.ExtraEgress) ([]*pb.EgressRule, error) {
	if e == nil {
		return nil, nil
	}
	var rules []*pb.EgressRule
	if len(e.Hostnames) > 0 {
		for _, h := range e.Hostnames {
			if err := checkHostname(h); err != nil {
				return nil, err
			}
		}
		rules = append(rules, &pb.EgressRule{Hostnames: &pb.HostnameRule{Patterns: e.Hostnames}})
	}
	if len(e.CIDRs) > 0 {
		cidrs := make([]string, 0, len(e.CIDRs))
		for _, c := range e.CIDRs {
			p, err := checkCIDR(c)
			if err != nil {
				return nil, err
			}
			cidrs = append(cidrs, p)
		}
		rules = append(rules, &pb.EgressRule{Cidrs: &pb.CIDRRule{Cidrs: cidrs}})
	}
	return rules, nil
}

func checkHostname(h string) error {
	name := h
	if rest, ok := strings.CutPrefix(h, "*."); ok {
		if strings.Count(rest, ".") < 1 {
			return fmt.Errorf("egress hostname %q: wildcard is too broad", h)
		}
		name = rest
	}
	if strings.Contains(name, "*") || strings.ToLower(name) != name {
		return fmt.Errorf("egress hostname %q: must be lowercase with at most a leading \"*.\"", h)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("egress hostname %q: %s", h, strings.Join(errs, "; "))
	}
	if strings.HasSuffix(name, ".svc") || strings.Contains(name, ".svc.") || strings.HasSuffix(name, ".local") {
		return fmt.Errorf("egress hostname %q: cluster names are reached through dependencies", h)
	}
	return nil
}

func checkCIDR(c string) (string, error) {
	p, err := netip.ParsePrefix(c)
	if err != nil {
		addr, aerr := netip.ParseAddr(c)
		if aerr != nil {
			return "", fmt.Errorf("egress cidr %q: %w", c, err)
		}
		p = netip.PrefixFrom(addr, addr.BitLen())
	}
	a := p.Addr()
	if p.Bits() != a.BitLen() {
		return "", fmt.Errorf("egress cidr %q: only single hosts (/32, /128) are allowed", c)
	}
	if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() ||
		a.IsMulticast() || !a.IsGlobalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(a) {
		return "", fmt.Errorf("egress cidr %q: private and cluster addresses are reached through dependencies", c)
	}
	return p.String(), nil
}

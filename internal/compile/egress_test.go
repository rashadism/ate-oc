package compile

import (
	"strings"
	"testing"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
)

func TestEgress(t *testing.T) {
	tests := []struct {
		name      string
		in        *v1alpha1.ExtraEgress
		wantRules int
		wantCIDR  string
		wantErr   string
	}{
		{name: "nil", in: nil},
		{name: "empty", in: &v1alpha1.ExtraEgress{}},
		{name: "hostnames", in: &v1alpha1.ExtraEgress{Hostnames: []string{"api.example.com", "*.example.com"}}, wantRules: 1},
		{name: "bare ip becomes /32", in: &v1alpha1.ExtraEgress{CIDRs: []string{"8.8.8.8"}}, wantRules: 1, wantCIDR: "8.8.8.8/32"},
		{name: "both", in: &v1alpha1.ExtraEgress{Hostnames: []string{"a.example.com"}, CIDRs: []string{"2001:4860:4860::8888/128"}}, wantRules: 2},
		{name: "tld wildcard", in: &v1alpha1.ExtraEgress{Hostnames: []string{"*.com"}}, wantErr: "too broad"},
		{name: "match all", in: &v1alpha1.ExtraEgress{Hostnames: []string{"*"}}, wantErr: "hostname"},
		{name: "inner wildcard", in: &v1alpha1.ExtraEgress{Hostnames: []string{"a.*.example.com"}}, wantErr: "leading"},
		{name: "uppercase", in: &v1alpha1.ExtraEgress{Hostnames: []string{"API.example.com"}}, wantErr: "lowercase"},
		{name: "cluster name", in: &v1alpha1.ExtraEgress{Hostnames: []string{"db.dp-cell.svc.cluster.local"}}, wantErr: "dependencies"},
		{name: "range", in: &v1alpha1.ExtraEgress{CIDRs: []string{"8.8.8.0/24"}}, wantErr: "single hosts"},
		{name: "private", in: &v1alpha1.ExtraEgress{CIDRs: []string{"10.0.0.5/32"}}, wantErr: "private"},
		{name: "metadata server", in: &v1alpha1.ExtraEgress{CIDRs: []string{"169.254.169.254"}}, wantErr: "private"},
		{name: "cgnat", in: &v1alpha1.ExtraEgress{CIDRs: []string{"100.64.1.1"}}, wantErr: "private"},
		{name: "garbage", in: &v1alpha1.ExtraEgress{CIDRs: []string{"nope"}}, wantErr: "cidr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := Egress(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rules) != tt.wantRules {
				t.Fatalf("rules = %d, want %d", len(rules), tt.wantRules)
			}
			if tt.wantCIDR != "" && rules[0].GetCidrs().GetCidrs()[0] != tt.wantCIDR {
				t.Fatalf("cidr = %v", rules[0].GetCidrs().GetCidrs())
			}
		})
	}
}

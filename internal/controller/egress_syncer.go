package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/reach"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const (
	CondEgress = "Egress"

	maxRulesPerPolicy = 256
	maxCIDRsPerRule   = 256
	// Recheck policies Substrate holds every so often, in case they were changed out of band.
	egressRecheckCycles = 60
)

// EgressSyncer keeps every actor's Substrate EgressPolicy equal to what a pod
// in its cell could reach (see package reach). Actors without a policy are
// denied everything, so a new actor is closed until its first sync.
type EgressSyncer struct {
	client.Client
	Ate      pb.ControlClient
	Interval time.Duration
	// ExtraClusterRanges adds pod or Service ranges the cluster does not report.
	ExtraClusterRanges []netip.Prefix
	// Blocked is never reachable from actors.
	Blocked []netip.Prefix

	applied map[string]string
	cycles  int
}

var _ manager.Runnable = &EgressSyncer{}

// +kubebuilder:rbac:groups="",resources=pods;services;namespaces;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;servicecidrs,verbs=get;list;watch

func (s *EgressSyncer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("egress-syncer")
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil {
			log.Error(err, "egress sync failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (s *EgressSyncer) Sync(ctx context.Context) error {
	if s.applied == nil || s.cycles%egressRecheckCycles == 0 {
		s.applied = map[string]string{}
	}
	s.cycles++

	snap, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	var actors substratev1alpha1.ActorList
	if err := s.List(ctx, &actors); err != nil {
		return err
	}
	perCell := map[string][]*pb.EgressRule{}
	perCellErr := map[string]error{}
	seen := map[string]bool{}
	var errs []error
	for i := range actors.Items {
		act := &actors.Items[i]
		if act.Status.Name == "" || !act.DeletionTimestamp.IsZero() {
			continue
		}
		key := act.Namespace + "/" + act.Status.Name
		seen[key] = true
		rules, ok := perCell[act.Namespace]
		if !ok {
			rules, perCellErr[act.Namespace] = pack(snap.Allowed(act.Namespace))
			perCell[act.Namespace] = rules
		}
		err := perCellErr[act.Namespace]
		if err == nil {
			h := hashRules(rules)
			if s.applied[key] == h {
				continue
			}
			if err = s.apply(ctx, actorRef(act.Namespace, act.Status.Name), rules); err == nil {
				s.applied[key] = h
			}
		}
		if cerr := s.setCondition(ctx, act, err); cerr != nil {
			errs = append(errs, cerr)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	for key := range s.applied {
		if !seen[key] {
			delete(s.applied, key)
		}
	}
	return errors.Join(errs...)
}

func (s *EgressSyncer) snapshot(ctx context.Context) (*reach.Snapshot, error) {
	snap := &reach.Snapshot{NamespaceLabels: map[string]map[string]string{}, Blocked: s.Blocked}
	snap.ClusterRanges = append(snap.ClusterRanges, s.ExtraClusterRanges...)

	var nodes corev1.NodeList
	if err := s.List(ctx, &nodes); err != nil {
		return nil, err
	}
	for _, n := range nodes.Items {
		snap.ClusterRanges = appendPrefixes(snap.ClusterRanges, n.Spec.PodCIDRs)
	}
	var cidrs networkingv1.ServiceCIDRList
	if err := s.List(ctx, &cidrs); err != nil && !meta.IsNoMatchError(err) && !runtime.IsNotRegisteredError(err) {
		return nil, err
	}
	for _, c := range cidrs.Items {
		snap.ClusterRanges = appendPrefixes(snap.ClusterRanges, c.Spec.CIDRs)
	}
	if len(snap.ClusterRanges) == 0 {
		// Without the cluster's ranges everything would count as internet.
		return nil, errors.New("no pod or Service ranges found; set --cluster-cidrs")
	}

	var pods corev1.PodList
	if err := s.List(ctx, &pods); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		if p.Spec.HostNetwork || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		rp := reach.Pod{Namespace: p.Namespace, Labels: p.Labels}
		for _, ip := range p.Status.PodIPs {
			if a, err := netip.ParseAddr(ip.IP); err == nil {
				rp.IPs = append(rp.IPs, a)
			}
		}
		snap.Pods = append(snap.Pods, rp)
	}
	var svcs corev1.ServiceList
	if err := s.List(ctx, &svcs); err != nil {
		return nil, err
	}
	for _, svc := range svcs.Items {
		rs := reach.Service{Namespace: svc.Namespace, Name: svc.Name, Selector: svc.Spec.Selector}
		for _, ip := range svc.Spec.ClusterIPs {
			if a, err := netip.ParseAddr(ip); err == nil {
				rs.IPs = append(rs.IPs, a)
			}
		}
		snap.Services = append(snap.Services, rs)
	}
	var nps networkingv1.NetworkPolicyList
	if err := s.List(ctx, &nps); err != nil {
		return nil, err
	}
	for i := range nps.Items {
		snap.Policies = append(snap.Policies, &nps.Items[i])
	}
	var nss corev1.NamespaceList
	if err := s.List(ctx, &nss); err != nil {
		return nil, err
	}
	for _, n := range nss.Items {
		snap.NamespaceLabels[n.Name] = n.Labels
	}
	return snap, nil
}

func appendPrefixes(out []netip.Prefix, cidrs []string) []netip.Prefix {
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func pack(prefixes []netip.Prefix) ([]*pb.EgressRule, error) {
	var rules []*pb.EgressRule
	for start := 0; start < len(prefixes); start += maxCIDRsPerRule {
		end := min(start+maxCIDRsPerRule, len(prefixes))
		cidrs := make([]string, 0, end-start)
		for _, p := range prefixes[start:end] {
			cidrs = append(cidrs, p.String())
		}
		rules = append(rules, &pb.EgressRule{Cidrs: &pb.CIDRRule{Cidrs: cidrs}})
	}
	if len(rules) > maxRulesPerPolicy {
		return nil, fmt.Errorf("reachable set needs %d prefixes, more than a policy holds (%d)",
			len(prefixes), maxRulesPerPolicy*maxCIDRsPerRule)
	}
	return rules, nil
}

func hashRules(rules []*pb.EgressRule) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&pb.EgressPolicy{Rules: rules})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *EgressSyncer) apply(ctx context.Context, ref *pb.ObjectRef, rules []*pb.EgressRule) error {
	cur, err := s.Ate.GetActorEgressPolicy(ctx, &pb.GetActorEgressPolicyRequest{Actor: ref})
	switch {
	case isCode(err, codes.NotFound):
		_, err = s.Ate.CreateActorEgressPolicy(ctx, &pb.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &pb.EgressPolicy{
			Metadata: &pb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: "default"},
			Rules:    rules,
		}})
		return err
	case err != nil:
		return err
	}
	if proto.Equal(&pb.EgressPolicy{Rules: cur.GetRules()}, &pb.EgressPolicy{Rules: rules}) {
		return nil
	}
	cur.Rules = rules
	_, err = s.Ate.UpdateActorEgressPolicy(ctx, &pb.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: cur})
	return err
}

func (s *EgressSyncer) setCondition(ctx context.Context, act *substratev1alpha1.Actor, err error) error {
	c := metav1.Condition{Type: CondEgress, Status: metav1.ConditionTrue, Reason: "MirrorsCell",
		Message: "egress matches a pod in this cell", ObservedGeneration: act.Generation}
	if err != nil {
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "SyncFailed", err.Error()
	}
	if cur := meta.FindStatusCondition(act.Status.Conditions, CondEgress); cur != nil &&
		cur.Status == c.Status && cur.Reason == c.Reason && cur.Message == c.Message {
		return nil
	}
	orig := act.DeepCopy()
	meta.SetStatusCondition(&act.Status.Conditions, c)
	return client.IgnoreNotFound(s.Status().Patch(ctx, act, client.MergeFrom(orig)))
}

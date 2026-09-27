package frontdoor

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
)

const (
	podIPIndex        = "status.podIP"
	actorServiceIndex = "spec.serviceName"
)

// ClusterDirectory answers Directory lookups from informer caches.
type ClusterDirectory struct {
	Reader client.Reader
	// EgressNamespace and EgressLabels identify Substrate's egress gateway pods.
	EgressNamespace string
	EgressLabels    map[string]string
}

// Indexes registers the lookups ClusterDirectory needs on c.
func Indexes(ctx context.Context, c cache.Cache) error {
	if err := c.IndexField(ctx, &corev1.Pod{}, podIPIndex, func(o client.Object) []string {
		p := o.(*corev1.Pod)
		if p.Spec.HostNetwork || p.Status.PodIP == "" {
			return nil
		}
		ips := make([]string, 0, len(p.Status.PodIPs))
		for _, ip := range p.Status.PodIPs {
			ips = append(ips, ip.IP)
		}
		return ips
	}); err != nil {
		return err
	}
	return c.IndexField(ctx, &v1alpha1.Actor{}, actorServiceIndex, func(o client.Object) []string {
		if s := o.(*v1alpha1.Actor).Spec.ServiceName; s != "" {
			return []string{s}
		}
		return nil
	})
}

// TrimPod keeps only what caller identification reads, to bound cache memory.
func TrimPod(o any) (any, error) {
	p, ok := o.(*corev1.Pod)
	if !ok {
		return o, nil
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels,
			ResourceVersion: p.ResourceVersion, UID: p.UID},
		Spec:   corev1.PodSpec{HostNetwork: p.Spec.HostNetwork},
		Status: corev1.PodStatus{PodIP: p.Status.PodIP, PodIPs: p.Status.PodIPs, Phase: p.Status.Phase},
	}, nil
}

func (d *ClusterDirectory) Caller(ip string) Caller {
	var pods corev1.PodList
	if err := d.Reader.List(context.Background(), &pods, client.MatchingFields{podIPIndex: ip}); err != nil {
		return Caller{}
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if p.Namespace == d.EgressNamespace && matches(p.Labels, d.EgressLabels) {
			return Caller{Kind: CallerEgress, Namespace: p.Namespace}
		}
		_, system := p.Labels[LabelSystemComponent]
		return Caller{Kind: CallerPod, Namespace: p.Namespace, SystemComponent: system}
	}
	return Caller{}
}

func (d *ClusterDirectory) Target(namespace, service string, port int32) (Target, bool) {
	var actors v1alpha1.ActorList
	if err := d.Reader.List(context.Background(), &actors, client.InNamespace(namespace),
		client.MatchingFields{actorServiceIndex: service}); err != nil {
		return Target{}, false
	}
	for _, a := range actors.Items {
		for _, ep := range a.Spec.Endpoints {
			if ep.Port != port {
				continue
			}
			return Target{
				Namespace:  namespace,
				Service:    service,
				Port:       port,
				Atespace:   namespace,
				Actor:      a.Status.Name,
				ActorPort:  ep.ActorPort(),
				Visibility: ep.Visibility,
				Ready:      a.Status.Name != "" && a.Status.UID != "",
				Paused:     a.Spec.Paused,
			}, true
		}
	}
	return Target{}, false
}

func (d *ClusterDirectory) NamespaceLabels(namespace string) map[string]string {
	var ns corev1.Namespace
	if err := d.Reader.Get(context.Background(), types.NamespacedName{Name: namespace}, &ns); err != nil {
		return nil
	}
	return ns.Labels
}

func matches(labels, want map[string]string) bool {
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Package compile turns an ActorTemplate CR into the Substrate ActorTemplate
// it describes, resolving Secret and ConfigMap references from the CR's namespace.
package compile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const durableVolume = "durable"

// RefError marks a missing Secret, ConfigMap or key, which may resolve later.
type RefError struct{ msg string }

func (e *RefError) Error() string { return e.msg }

type Options struct {
	// StorageLocation is the object-store prefix for this actor's snapshots.
	StorageLocation string
	// SandboxConfigName overrides spec.sandboxConfigName, pinning an existing
	// actor to the runtime its snapshots were taken with.
	SandboxConfigName string
}

type Result struct {
	Template *pb.ActorTemplate
	// Hash covers everything that affects the running sandbox, including
	// resolved secret values and the SandboxConfig name.
	Hash string
}

func Compile(ctx context.Context, r client.Reader, at *v1alpha1.ActorTemplate, opts Options) (*Result, error) {
	id, err := naming.IdentityFromLabels(at.Labels)
	if err != nil {
		return nil, err
	}
	spec := at.Spec

	sel, err := selector(spec)
	if err != nil {
		return nil, err
	}
	var volumes []*pb.Volume
	var mounts []*pb.VolumeMount
	if spec.DurableDir != nil {
		p := spec.DurableDir.MountPath
		if p == "/" || path.Clean(p) != p {
			return nil, fmt.Errorf("durableDir.mountPath %q must be a clean absolute path other than /", p)
		}
		volumes = []*pb.Volume{{Name: durableVolume, DurableDir: &pb.DurableDirVolumeSource{}}}
		mounts = []*pb.VolumeMount{{Name: durableVolume, MountPath: p}}
	}

	refs := &resolver{r: r, ns: at.Namespace}
	containers := make([]*pb.Container, 0, len(spec.Containers))
	for _, c := range spec.Containers {
		pc := &pb.Container{
			Name:         c.Name,
			Image:        c.Image,
			Command:      c.Command,
			Args:         c.Args,
			VolumeMounts: mounts,
		}
		env, err := refs.env(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("container %s: %w", c.Name, err)
		}
		pc.Env = env
		if p := c.WakeupProbe; p != nil {
			pc.WakeupProbe = &pb.ContainerWakeupProbe{
				HttpGet:        &pb.HTTPGetAction{Path: p.HTTPGet.Path, Port: p.HTTPGet.Port},
				TimeoutSeconds: p.TimeoutSeconds,
			}
		}
		if len(c.Capabilities) > 0 {
			add := make([]string, len(c.Capabilities))
			for i, cap := range c.Capabilities {
				add[i] = string(cap)
			}
			pc.SecurityContext = &pb.SecurityContext{Capabilities: &pb.Capabilities{Add: add}}
		}
		containers = append(containers, pc)
	}

	t := &pb.ActorTemplate{
		Metadata:       &pb.ResourceMetadata{Atespace: at.Namespace},
		WorkerSelector: sel,
		Containers:     containers,
		Volumes:        volumes,
		SnapshotConfig: snapshot(spec.Snapshot, opts.StorageLocation),
		SandboxConfig:  &pb.SandboxConfig{SandboxClass: class(spec.SandboxClass), ConfigName: sandboxConfigName(spec, opts)},
		Resources:      limits(spec.Resources),
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(t)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	t.Metadata.Name = id.TemplateRevisionName(hash)
	return &Result{Template: t, Hash: hash}, nil
}

func sandboxConfigName(spec v1alpha1.ActorTemplateSpec, opts Options) string {
	if opts.SandboxConfigName != "" {
		return opts.SandboxConfigName
	}
	return spec.SandboxConfigName
}

func selector(spec v1alpha1.ActorTemplateSpec) (*pb.Selector, error) {
	if spec.WorkerSelector == nil {
		return nil, nil
	}
	if len(spec.WorkerSelector.MatchExpressions) > 0 {
		return nil, errors.New("workerSelector.matchExpressions is not supported; use matchLabels")
	}
	return &pb.Selector{MatchLabels: spec.WorkerSelector.MatchLabels}, nil
}

func class(c v1alpha1.SandboxClass) pb.SandboxClass {
	if c == v1alpha1.SandboxClassMicroVM {
		return pb.SandboxClass_SANDBOX_CLASS_MICROVM
	}
	return pb.SandboxClass_SANDBOX_CLASS_GVISOR
}

func snapshot(p *v1alpha1.SnapshotPolicy, storage string) *pb.SnapshotConfig {
	if p == nil {
		p = &v1alpha1.SnapshotPolicy{}
	}
	scope := func(s, def v1alpha1.SnapshotScope) pb.SnapshotContentScope {
		if s == "" {
			s = def
		}
		if s == v1alpha1.SnapshotScopeData {
			return pb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA
		}
		return pb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	}
	from := pb.ResumeSource_RESUME_SOURCE_GOLDEN
	if p.OnResumeFromData == v1alpha1.ResumeSourceColdBoot {
		from = pb.ResumeSource_RESUME_SOURCE_COLD_BOOT
	}
	return &pb.SnapshotConfig{
		OnPause:         scope(p.OnPause, v1alpha1.SnapshotScopeFull),
		OnCommit:        scope(p.OnCommit, v1alpha1.SnapshotScopeData),
		OnResume:        &pb.OnResumeConfig{FromData: from},
		StorageLocation: storage,
	}
}

func limits(r v1alpha1.ActorResources) *pb.Resources {
	names := make([]string, 0, len(r.Limits))
	for n := range r.Limits {
		names = append(names, string(n))
	}
	slices.Sort(names)
	out := &pb.Resources{}
	for _, n := range names {
		q := r.Limits[corev1.ResourceName(n)]
		out.Limits = append(out.Limits, &pb.Limits{Name: n, Quantity: q.String()})
	}
	return out
}

type resolver struct {
	r  client.Reader
	ns string
}

// env resolves a container's variables as the kubelet would: envFrom sources
// in order, then env entries, a later definition of a name replacing an
// earlier one in place. Keys from each source are taken in sorted order.
func (res *resolver) env(ctx context.Context, c v1alpha1.Container) ([]*pb.EnvVar, error) {
	var out []*pb.EnvVar
	index := map[string]int{}
	set := func(name, value string) {
		if i, ok := index[name]; ok {
			out[i].Value = value
			return
		}
		index[name] = len(out)
		out = append(out, &pb.EnvVar{Name: name, Value: value})
	}
	for _, src := range c.EnvFrom {
		data, err := res.all(ctx, src)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			set(k, data[k])
		}
	}
	for _, e := range c.Env {
		v, err := res.value(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", e.Name, err)
		}
		set(e.Name, v)
	}
	return out, nil
}

func (res *resolver) all(ctx context.Context, src v1alpha1.EnvFromSource) (map[string]string, error) {
	if src.SecretRef != nil {
		var s corev1.Secret
		if err := res.get(ctx, "Secret", src.SecretRef.Name, &s); err != nil {
			return nil, err
		}
		out := make(map[string]string, len(s.Data))
		for k, v := range s.Data {
			out[k] = string(v)
		}
		return out, nil
	}
	var cm corev1.ConfigMap
	if err := res.get(ctx, "ConfigMap", src.ConfigMapRef.Name, &cm); err != nil {
		return nil, err
	}
	return cm.Data, nil
}

func (res *resolver) value(ctx context.Context, e v1alpha1.EnvVar) (string, error) {
	if e.ValueFrom == nil {
		if e.Value == nil {
			return "", nil
		}
		return *e.Value, nil
	}
	if ref := e.ValueFrom.SecretKeyRef; ref != nil {
		var s corev1.Secret
		if err := res.get(ctx, "Secret", ref.Name, &s); err != nil {
			return "", err
		}
		v, ok := s.Data[ref.Key]
		if !ok {
			return "", &RefError{fmt.Sprintf("key %q not found in Secret %s", ref.Key, ref.Name)}
		}
		return string(v), nil
	}
	ref := e.ValueFrom.ConfigMapKeyRef
	var cm corev1.ConfigMap
	if err := res.get(ctx, "ConfigMap", ref.Name, &cm); err != nil {
		return "", err
	}
	v, ok := cm.Data[ref.Key]
	if !ok {
		return "", &RefError{fmt.Sprintf("key %q not found in ConfigMap %s", ref.Key, ref.Name)}
	}
	return v, nil
}

func (res *resolver) get(ctx context.Context, kind, name string, obj client.Object) error {
	err := res.r.Get(ctx, types.NamespacedName{Namespace: res.ns, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return &RefError{fmt.Sprintf("%s %s not found", kind, name)}
	}
	return err
}

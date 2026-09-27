package compile

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const ns = "dp-cell"

func baseTemplate() *v1alpha1.ActorTemplate {
	return &v1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns, Labels: map[string]string{
			naming.LabelComponentUID:   "comp-uid",
			naming.LabelEnvironmentUID: "env-uid",
		}},
		Spec: v1alpha1.ActorTemplateSpec{
			SandboxClass:      v1alpha1.SandboxClassGVisor,
			SandboxConfigName: "gvisor-v1",
			WorkerSelector:    &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "default"}},
			Resources: v1alpha1.ActorResources{Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
				corev1.ResourceCPU:    resource.MustParse("1"),
			}},
			DurableDir: &v1alpha1.DurableDir{MountPath: "/data"},
			Containers: []v1alpha1.Container{{
				Name:  "main",
				Image: "ghcr.io/x/app:1",
				Env: []v1alpha1.EnvVar{
					{Name: "PLAIN", Value: ptr.To("v")},
					{Name: "SECRET", ValueFrom: &v1alpha1.EnvVarSource{SecretKeyRef: &v1alpha1.KeyRef{Name: "creds", Key: "token"}}},
					{Name: "CONF", ValueFrom: &v1alpha1.EnvVarSource{ConfigMapKeyRef: &v1alpha1.KeyRef{Name: "conf", Key: "level"}}},
				},
				WakeupProbe:  &v1alpha1.WakeupProbe{HTTPGet: v1alpha1.HTTPGetAction{Path: "/healthz", Port: 8080}},
				Capabilities: []corev1.Capability{"CHOWN"},
			}},
		},
	}
}

func objects() []client.Object {
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns}, Data: map[string][]byte{"token": []byte("s3cret")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "conf", Namespace: ns}, Data: map[string]string{"level": "info"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "other"}, Data: map[string][]byte{"token": []byte("x")}},
	}
}

func reader(objs ...client.Object) client.Reader {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func compile(t *testing.T, at *v1alpha1.ActorTemplate, objs ...client.Object) (*Result, error) {
	t.Helper()
	return Compile(context.Background(), reader(objs...), at, Options{StorageLocation: "s3://snap/dp-cell"})
}

func TestCompileMapping(t *testing.T) {
	res, err := compile(t, baseTemplate(), objects()...)
	if err != nil {
		t.Fatal(err)
	}
	tpl := res.Template
	id := naming.Identity{ComponentUID: "comp-uid", EnvironmentUID: "env-uid"}

	if got, want := tpl.GetMetadata().GetName(), id.TemplateRevisionName(res.Hash); got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if tpl.GetMetadata().GetAtespace() != ns {
		t.Errorf("atespace = %q", tpl.GetMetadata().GetAtespace())
	}
	c := tpl.GetContainers()[0]
	env := map[string]string{}
	for _, e := range c.GetEnv() {
		env[e.GetName()] = e.GetValue()
	}
	if env["PLAIN"] != "v" || env["SECRET"] != "s3cret" || env["CONF"] != "info" {
		t.Errorf("env = %v", env)
	}
	if c.GetWakeupProbe().GetHttpGet().GetPort() != 8080 || c.GetWakeupProbe().GetHttpGet().GetPath() != "/healthz" {
		t.Errorf("wakeup probe = %v", c.GetWakeupProbe())
	}
	if got := c.GetSecurityContext().GetCapabilities().GetAdd(); len(got) != 1 || got[0] != "CHOWN" {
		t.Errorf("capabilities = %v", got)
	}
	if len(tpl.GetVolumes()) != 1 || tpl.GetVolumes()[0].GetDurableDir() == nil ||
		c.GetVolumeMounts()[0].GetMountPath() != "/data" || c.GetVolumeMounts()[0].GetName() != tpl.GetVolumes()[0].GetName() {
		t.Errorf("durable dir not mapped: volumes=%v mounts=%v", tpl.GetVolumes(), c.GetVolumeMounts())
	}
	lim := tpl.GetResources().GetLimits()
	if len(lim) != 2 || lim[0].GetName() != "cpu" || lim[0].GetQuantity() != "1" || lim[1].GetName() != "memory" || lim[1].GetQuantity() != "512Mi" {
		t.Errorf("limits = %v", lim)
	}
	sc := tpl.GetSnapshotConfig()
	if sc.GetOnPause() != pb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL ||
		sc.GetOnCommit() != pb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA ||
		sc.GetOnResume().GetFromData() != pb.ResumeSource_RESUME_SOURCE_GOLDEN ||
		sc.GetStorageLocation() != "s3://snap/dp-cell" {
		t.Errorf("snapshot config = %v", sc)
	}
	if tpl.GetSandboxConfig().GetConfigName() != "gvisor-v1" || tpl.GetSandboxConfig().GetSandboxClass() != pb.SandboxClass_SANDBOX_CLASS_GVISOR {
		t.Errorf("sandbox config = %v", tpl.GetSandboxConfig())
	}
	if tpl.GetWorkerSelector().GetMatchLabels()["pool"] != "default" {
		t.Errorf("worker selector = %v", tpl.GetWorkerSelector())
	}
}

func TestCompileHash(t *testing.T) {
	base, err := compile(t, baseTemplate(), objects()...)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := compile(t, baseTemplate(), objects()...)
	if base.Hash != again.Hash {
		t.Fatal("hash is not deterministic")
	}

	relabeled := baseTemplate()
	relabeled.Name = "renamed"
	relabeled.Labels["extra"] = "x"
	if r, _ := compile(t, relabeled, objects()...); r.Hash != base.Hash {
		t.Error("CR name and unrelated labels must not change the hash")
	}

	rotated := objects()
	rotated[0].(*corev1.Secret).Data["token"] = []byte("rotated")

	tests := []struct {
		name   string
		mutate func(*v1alpha1.ActorTemplate)
		objs   []client.Object
	}{
		{name: "image", mutate: func(at *v1alpha1.ActorTemplate) { at.Spec.Containers[0].Image = "ghcr.io/x/app:2" }},
		{name: "sandbox config", mutate: func(at *v1alpha1.ActorTemplate) { at.Spec.SandboxConfigName = "gvisor-v2" }},
		{name: "limits", mutate: func(at *v1alpha1.ActorTemplate) {
			at.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
		}},
		{name: "snapshot policy", mutate: func(at *v1alpha1.ActorTemplate) {
			at.Spec.Snapshot = &v1alpha1.SnapshotPolicy{OnResumeFromData: v1alpha1.ResumeSourceColdBoot}
		}},
		{name: "secret value", objs: rotated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := baseTemplate()
			if tt.mutate != nil {
				tt.mutate(at)
			}
			objs := tt.objs
			if objs == nil {
				objs = objects()
			}
			r, err := compile(t, at, objs...)
			if err != nil {
				t.Fatal(err)
			}
			if r.Hash == base.Hash {
				t.Fatal("hash did not change")
			}
			if r.Template.GetMetadata().GetName() == base.Template.GetMetadata().GetName() {
				t.Fatal("revision name did not change")
			}
		})
	}
}

func TestCompileSandboxConfigOverride(t *testing.T) {
	base, _ := compile(t, baseTemplate(), objects()...)
	at := baseTemplate()
	at.Spec.SandboxConfigName = "gvisor-v2"
	res, err := Compile(context.Background(), reader(objects()...), at,
		Options{StorageLocation: "s3://snap/dp-cell", SandboxConfigName: "gvisor-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Template.GetSandboxConfig().GetConfigName() != "gvisor-v1" || res.Hash != base.Hash {
		t.Fatalf("override not applied: %v", res.Template.GetSandboxConfig())
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*v1alpha1.ActorTemplate)
		objs    []client.Object
		wantRef bool
		wantErr string
	}{
		{name: "missing uid label", mutate: func(at *v1alpha1.ActorTemplate) { delete(at.Labels, naming.LabelComponentUID) }, wantErr: "component-uid"},
		{name: "missing secret", objs: objects()[1:], wantRef: true, wantErr: "Secret creds not found"},
		{name: "missing configmap", objs: []client.Object{objects()[0]}, wantRef: true, wantErr: "ConfigMap conf not found"},
		{name: "missing key", mutate: func(at *v1alpha1.ActorTemplate) {
			at.Spec.Containers[0].Env[1].ValueFrom.SecretKeyRef.Key = "nope"
		}, wantRef: true, wantErr: `key "nope"`},
		{name: "match expressions", mutate: func(at *v1alpha1.ActorTemplate) {
			at.Spec.WorkerSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "k", Operator: metav1.LabelSelectorOpExists}}
		}, wantErr: "matchExpressions"},
		{name: "unclean mount path", mutate: func(at *v1alpha1.ActorTemplate) { at.Spec.DurableDir.MountPath = "/data/../etc" }, wantErr: "mountPath"},
		{name: "root mount path", mutate: func(at *v1alpha1.ActorTemplate) { at.Spec.DurableDir.MountPath = "/" }, wantErr: "mountPath"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := baseTemplate()
			if tt.mutate != nil {
				tt.mutate(at)
			}
			objs := tt.objs
			if objs == nil {
				objs = objects()
			}
			_, err := compile(t, at, objs...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			var refErr *RefError
			if errors.As(err, &refErr) != tt.wantRef {
				t.Fatalf("RefError = %v, want %v", errors.As(err, &refErr), tt.wantRef)
			}
		})
	}
}

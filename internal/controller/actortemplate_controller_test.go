package controller

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/fakeateapi"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const (
	ns     = "dp-cell"
	image2 = "ghcr.io/x/app:2"
)

var (
	key = types.NamespacedName{Namespace: ns, Name: "app"}
	id  = naming.Identity{ComponentUID: "comp-uid", EnvironmentUID: "env-uid"}
)

type env struct {
	t   *testing.T
	r   *ActorTemplateReconciler
	srv *fakeateapi.Server
	ate pb.ControlClient
}

// newEnv adds a WorkerPool the default template fits.
func newEnv(t *testing.T, golden bool, objs ...client.Object) *env {
	t.Helper()
	return build(t, golden, append(objs, workerPool("default", "4Gi"))...)
}

func build(t *testing.T, golden bool, objs ...client.Object) *env {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = substratev1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(workerPoolListGVK.GroupVersion().WithKind("WorkerPool"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(workerPoolListGVK, &unstructured.UnstructuredList{})

	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&substratev1alpha1.ActorTemplate{}).
		WithIndex(&substratev1alpha1.ActorTemplate{}, secretRefIndex, func(o client.Object) []string {
			return refNames(o.(*substratev1alpha1.ActorTemplate), true)
		}).
		WithIndex(&substratev1alpha1.ActorTemplate{}, configMapRefIndex, func(o client.Object) []string {
			return refNames(o.(*substratev1alpha1.ActorTemplate), false)
		}).
		WithObjects(objs...).Build()

	opts := []fakeateapi.Option{
		fakeateapi.WithSandboxConfig("gvisor-v1", pb.SandboxClass_SANDBOX_CLASS_GVISOR),
		fakeateapi.WithSandboxConfig("gvisor-v2", pb.SandboxClass_SANDBOX_CLASS_GVISOR),
	}
	if golden {
		opts = append(opts, fakeateapi.WithAutoGolden())
	}
	srv := fakeateapi.New(opts...)
	ate := fakeateapi.Start(t, srv)
	return &env{t: t, srv: srv, ate: ate, r: &ActorTemplateReconciler{Client: c, Scheme: s, Ate: ate, StorageLocation: "s3://snap/"}}
}

func actorTemplate() *substratev1alpha1.ActorTemplate {
	return &substratev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns, Labels: map[string]string{
			naming.LabelComponentUID:   id.ComponentUID,
			naming.LabelEnvironmentUID: id.EnvironmentUID,
		}},
		Spec: substratev1alpha1.ActorTemplateSpec{
			SandboxClass:      substratev1alpha1.SandboxClassGVisor,
			SandboxConfigName: "gvisor-v1",
			Resources: substratev1alpha1.ActorResources{Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			}},
			Containers: []substratev1alpha1.Container{{Name: "main", Image: "ghcr.io/x/app:1"}},
		},
	}
}

func workerPool(name, memLimit string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1alpha1",
		"kind":       "WorkerPool",
		"metadata":   map[string]any{"name": name, "namespace": "ate-system", "labels": map[string]any{"pool": name}},
		"spec": map[string]any{
			"replicas":    int64(2),
			"workerImage": "w",
			"template": map[string]any{
				"labels":    map[string]any{"ignored": "true"},
				"resources": map[string]any{"limits": map[string]any{"memory": memLimit}},
			},
		},
	}}
	return u
}

func (e *env) reconcile() (ctrl.Result, error) {
	e.t.Helper()
	return e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
}

func (e *env) get() *substratev1alpha1.ActorTemplate {
	e.t.Helper()
	var at substratev1alpha1.ActorTemplate
	if err := e.r.Get(context.Background(), key, &at); err != nil {
		e.t.Fatal(err)
	}
	return &at
}

func (e *env) update(mutate func(*substratev1alpha1.ActorTemplate)) {
	e.t.Helper()
	at := e.get()
	mutate(at)
	if err := e.r.Update(context.Background(), at); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) revisions() map[string]*pb.ActorTemplate {
	e.t.Helper()
	resp, err := e.ate.ListActorTemplates(context.Background(), &pb.ListActorTemplatesRequest{Atespace: ns})
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]*pb.ActorTemplate{}
	for _, t := range resp.GetActorTemplates() {
		out[t.GetMetadata().GetName()] = t
	}
	return out
}

func cond(at *substratev1alpha1.ActorTemplate, t string) metav1.Condition {
	if c := meta.FindStatusCondition(at.Status.Conditions, t); c != nil {
		return *c
	}
	return metav1.Condition{}
}

func wantCond(t *testing.T, at *substratev1alpha1.ActorTemplate, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := cond(at, typ)
	if c.Status != status || (reason != "" && c.Reason != reason) {
		t.Fatalf("%s = %s/%s (%s), want %s/%s", typ, c.Status, c.Reason, c.Message, status, reason)
	}
}

func TestCreatesRevisionAndWaitsForGolden(t *testing.T) {
	e := newEnv(t, false, actorTemplate())

	res, err := e.reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != goldenPoll {
		t.Fatalf("requeueAfter = %v, want golden poll", res.RequeueAfter)
	}
	at := e.get()
	wantCond(t, at, CondReady, metav1.ConditionFalse, "GoldenPending")
	wantCond(t, at, CondSchedulable, metav1.ConditionTrue, "")
	if at.Status.DesiredRevision == "" || at.Status.LatestReadyRevision != "" {
		t.Fatalf("status = %+v", at.Status)
	}
	if !slices.Contains(at.Finalizers, Finalizer) {
		t.Fatal("finalizer not added")
	}
	name := id.TemplateRevisionName(at.Status.DesiredRevision)
	rev, ok := e.revisions()[name]
	if !ok {
		t.Fatalf("revision %s not created", name)
	}
	if got := rev.GetSnapshotConfig().GetStorageLocation(); got != "s3://snap/"+ns {
		t.Fatalf("storage location = %q", got)
	}

	e.srv.SetGolden(ns, name, "")
	res, err = e.reconcile()
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	at = e.get()
	wantCond(t, at, CondReady, metav1.ConditionTrue, "GoldenReady")
	if at.Status.LatestReadyRevision != at.Status.DesiredRevision || !at.Status.Revisions[0].Ready {
		t.Fatalf("status = %+v", at.Status)
	}
}

func TestGoldenFailureDeletesRevisionAndRetries(t *testing.T) {
	e := newEnv(t, false, actorTemplate())
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	name := id.TemplateRevisionName(e.get().Status.DesiredRevision)
	e.srv.SetGolden(ns, name, "GoldenActorCrashed: exit 1")

	if _, err := e.reconcile(); err == nil {
		t.Fatal("want an error so the controller backs off")
	}
	at := e.get()
	wantCond(t, at, CondReady, metav1.ConditionFalse, "GoldenFailed")
	if at.Status.Revisions[0].Message == "" {
		t.Fatal("failure message not recorded")
	}
	if _, ok := e.revisions()[name]; ok {
		t.Fatal("failed revision not deleted")
	}

	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.revisions()[name]; !ok {
		t.Fatal("revision not recreated on retry")
	}
}

func TestSpecChangesKeepHistoryAndReferencedRevisions(t *testing.T) {
	e := newEnv(t, true, actorTemplate())
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	first := id.TemplateRevisionName(e.get().Status.DesiredRevision)
	if _, err := e.ate.CreateActor(context.Background(), &pb.CreateActorRequest{Actor: &pb.Actor{
		Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: id.ActorName()},
		ActorTemplate: &pb.ObjectRef{Atespace: ns, Name: first},
	}}); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, 5)
	for _, tag := range []string{"2", "3", "4", "5", "6"} {
		e.update(func(at *substratev1alpha1.ActorTemplate) { at.Spec.Containers[0].Image = "ghcr.io/x/app:" + tag })
		if _, err := e.reconcile(); err != nil {
			t.Fatal(err)
		}
		names = append(names, id.TemplateRevisionName(e.get().Status.DesiredRevision))
	}

	revs := e.revisions()
	for _, n := range append([]string{first}, names[2:]...) {
		if _, ok := revs[n]; !ok {
			t.Errorf("revision %s should be kept", n)
		}
	}
	for _, n := range names[:2] {
		if _, ok := revs[n]; ok {
			t.Errorf("revision %s should be collected", n)
		}
	}
	if got := len(e.get().Status.Revisions); got != 4 {
		t.Errorf("status revisions = %d, want 4 (3 recent + referenced)", got)
	}
}

func TestMissingIdentity(t *testing.T) {
	at := actorTemplate()
	delete(at.Labels, naming.LabelEnvironmentUID)
	e := newEnv(t, true, at)
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	wantCond(t, e.get(), CondAccepted, metav1.ConditionFalse, "MissingIdentity")
	if len(e.revisions()) != 0 {
		t.Fatal("no revision should be created")
	}
}

func TestUnresolvedSecret(t *testing.T) {
	at := actorTemplate()
	at.Spec.Containers[0].Env = []substratev1alpha1.EnvVar{{Name: "TOKEN", ValueFrom: &substratev1alpha1.EnvVarSource{
		SecretKeyRef: &substratev1alpha1.KeyRef{Name: "creds", Key: "token"},
	}}}
	e := newEnv(t, true, at)
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	wantCond(t, e.get(), CondResolvedRefs, metav1.ConditionFalse, "RefNotFound")
	if len(e.revisions()) != 0 {
		t.Fatal("no revision should be created")
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns}, Data: map[string][]byte{"token": []byte("x")}}
	if err := e.r.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if reqs := e.r.referencing(secretRefIndex)(context.Background(), secret); len(reqs) != 1 || reqs[0].NamespacedName != key {
		t.Fatalf("secret change maps to %v", reqs)
	}
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	wantCond(t, e.get(), CondReady, metav1.ConditionTrue, "")
}

func TestSandboxConfigPinnedByExistingActor(t *testing.T) {
	e := newEnv(t, true, actorTemplate())
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	first := id.TemplateRevisionName(e.get().Status.DesiredRevision)
	if _, err := e.ate.CreateActor(context.Background(), &pb.CreateActorRequest{Actor: &pb.Actor{
		Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: id.ActorName()},
		ActorTemplate: &pb.ObjectRef{Atespace: ns, Name: first},
	}}); err != nil {
		t.Fatal(err)
	}

	e.update(func(at *substratev1alpha1.ActorTemplate) {
		at.Spec.SandboxConfigName = "gvisor-v2"
		at.Spec.Containers[0].Image = image2
	})
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	at := e.get()
	wantCond(t, at, CondSandboxConfigOutdated, metav1.ConditionTrue, "PinnedByActor")
	rev := e.revisions()[id.TemplateRevisionName(at.Status.DesiredRevision)]
	if got := rev.GetSandboxConfig().GetConfigName(); got != "gvisor-v1" {
		t.Fatalf("new revision uses %s, want the pinned gvisor-v1", got)
	}
	if got := rev.GetContainers()[0].GetImage(); got != image2 {
		t.Fatalf("image change not applied: %s", got)
	}

	if _, err := e.ate.DeleteActor(context.Background(), &pb.DeleteActorRequest{Actor: &pb.ObjectRef{Atespace: ns, Name: id.ActorName()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	at = e.get()
	wantCond(t, at, CondSandboxConfigOutdated, metav1.ConditionFalse, "")
	if got := e.revisions()[id.TemplateRevisionName(at.Status.DesiredRevision)].GetSandboxConfig().GetConfigName(); got != "gvisor-v2" {
		t.Fatalf("without an actor the CR's config applies, got %s", got)
	}
}

func TestSchedulable(t *testing.T) {
	tests := []struct {
		name     string
		pools    []client.Object
		selector map[string]string
		want     metav1.ConditionStatus
	}{
		{name: "no pools", want: metav1.ConditionFalse},
		{name: "fits", pools: []client.Object{workerPool("small", "256Mi"), workerPool("big", "2Gi")}, want: metav1.ConditionTrue},
		{name: "too big", pools: []client.Object{workerPool("small", "256Mi")}, want: metav1.ConditionFalse},
		{name: "selector excludes the big pool", pools: []client.Object{workerPool("small", "256Mi"), workerPool("big", "2Gi")},
			selector: map[string]string{"pool": "small"}, want: metav1.ConditionFalse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := actorTemplate()
			if tt.selector != nil {
				at.Spec.WorkerSelector = &metav1.LabelSelector{MatchLabels: tt.selector}
			}
			e := build(t, true, append(tt.pools, at)...)
			if _, err := e.reconcile(); err != nil {
				t.Fatal(err)
			}
			wantCond(t, e.get(), CondSchedulable, tt.want, "")
			if (len(e.revisions()) > 0) != (tt.want == metav1.ConditionTrue) {
				t.Fatalf("revisions = %d", len(e.revisions()))
			}
		})
	}
}

func TestDeleteReleasesImmediately(t *testing.T) {
	e := newEnv(t, true, actorTemplate())
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	referenced := id.TemplateRevisionName(e.get().Status.DesiredRevision)
	if _, err := e.ate.CreateActor(context.Background(), &pb.CreateActorRequest{Actor: &pb.Actor{
		Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: id.ActorName()},
		ActorTemplate: &pb.ObjectRef{Atespace: ns, Name: referenced},
	}}); err != nil {
		t.Fatal(err)
	}
	e.update(func(at *substratev1alpha1.ActorTemplate) { at.Spec.Containers[0].Image = image2 })
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	unreferenced := id.TemplateRevisionName(e.get().Status.DesiredRevision)

	e.srv.FailNext("ListActors", codes.Unavailable)
	if err := e.r.Delete(context.Background(), e.get()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	var at substratev1alpha1.ActorTemplate
	if err := e.r.Get(context.Background(), key, &at); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("ActorTemplate should be gone, err=%v", err)
	}
	if len(e.revisions()) != 2 {
		t.Fatal("with ateapi unavailable, revisions are left for the sweeper")
	}

	e2 := newEnv(t, true, actorTemplate())
	if _, err := e2.reconcile(); err != nil {
		t.Fatal(err)
	}
	ref2 := id.TemplateRevisionName(e2.get().Status.DesiredRevision)
	if _, err := e2.ate.CreateActor(context.Background(), &pb.CreateActorRequest{Actor: &pb.Actor{
		Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: id.ActorName()},
		ActorTemplate: &pb.ObjectRef{Atespace: ns, Name: ref2},
	}}); err != nil {
		t.Fatal(err)
	}
	e2.update(func(at *substratev1alpha1.ActorTemplate) { at.Spec.Containers[0].Image = image2 })
	if _, err := e2.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := e2.r.Delete(context.Background(), e2.get()); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.reconcile(); err != nil {
		t.Fatal(err)
	}
	revs := e2.revisions()
	if _, ok := revs[ref2]; !ok || len(revs) != 1 {
		t.Fatalf("only the retained actor's revision should remain, got %d (unreferenced=%s)", len(revs), unreferenced)
	}
}

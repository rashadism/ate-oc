package fakeateapi

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rashadism/oc-substrate/internal/ateclient"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const (
	ns     = "dp-ns"
	gvisor = pb.SandboxClass_SANDBOX_CLASS_GVISOR
)

func setup(t *testing.T, opts ...Option) (*Server, pb.ControlClient) {
	t.Helper()
	s := New(append([]Option{WithSandboxConfig("gvisor-v1", gvisor), WithSandboxConfig("gvisor-v2", gvisor)}, opts...)...)
	c := Start(t, s)
	mustOK(t, call(c.CreateAtespace(ctx(), &pb.CreateAtespaceRequest{Atespace: &pb.Atespace{Metadata: &pb.ResourceMetadata{Name: ns}}})))
	return s, c
}

func ctx() context.Context { return context.Background() }

func call[T any](v T, err error) error { return err }

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("code = %v (%v), want %v", status.Code(err), err, code)
	}
}

func template(name, sandbox string) *pb.CreateActorTemplateRequest {
	return &pb.CreateActorTemplateRequest{ActorTemplate: &pb.ActorTemplate{
		Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: name},
		SandboxConfig: &pb.SandboxConfig{SandboxClass: gvisor, ConfigName: sandbox},
		Containers:    []*pb.Container{{Name: "main", Image: "img"}},
	}}
}

func actor(name string) *pb.CreateActorRequest {
	return &pb.CreateActorRequest{Actor: &pb.Actor{
		Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: name},
		ActorTemplate: &pb.ObjectRef{Atespace: ns, Name: "t1"},
	}}
}

func ref(name string) *pb.ObjectRef { return &pb.ObjectRef{Atespace: ns, Name: name} }

func TestAtespace(t *testing.T) {
	_, c := setup(t)
	_, err := c.CreateAtespace(ctx(), &pb.CreateAtespaceRequest{Atespace: &pb.Atespace{Metadata: &pb.ResourceMetadata{Name: ns}}})
	wantCode(t, err, codes.AlreadyExists)
	_, err = c.GetAtespace(ctx(), &pb.GetAtespaceRequest{Atespace: &pb.ObjectRef{Name: "nope"}})
	wantCode(t, err, codes.NotFound)
	as, err := c.GetAtespace(ctx(), &pb.GetAtespaceRequest{Atespace: &pb.ObjectRef{Name: ns}})
	mustOK(t, err)
	if as.GetMetadata().GetUid() == "" || as.GetMetadata().GetVersion() != 1 {
		t.Fatalf("server metadata not assigned: %v", as.GetMetadata())
	}
}

func TestActorTemplate(t *testing.T) {
	s, c := setup(t)

	tmpl, err := c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))
	mustOK(t, err)
	if tmpl.GetStatus().GetGoldenSnapshotStatus() != nil {
		t.Fatal("golden status must start empty")
	}
	_, err = c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))
	wantCode(t, err, codes.AlreadyExists)
	_, err = c.CreateActorTemplate(ctx(), template("t2", "missing"))
	wantCode(t, err, codes.FailedPrecondition)
	other := template("t3", "gvisor-v1")
	other.ActorTemplate.Metadata.Atespace = "nope"
	_, err = c.CreateActorTemplate(ctx(), other)
	wantCode(t, err, codes.FailedPrecondition)

	s.SetGolden(ns, "t1", "")
	got, err := c.GetActorTemplate(ctx(), &pb.GetActorTemplateRequest{ActorTemplate: ref("t1")})
	mustOK(t, err)
	if got.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag() == nil {
		t.Fatal("golden tag not set")
	}

	mustOK(t, call(c.CreateActor(ctx(), actor("a1"))))
	_, err = c.DeleteActorTemplate(ctx(), &pb.DeleteActorTemplateRequest{ActorTemplate: ref("t1"), Options: &pb.DeleteOptions{Uid: "stale"}})
	wantCode(t, err, codes.Aborted)
	// Referencing actors are not checked; they break afterwards.
	mustOK(t, call(c.DeleteActorTemplate(ctx(), &pb.DeleteActorTemplateRequest{ActorTemplate: ref("t1")})))
	_, err = c.ResumeActor(ctx(), &pb.ResumeActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.FailedPrecondition)
	_, err = c.SuspendActor(ctx(), &pb.SuspendActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.FailedPrecondition)
	mustOK(t, call(c.DeleteActor(ctx(), &pb.DeleteActorRequest{Actor: ref("a1")})))
	_, err = c.GetActorTemplate(ctx(), &pb.GetActorTemplateRequest{ActorTemplate: ref("t1")})
	wantCode(t, err, codes.NotFound)
}

func TestCreateActor(t *testing.T) {
	s, c := setup(t)
	_, err := c.CreateActor(ctx(), actor("a1"))
	wantCode(t, err, codes.FailedPrecondition)

	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))))
	a, err := c.CreateActor(ctx(), actor("cold"))
	mustOK(t, err)
	if a.GetStatus().GetState() != pb.ActorState_ACTOR_STATE_SUSPENDED || a.GetSourceTag() != nil {
		t.Fatalf("before golden: state=%v sourceTag=%v", a.GetStatus().GetState(), a.GetSourceTag())
	}
	s.SetGolden(ns, "t1", "")
	a, err = c.CreateActor(ctx(), actor("warm"))
	mustOK(t, err)
	if a.GetSourceTag() == nil {
		t.Fatal("actor created after golden should be seeded from the golden tag")
	}
	_, err = c.CreateActor(ctx(), actor("warm"))
	wantCode(t, err, codes.AlreadyExists)
	_, err = c.GetActor(ctx(), &pb.GetActorRequest{Actor: ref("nope")})
	wantCode(t, err, codes.NotFound)
}

func TestLifecycle(t *testing.T) {
	s, c := setup(t, WithAutoGolden())
	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))))
	mustOK(t, call(c.CreateActor(ctx(), actor("a1"))))

	state := func() pb.ActorState {
		a, err := c.GetActor(ctx(), &pb.GetActorRequest{Actor: ref("a1")})
		mustOK(t, err)
		return a.GetStatus().GetState()
	}

	_, err := c.RevertActor(ctx(), &pb.RevertActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.FailedPrecondition)
	mustOK(t, call(c.SuspendActor(ctx(), &pb.SuspendActorRequest{Actor: ref("a1")})))

	r, err := c.ResumeActor(ctx(), &pb.ResumeActorRequest{Actor: ref("a1")})
	mustOK(t, err)
	if !r.GetResumed() || state() != pb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("resume: resumed=%v state=%v", r.GetResumed(), state())
	}
	r, err = c.ResumeActor(ctx(), &pb.ResumeActorRequest{Actor: ref("a1")})
	mustOK(t, err)
	if r.GetResumed() {
		t.Fatal("resume of a RUNNING actor must report resumed=false")
	}
	_, err = c.DeleteActor(ctx(), &pb.DeleteActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.FailedPrecondition)

	mustOK(t, call(c.SuspendActor(ctx(), &pb.SuspendActorRequest{Actor: ref("a1")})))
	if state() != pb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("state = %v after suspend", state())
	}

	s.SetActorState(ns, "a1", pb.ActorState_ACTOR_STATE_CRASHED, "oom")
	a, _ := c.GetActor(ctx(), &pb.GetActorRequest{Actor: ref("a1")})
	if a.GetStatus().GetCrash().GetMessage() != "oom" {
		t.Fatal("crash reason not recorded")
	}
	_, err = c.ResumeActor(ctx(), &pb.ResumeActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.FailedPrecondition)
	_, err = c.SuspendActor(ctx(), &pb.SuspendActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.FailedPrecondition)
	mustOK(t, call(c.RevertActor(ctx(), &pb.RevertActorRequest{Actor: ref("a1")})))
	a, _ = c.GetActor(ctx(), &pb.GetActorRequest{Actor: ref("a1")})
	if a.GetStatus().GetState() != pb.ActorState_ACTOR_STATE_SUSPENDED || a.GetStatus().GetCrash() != nil {
		t.Fatalf("revert: %v", a.GetStatus())
	}

	s.SetActorState(ns, "a1", pb.ActorState_ACTOR_STATE_RUNNING, "")
	mustOK(t, call(c.DeleteActor(ctx(), &pb.DeleteActorRequest{Actor: ref("a1"), AnyState: true})))
	_, err = c.DeleteActor(ctx(), &pb.DeleteActorRequest{Actor: ref("a1")})
	wantCode(t, err, codes.NotFound)
}

func TestUpdateActor(t *testing.T) {
	s, c := setup(t, WithAutoGolden())
	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))))
	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t2", "gvisor-v1"))))
	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t3", "gvisor-v2"))))
	a, err := c.CreateActor(ctx(), actor("a1"))
	mustOK(t, err)

	update := func(a *pb.Actor, tmpl string) (*pb.Actor, error) {
		a.ActorTemplate = ref(tmpl)
		return c.UpdateActor(ctx(), &pb.UpdateActorRequest{Actor: a})
	}

	noPre := &pb.Actor{Metadata: &pb.ResourceMetadata{Atespace: ns, Name: "a1"}, SourceTag: a.GetSourceTag()}
	_, err = update(noPre, "t2")
	wantCode(t, err, codes.InvalidArgument)

	updated, err := update(a, "t2")
	mustOK(t, err)
	if updated.GetMetadata().GetVersion() != a.GetMetadata().GetVersion()+1 {
		t.Fatalf("version = %d, want %d", updated.GetMetadata().GetVersion(), a.GetMetadata().GetVersion()+1)
	}
	_, err = update(a, "t1")
	wantCode(t, err, codes.Aborted)

	_, err = update(updated, "t3")
	wantCode(t, err, codes.FailedPrecondition)
	_, err = update(updated, "missing")
	wantCode(t, err, codes.FailedPrecondition)

	s.SetActorState(ns, "a1", pb.ActorState_ACTOR_STATE_RUNNING, "")
	running, _ := c.GetActor(ctx(), &pb.GetActorRequest{Actor: ref("a1")})
	_, err = update(running, "t1")
	wantCode(t, err, codes.FailedPrecondition)
	_, err = update(running, "t2")
	mustOK(t, err)
}

func TestEgressPolicy(t *testing.T) {
	_, c := setup(t, WithAutoGolden())
	policy := func(name string) *pb.EgressPolicy {
		return &pb.EgressPolicy{Metadata: &pb.ResourceMetadata{Atespace: ns, Name: name}}
	}
	_, err := c.CreateActorEgressPolicy(ctx(), &pb.CreateActorEgressPolicyRequest{Actor: ref("a1"), EgressPolicy: policy("default")})
	wantCode(t, err, codes.FailedPrecondition)

	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))))
	mustOK(t, call(c.CreateActor(ctx(), actor("a1"))))
	_, err = c.CreateActorEgressPolicy(ctx(), &pb.CreateActorEgressPolicyRequest{Actor: ref("a1"), EgressPolicy: policy("custom")})
	wantCode(t, err, codes.InvalidArgument)
	p, err := c.CreateActorEgressPolicy(ctx(), &pb.CreateActorEgressPolicyRequest{Actor: ref("a1"), EgressPolicy: policy("default")})
	mustOK(t, err)
	_, err = c.CreateActorEgressPolicy(ctx(), &pb.CreateActorEgressPolicyRequest{Actor: ref("a1"), EgressPolicy: policy("default")})
	wantCode(t, err, codes.AlreadyExists)

	p.Rules = []*pb.EgressRule{{}}
	p2, err := c.UpdateActorEgressPolicy(ctx(), &pb.UpdateActorEgressPolicyRequest{Actor: ref("a1"), EgressPolicy: p})
	mustOK(t, err)
	if len(p2.GetRules()) != 1 {
		t.Fatal("rules not replaced")
	}
	_, err = c.UpdateActorEgressPolicy(ctx(), &pb.UpdateActorEgressPolicyRequest{Actor: ref("a1"), EgressPolicy: p})
	wantCode(t, err, codes.Aborted)

	mustOK(t, call(c.DeleteActor(ctx(), &pb.DeleteActorRequest{Actor: ref("a1")})))
	_, err = c.GetActorEgressPolicy(ctx(), &pb.GetActorEgressPolicyRequest{Actor: ref("a1")})
	wantCode(t, err, codes.NotFound)
}

func TestClientRetriesLeaseConflicts(t *testing.T) {
	s := New(WithSandboxConfig("gvisor-v1", gvisor), WithAutoGolden())
	retry := ateclient.RetryAborted(ateclient.RetryPolicy{Attempts: 3, Initial: time.Millisecond, Max: time.Millisecond})
	c := Start(t, s, grpc.WithChainUnaryInterceptor(retry))
	mustOK(t, call(c.CreateAtespace(ctx(), &pb.CreateAtespaceRequest{Atespace: &pb.Atespace{Metadata: &pb.ResourceMetadata{Name: ns}}})))
	mustOK(t, call(c.CreateActorTemplate(ctx(), template("t1", "gvisor-v1"))))
	mustOK(t, call(c.CreateActor(ctx(), actor("a1"))))

	s.FailNext("SuspendActor", codes.Aborted)
	mustOK(t, call(c.SuspendActor(ctx(), &pb.SuspendActorRequest{Actor: ref("a1")})))

	s.FailNext("UpdateActor", codes.Aborted)
	a, _ := c.GetActor(ctx(), &pb.GetActorRequest{Actor: ref("a1")})
	_, err := c.UpdateActor(ctx(), &pb.UpdateActorRequest{Actor: a})
	wantCode(t, err, codes.Aborted)
}

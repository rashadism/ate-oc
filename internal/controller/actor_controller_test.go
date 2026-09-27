package controller

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

var actorKey = types.NamespacedName{Namespace: ns, Name: "app"}

func actorCR(ident naming.Identity) *substratev1alpha1.Actor {
	return &substratev1alpha1.Actor{
		ObjectMeta: metav1.ObjectMeta{Name: actorKey.Name, Namespace: ns, Labels: map[string]string{
			naming.LabelComponentUID:   ident.ComponentUID,
			naming.LabelEnvironmentUID: ident.EnvironmentUID,
		}},
		Spec: substratev1alpha1.ActorSpec{TemplateRef: substratev1alpha1.LocalRef{Name: key.Name}},
	}
}

func (e *env) reconcileActor() ctrl.Result {
	e.t.Helper()
	res, err := e.actors.Reconcile(context.Background(), ctrl.Request{NamespacedName: actorKey})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) reconcileRetained(name string) ctrl.Result {
	e.t.Helper()
	res, err := e.retained.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) actorCR() *substratev1alpha1.Actor {
	e.t.Helper()
	var a substratev1alpha1.Actor
	if err := e.actors.Get(context.Background(), actorKey, &a); err != nil {
		e.t.Fatal(err)
	}
	return &a
}

func (e *env) updateActorCR(mutate func(*substratev1alpha1.Actor)) {
	e.t.Helper()
	a := e.actorCR()
	mutate(a)
	if err := e.actors.Update(context.Background(), a); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) substrateActor(name string) *pb.Actor {
	e.t.Helper()
	a, err := e.ate.GetActor(context.Background(), &pb.GetActorRequest{Actor: actorRef(ns, name)})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) retainedEntry(name string) *substratev1alpha1.RetainedActor {
	e.t.Helper()
	var ra substratev1alpha1.RetainedActor
	err := e.actors.Get(context.Background(), types.NamespacedName{Name: name}, &ra)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return &ra
}

// deployed returns an env with a Ready ActorTemplate and an Actor CR whose
// Substrate actor exists.
func deployed(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, true, actorTemplate(), actorCR(id))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	if e.substrateActor(id.ActorName()) == nil {
		t.Fatal("actor not created")
	}
	return e
}

func (e *env) readyRevision() string {
	e.t.Helper()
	return id.TemplateRevisionName(e.get().Status.LatestReadyRevision)
}

func TestActorWaitsForTemplate(t *testing.T) {
	e := newEnv(t, false, actorTemplate(), actorCR(id))
	if res := e.reconcileActor(); res.RequeueAfter != actorPoll {
		t.Fatalf("requeue = %v", res.RequeueAfter)
	}
	wantActorCond(t, e.actorCR(), CondReady, metav1.ConditionFalse, "TemplateNotReady")

	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	if e.substrateActor(id.ActorName()) != nil {
		t.Fatal("actor must wait for the golden snapshot")
	}

	e.srv.SetGolden(ns, id.TemplateRevisionName(e.get().Status.DesiredRevision), "")
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	a := e.substrateActor(id.ActorName())
	if a == nil || a.GetActorTemplate().GetName() != e.readyRevision() || a.GetSourceTag() == nil {
		t.Fatalf("actor = %v", a)
	}
	cr := e.actorCR()
	if cr.Status.Name != id.ActorName() || cr.Status.UID != a.GetMetadata().GetUid() || cr.Status.State != "SUSPENDED" ||
		cr.Status.Revision != e.readyRevision() {
		t.Fatalf("status = %+v", cr.Status)
	}
	wantActorCond(t, cr, CondReady, metav1.ConditionTrue, "SUSPENDED")
}

func TestActorIdentityMismatch(t *testing.T) {
	other := naming.Identity{ComponentUID: "other", EnvironmentUID: id.EnvironmentUID}
	e := newEnv(t, true, actorTemplate(), actorCR(other))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	wantActorCond(t, e.actorCR(), CondAccepted, metav1.ConditionFalse, "IdentityMismatch")
	if e.substrateActor(other.ActorName()) != nil {
		t.Fatal("no actor for a mismatched template")
	}
}

func TestDeleteRetainsAndReattaches(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	uid := e.substrateActor(name).GetMetadata().GetUid()
	e.srv.SetActorState(ns, name, pb.ActorState_ACTOR_STATE_RUNNING, "")

	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	if err := e.actors.Get(context.Background(), actorKey, &substratev1alpha1.Actor{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Actor CR should be released immediately, err=%v", err)
	}
	entryName := substratev1alpha1.RetainedActorName(ns, name)
	ra := e.retainedEntry(entryName)
	if ra == nil || ra.Spec.ActorUID != uid || ra.Spec.ComponentUID != id.ComponentUID {
		t.Fatalf("retained entry = %+v", ra)
	}
	if got := e.substrateActor(name).GetStatus().GetState(); got != pb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("finalizer must not call Substrate; state = %v", got)
	}

	e.reconcileRetained(entryName)
	if res := e.reconcileRetained(entryName); res.RequeueAfter <= retainedPoll {
		t.Fatalf("a suspended entry should requeue at expiry, got %v", res.RequeueAfter)
	}
	if got := e.substrateActor(name).GetStatus().GetState(); got != pb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("retained actor state = %v", got)
	}
	if e.retainedEntry(entryName).Status.Phase != substratev1alpha1.RetainedSuspended {
		t.Fatal("phase not Suspended")
	}

	if err := e.actors.Create(context.Background(), actorCR(id)); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	if e.retainedEntry(entryName) != nil {
		t.Fatal("re-attach must release the retained entry")
	}
	if got := e.substrateActor(name).GetMetadata().GetUid(); got != uid {
		t.Fatalf("re-attached a different incarnation: %s != %s", got, uid)
	}
}

func TestRecreatedComponentGetsFreshActor(t *testing.T) {
	e := deployed(t)
	old := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()

	recreated := naming.Identity{ComponentUID: "comp-uid-2", EnvironmentUID: id.EnvironmentUID}
	at := e.get()
	at.Labels[naming.LabelComponentUID] = recreated.ComponentUID
	if err := e.r.Update(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := e.actors.Create(context.Background(), actorCR(recreated)); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	if e.substrateActor(recreated.ActorName()) == nil {
		t.Fatal("recreated component needs its own actor")
	}
	if e.retainedEntry(substratev1alpha1.RetainedActorName(ns, old)) == nil || e.substrateActor(old) == nil {
		t.Fatal("the old component's state must stay retained, not re-attached")
	}
}

func TestRetainedExpiryAndPurge(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	if err := e.r.Delete(context.Background(), e.get()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(e.revisions()) != 1 {
		t.Fatalf("the retained actor's revision must survive template deletion, got %d", len(e.revisions()))
	}

	entryName := substratev1alpha1.RetainedActorName(ns, name)
	e.reconcileRetained(entryName)
	e.now = e.now.Add(25 * time.Hour)
	e.reconcileRetained(entryName)
	e.reconcileRetained(entryName)
	if e.retainedEntry(entryName) != nil || e.substrateActor(name) != nil {
		t.Fatal("expired entry should purge the actor")
	}
	if len(e.revisions()) != 0 {
		t.Fatal("purge should collect the now-unreferenced revision")
	}
}

func TestPurgeOnDeleteSparesNewerIncarnation(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	entryName := substratev1alpha1.RetainedActorName(ns, name)
	e.reconcileRetained(entryName)

	ra := e.retainedEntry(entryName)
	ra.Spec.ActorUID = "some-older-incarnation"
	if err := e.actors.Update(context.Background(), ra); err != nil {
		t.Fatal(err)
	}
	if err := e.actors.Delete(context.Background(), ra); err != nil {
		t.Fatal(err)
	}
	e.reconcileRetained(entryName)
	if e.retainedEntry(entryName) != nil {
		t.Fatal("entry should be gone")
	}
	if e.substrateActor(name) == nil {
		t.Fatal("purge must not delete an actor with a different uid")
	}
}

func TestExplicitPurge(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	entryName := substratev1alpha1.RetainedActorName(ns, name)
	e.reconcileRetained(entryName)
	if err := e.actors.Delete(context.Background(), e.retainedEntry(entryName)); err != nil {
		t.Fatal(err)
	}
	e.reconcileRetained(entryName)
	if e.substrateActor(name) != nil || e.retainedEntry(entryName) != nil {
		t.Fatal("deleting the entry purges the actor")
	}
}

func TestRepoint(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *env, actor string)
		steps int
	}{
		{name: "suspended", setup: func(*env, string) {}, steps: 1},
		{name: "running", setup: func(e *env, a string) { e.srv.SetActorState(ns, a, pb.ActorState_ACTOR_STATE_RUNNING, "") }, steps: 2},
		{name: "crashed", setup: func(e *env, a string) {
			e.srv.SetActorState(ns, a, pb.ActorState_ACTOR_STATE_CRASHED, "oom")
		}, steps: 2},
		{name: "stuck suspending", setup: func(e *env, a string) {
			e.srv.SetActorState(ns, a, pb.ActorState_ACTOR_STATE_SUSPENDING, "")
			e.srv.AgeActor(ns, a, 11*time.Minute)
		}, steps: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := deployed(t)
			name := id.ActorName()
			before := e.substrateActor(name).GetMetadata().GetUid()
			tt.setup(e, name)
			e.update(func(at *substratev1alpha1.ActorTemplate) { at.Spec.Containers[0].Image = image2 })
			if _, err := e.reconcile(); err != nil {
				t.Fatal(err)
			}
			target := e.readyRevision()
			for range tt.steps {
				e.reconcileActor()
			}
			a := e.substrateActor(name)
			if a.GetActorTemplate().GetName() != target || a.GetMetadata().GetUid() != before {
				t.Fatalf("actor on %s (uid %s), want %s in place", a.GetActorTemplate().GetName(), a.GetMetadata().GetUid(), target)
			}
		})
	}

	t.Run("fresh suspending waits", func(t *testing.T) {
		e := deployed(t)
		name := id.ActorName()
		e.srv.SetActorState(ns, name, pb.ActorState_ACTOR_STATE_SUSPENDING, "")
		e.update(func(at *substratev1alpha1.ActorTemplate) { at.Spec.Containers[0].Image = image2 })
		if _, err := e.reconcile(); err != nil {
			t.Fatal(err)
		}
		e.reconcileActor()
		a := e.substrateActor(name)
		if a.GetStatus().GetState() != pb.ActorState_ACTOR_STATE_SUSPENDING || a.GetActorTemplate().GetName() == e.readyRevision() {
			t.Fatal("never repoint or re-suspend mid-suspend")
		}
	})

	t.Run("incompatible revision is blocked", func(t *testing.T) {
		e := deployed(t)
		e.update(func(at *substratev1alpha1.ActorTemplate) {
			at.Spec.DurableDir = &substratev1alpha1.DurableDir{MountPath: "/data"}
		})
		if _, err := e.reconcile(); err != nil {
			t.Fatal(err)
		}
		e.reconcileActor()
		wantActorCond(t, e.actorCR(), CondRepointBlocked, metav1.ConditionTrue, "IncompatibleRevision")
		if e.substrateActor(id.ActorName()).GetActorTemplate().GetName() == e.readyRevision() {
			t.Fatal("must not repoint across volume changes")
		}
	})
}

func TestPausedSuspends(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	e.srv.SetActorState(ns, name, pb.ActorState_ACTOR_STATE_RUNNING, "")
	e.updateActorCR(func(a *substratev1alpha1.Actor) { a.Spec.Paused = true })
	e.reconcileActor()
	if got := e.substrateActor(name).GetStatus().GetState(); got != pb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("paused actor state = %v", got)
	}
}

func TestCrashIsSurfacedThenReverted(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	e.srv.SetActorState(ns, name, pb.ActorState_ACTOR_STATE_CRASHED, "exit 137")
	e.reconcileActor()
	cr := e.actorCR()
	if cr.Status.CrashReason != "exit 137" || cr.Status.CrashTime == nil {
		t.Fatalf("crash not surfaced: %+v", cr.Status)
	}
	wantActorCond(t, cr, CondReady, metav1.ConditionFalse, "Crashed")
	if got := e.substrateActor(name).GetStatus().GetState(); got != pb.ActorState_ACTOR_STATE_CRASHED {
		t.Fatal("revert waits for the crash backoff")
	}

	e.now = e.now.Add(time.Minute)
	e.reconcileActor()
	if got := e.substrateActor(name).GetStatus().GetState(); got != pb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("state after backoff = %v", got)
	}
}

func TestEgressPolicy(t *testing.T) {
	e := deployed(t)
	ref := actorRef(ns, id.ActorName())
	policy := func() *pb.EgressPolicy {
		p, err := e.ate.GetActorEgressPolicy(context.Background(), &pb.GetActorEgressPolicyRequest{Actor: ref})
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if policy() != nil {
		t.Fatal("no extra egress means no policy (deny all)")
	}

	e.updateActorCR(func(a *substratev1alpha1.Actor) {
		a.Spec.ExtraEgress = &substratev1alpha1.ExtraEgress{Hostnames: []string{"api.example.com"}}
	})
	e.reconcileActor()
	if p := policy(); len(p.GetRules()) != 1 || p.GetRules()[0].GetHostnames().GetPatterns()[0] != "api.example.com" {
		t.Fatalf("policy = %v", p)
	}
	wantActorCond(t, e.actorCR(), CondEgressResolved, metav1.ConditionTrue, "Applied")

	e.updateActorCR(func(a *substratev1alpha1.Actor) {
		a.Spec.ExtraEgress = &substratev1alpha1.ExtraEgress{Hostnames: []string{"api.example.com", "b.example.com"}}
	})
	e.reconcileActor()
	if p := policy(); len(p.GetRules()[0].GetHostnames().GetPatterns()) != 2 {
		t.Fatalf("policy not updated: %v", p)
	}

	e.updateActorCR(func(a *substratev1alpha1.Actor) {
		a.Spec.ExtraEgress = &substratev1alpha1.ExtraEgress{CIDRs: []string{"10.0.0.1"}}
	})
	e.reconcileActor()
	wantActorCond(t, e.actorCR(), CondEgressResolved, metav1.ConditionFalse, "Rejected")
	if policy() != nil {
		t.Fatal("rejected egress must fall back to deny all")
	}
}

func TestOrphanScan(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	orphan := naming.Identity{ComponentUID: "gone", EnvironmentUID: "gone"}.ActorName()
	for _, n := range []string{orphan, "my-counter-1"} {
		if _, err := e.ate.CreateActor(context.Background(), &pb.CreateActorRequest{Actor: &pb.Actor{
			Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: n},
			ActorTemplate: actorRef(ns, e.readyRevision()),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.retainedEntry(substratev1alpha1.RetainedActorName(ns, orphan)) == nil {
		t.Fatal("orphan not retained")
	}
	if e.retainedEntry(substratev1alpha1.RetainedActorName(ns, name)) != nil {
		t.Fatal("actor with a live CR must not be retained")
	}
	if e.retainedEntry(substratev1alpha1.RetainedActorName(ns, "my-counter-1")) != nil {
		t.Fatal("actors the operator did not name are left alone")
	}
}

func wantActorCond(t *testing.T, a *substratev1alpha1.Actor, typ string, s metav1.ConditionStatus, reason string) {
	t.Helper()
	for _, c := range a.Status.Conditions {
		if c.Type == typ {
			if c.Status != s || (reason != "" && c.Reason != reason) {
				t.Fatalf("%s = %s/%s (%s), want %s/%s", typ, c.Status, c.Reason, c.Message, s, reason)
			}
			return
		}
	}
	t.Fatalf("condition %s not set", typ)
}

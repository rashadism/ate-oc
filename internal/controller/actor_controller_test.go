package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	substratev1alpha1 "github.com/rashadism/ate-oc/api/v1alpha1"
	"github.com/rashadism/ate-oc/internal/naming"
	pb "github.com/rashadism/ate-oc/third_party/ateapipb"
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

// Re-attach releases the entry in two writes; the retained-state controller
// may run in between and must not re-arm the finalizer and purge.
func TestReleaseSurvivesInterleavedRetainedReconcile(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	entryName := substratev1alpha1.RetainedActorName(ns, name)
	e.reconcileRetained(entryName)

	ra := e.retainedEntry(entryName)
	metav1.SetMetaDataAnnotation(&ra.ObjectMeta, substratev1alpha1.ReleasedAnnotation, "true")
	ra.Finalizers = nil
	if err := e.actors.Update(context.Background(), ra); err != nil {
		t.Fatal(err)
	}
	e.reconcileRetained(entryName)
	if e.retainedEntry(entryName) != nil {
		t.Fatal("a released entry should be dropped")
	}
	if e.substrateActor(name) == nil {
		t.Fatal("releasing must never purge the actor")
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

// A DeleteActor Aborted also covers plain lease contention, not just a uid
// mismatch; purge must retry rather than treat the entry as superseded.
func TestPurgeRetriesOnLeaseContention(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	entryName := substratev1alpha1.RetainedActorName(ns, name)
	e.reconcileRetained(entryName)
	e.now = e.now.Add(25 * time.Hour)
	e.reconcileRetained(entryName)

	e.srv.FailNext("DeleteActor", codes.Aborted)
	if _, err := e.retained.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: entryName}}); err == nil {
		t.Fatal("a transient Aborted must be retried, not treated as a uid mismatch")
	}
	if e.retainedEntry(entryName) == nil {
		t.Fatal("lease contention must not drop the entry")
	}
	if e.substrateActor(name) == nil {
		t.Fatal("lease contention must not be treated as if the actor were already gone")
	}
}

// purge is handed the RetainedActor as fetched by Reconcile; if a concurrent
// reattach released the entry since then, it must re-check before deleting.
func TestPurgeRereadsBeforeDeleting(t *testing.T) {
	e := deployed(t)
	name := id.ActorName()
	if err := e.actors.Delete(context.Background(), e.actorCR()); err != nil {
		t.Fatal(err)
	}
	e.reconcileActor()
	entryName := substratev1alpha1.RetainedActorName(ns, name)
	e.reconcileRetained(entryName)

	stale := e.retainedEntry(entryName)

	live := e.retainedEntry(entryName)
	metav1.SetMetaDataAnnotation(&live.ObjectMeta, substratev1alpha1.ReleasedAnnotation, "true")
	live.Finalizers = nil
	if err := e.actors.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	if err := e.retained.purge(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if e.retainedEntry(entryName) != nil {
		t.Fatal("a released entry should still be dropped")
	}
	if e.substrateActor(name) == nil {
		t.Fatal("a concurrently released entry must not be purged")
	}
}

// collectRevisions must not stop at the first page of revisions either.
func TestCollectRevisionsPaginates(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	if _, err := e.ate.CreateAtespace(ctx, &pb.CreateAtespaceRequest{Atespace: &pb.Atespace{Metadata: &pb.ResourceMetadata{Name: ns}}}); err != nil {
		t.Fatal(err)
	}
	prefix := naming.RevisionPrefixForActor(id.ActorName())
	var extra []string
	for _, h := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd"} {
		n := prefix + h
		if _, err := e.ate.CreateActorTemplate(ctx, &pb.CreateActorTemplateRequest{ActorTemplate: &pb.ActorTemplate{
			Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: n},
			SandboxConfig: &pb.SandboxConfig{SandboxClass: pb.SandboxClass_SANDBOX_CLASS_GVISOR, ConfigName: "gvisor-v1"},
		}}); err != nil {
			t.Fatal(err)
		}
		extra = append(extra, n)
	}

	ra := &substratev1alpha1.RetainedActor{Spec: substratev1alpha1.RetainedActorSpec{Atespace: ns, ActorName: id.ActorName()}}
	if err := e.retained.collectRevisions(ctx, ra); err != nil {
		t.Fatal(err)
	}
	revs := e.revisions()
	for _, n := range extra {
		if _, ok := revs[n]; ok {
			t.Fatalf("unreferenced revision %s should have been collected", n)
		}
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

// A scan must not stop at the first page of actors.
func TestOrphanScanPaginates(t *testing.T) {
	e := deployed(t)
	var orphans []string
	for i := range 4 {
		o := naming.Identity{ComponentUID: fmt.Sprintf("gone-%d", i), EnvironmentUID: "gone"}.ActorName()
		orphans = append(orphans, o)
		if _, err := e.ate.CreateActor(context.Background(), &pb.CreateActorRequest{Actor: &pb.Actor{
			Metadata:      &pb.ResourceMetadata{Atespace: ns, Name: o},
			ActorTemplate: actorRef(ns, e.readyRevision()),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, o := range orphans {
		if e.retainedEntry(substratev1alpha1.RetainedActorName(ns, o)) == nil {
			t.Fatalf("orphan %s not retained", o)
		}
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

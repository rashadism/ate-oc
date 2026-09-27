package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/compile"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const (
	CondEgressResolved = "EgressResolved"
	CondRepointBlocked = "RepointBlocked"

	actorPoll    = 10 * time.Second
	crashBackoff = 30 * time.Second

	actorIdentityIndex = "actorName"
	templateRefIndex   = "spec.templateRef.name"
)

type ActorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Ate      pb.ControlClient
	Recorder events.EventRecorder
	// RetentionTTL is how long a deleted component's actor keeps its state.
	RetentionTTL time.Duration
	Now          func() time.Time
}

// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actors/finalizers,verbs=update
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=retainedactors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ActorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var act substratev1alpha1.Actor
	if err := r.Get(ctx, req.NamespacedName, &act); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !act.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &act)
	}
	if controllerutil.AddFinalizer(&act, Finalizer) {
		if err := r.Update(ctx, &act); err != nil {
			return ctrl.Result{}, err
		}
	}

	orig := act.DeepCopy()
	res, err := r.reconcile(ctx, &act)
	act.Status.ObservedGeneration = act.Generation
	if perr := r.Status().Patch(ctx, &act, client.MergeFrom(orig)); perr != nil {
		return ctrl.Result{}, errors.Join(err, perr)
	}
	return res, err
}

func (r *ActorReconciler) reconcile(ctx context.Context, act *substratev1alpha1.Actor) (ctrl.Result, error) {
	poll := ctrl.Result{RequeueAfter: actorPoll}
	id, err := naming.IdentityFromLabels(act.Labels)
	if err != nil {
		setActorCond(act, CondAccepted, false, "MissingIdentity", err.Error())
		return ctrl.Result{}, nil
	}
	name := id.ActorName()
	act.Status.Name = name
	ref := actorRef(act.Namespace, name)

	target, err := r.targetRevision(ctx, act, id)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cond := meta.FindStatusCondition(act.Status.Conditions, CondAccepted); cond != nil && cond.Status == metav1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	if err := r.reattach(ctx, act, name); err != nil {
		return ctrl.Result{}, err
	}

	a, err := r.Ate.GetActor(ctx, &pb.GetActorRequest{Actor: ref})
	switch {
	case isCode(err, codes.NotFound):
		if target == "" {
			setActorCond(act, CondReady, false, "TemplateNotReady", "waiting for the ActorTemplate's golden snapshot")
			return poll, nil
		}
		a, err = r.Ate.CreateActor(ctx, &pb.CreateActorRequest{Actor: &pb.Actor{
			Metadata:      &pb.ResourceMetadata{Atespace: act.Namespace, Name: name},
			ActorTemplate: actorRef(act.Namespace, target),
		}})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create actor: %w", err)
		}
		r.event(act, corev1.EventTypeNormal, "Created", "created actor %s on %s", name, target)
	case err != nil:
		return ctrl.Result{}, err
	}
	r.syncStatus(act, a)

	now := r.Now()
	if target != "" && a.GetActorTemplate().GetName() != target {
		return r.repoint(ctx, act, a, target, now)
	}
	meta.RemoveStatusCondition(&act.Status.Conditions, CondRepointBlocked)

	switch {
	case act.Spec.Paused:
		if _, err := settle(ctx, r.Ate, a, now); err != nil {
			return ctrl.Result{}, err
		}
	case a.GetStatus().GetState() == pb.ActorState_ACTOR_STATE_CRASHED &&
		now.Sub(a.GetStatus().GetCrash().GetCrashTime().AsTime()) >= crashBackoff:
		if _, err := r.Ate.RevertActor(ctx, &pb.RevertActorRequest{Actor: ref}); err != nil && !isCode(err, codes.FailedPrecondition) {
			return ctrl.Result{}, err
		}
		r.event(act, corev1.EventTypeWarning, "Reverted", "actor crashed (%s); reverted to its last snapshot", act.Status.CrashReason)
	}

	if err := r.applyEgress(ctx, act, ref); err != nil {
		return ctrl.Result{}, err
	}

	if a.GetStatus().GetState() == pb.ActorState_ACTOR_STATE_CRASHED {
		setActorCond(act, CondReady, false, "Crashed", act.Status.CrashReason)
	} else {
		setActorCond(act, CondReady, true, stateName(a.GetStatus().GetState()), "")
	}
	return poll, nil
}

// targetRevision returns the Substrate template the actor should run: the
// ActorTemplate's latest ready revision, or "" while none is ready.
func (r *ActorReconciler) targetRevision(ctx context.Context, act *substratev1alpha1.Actor, id naming.Identity) (string, error) {
	var at substratev1alpha1.ActorTemplate
	err := r.Get(ctx, types.NamespacedName{Namespace: act.Namespace, Name: act.Spec.TemplateRef.Name}, &at)
	if apierrors.IsNotFound(err) {
		setActorCond(act, CondAccepted, true, "Accepted", "")
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if tid, err := naming.IdentityFromLabels(at.Labels); err != nil || tid != id {
		setActorCond(act, CondAccepted, false, "IdentityMismatch", "templateRef must belong to the same component and environment")
		return "", nil
	}
	setActorCond(act, CondAccepted, true, "Accepted", "")
	for _, rev := range at.Status.Revisions {
		if rev.Hash == at.Status.LatestReadyRevision && rev.Ready {
			return rev.AteName, nil
		}
	}
	return "", nil
}

// reattach releases a retained entry for this actor: the same UIDs are back.
func (r *ActorReconciler) reattach(ctx context.Context, act *substratev1alpha1.Actor, name string) error {
	var ra substratev1alpha1.RetainedActor
	err := r.Get(ctx, types.NamespacedName{Name: substratev1alpha1.RetainedActorName(act.Namespace, name)}, &ra)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := releaseRetained(ctx, r.Client, &ra); err != nil {
		return err
	}
	r.event(act, corev1.EventTypeNormal, "Reattached", "re-attached retained actor %s", name)
	return nil
}

// releaseRetained drops an entry without purging. Marking it released in the
// same write that removes the finalizer stops the retained-state controller
// from re-arming the finalizer before the delete lands.
func releaseRetained(ctx context.Context, c client.Client, ra *substratev1alpha1.RetainedActor) error {
	if ra.Annotations[substratev1alpha1.ReleasedAnnotation] != "true" || controllerutil.ContainsFinalizer(ra, Finalizer) {
		metav1.SetMetaDataAnnotation(&ra.ObjectMeta, substratev1alpha1.ReleasedAnnotation, "true")
		controllerutil.RemoveFinalizer(ra, Finalizer)
		if err := c.Update(ctx, ra); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return client.IgnoreNotFound(c.Delete(ctx, ra))
}

func (r *ActorReconciler) repoint(ctx context.Context, act *substratev1alpha1.Actor, a *pb.Actor, target string,
	now time.Time) (ctrl.Result, error) {
	setActorCond(act, CondReady, true, "Rolling", fmt.Sprintf("moving to %s", target))
	settled, err := settle(ctx, r.Ate, a, now)
	if err != nil || !settled {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	next := proto.Clone(a).(*pb.Actor)
	next.ActorTemplate = actorRef(act.Namespace, target)
	next.Status = nil
	_, err = r.Ate.UpdateActor(ctx, &pb.UpdateActorRequest{Actor: next})
	switch {
	case isCode(err, codes.FailedPrecondition):
		setActorCond(act, CondRepointBlocked, true, "IncompatibleRevision",
			fmt.Sprintf("%s changes the sandbox or volumes; applying it resets the actor's state: %v", target, err))
		return ctrl.Result{RequeueAfter: actorPoll}, nil
	case isCode(err, codes.Aborted):
		return ctrl.Result{RequeueAfter: time.Second}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	r.event(act, corev1.EventTypeNormal, "Rolled", "actor moved from %s to %s", a.GetActorTemplate().GetName(), target)
	act.Status.Revision = target
	return ctrl.Result{Requeue: true}, nil
}

func (r *ActorReconciler) applyEgress(ctx context.Context, act *substratev1alpha1.Actor, ref *pb.ObjectRef) error {
	rules, err := compile.Egress(act.Spec.ExtraEgress)
	if err != nil {
		setActorCond(act, CondEgressResolved, false, "Rejected", err.Error())
		rules = nil
	} else {
		setActorCond(act, CondEgressResolved, true, "Applied", "")
	}

	cur, err := r.Ate.GetActorEgressPolicy(ctx, &pb.GetActorEgressPolicyRequest{Actor: ref})
	switch {
	case isCode(err, codes.NotFound):
		if len(rules) == 0 {
			return nil
		}
		_, err = r.Ate.CreateActorEgressPolicy(ctx, &pb.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &pb.EgressPolicy{
			Metadata: &pb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: "default"},
			Rules:    rules,
		}})
		return err
	case err != nil:
		return err
	}
	if len(rules) == 0 {
		_, err = r.Ate.DeleteActorEgressPolicy(ctx, &pb.DeleteActorEgressPolicyRequest{
			Actor: ref, Options: &pb.DeleteOptions{Uid: cur.GetMetadata().GetUid()},
		})
		return ignoreCodes(err, codes.NotFound, codes.Aborted)
	}
	if proto.Equal(&pb.EgressPolicy{Rules: cur.GetRules()}, &pb.EgressPolicy{Rules: rules}) {
		return nil
	}
	cur.Rules = rules
	_, err = r.Ate.UpdateActorEgressPolicy(ctx, &pb.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: cur})
	return ignoreCodes(err, codes.Aborted)
}

func ignoreCodes(err error, cs ...codes.Code) error {
	for _, c := range cs {
		if isCode(err, c) {
			return nil
		}
	}
	return err
}

func (r *ActorReconciler) syncStatus(act *substratev1alpha1.Actor, a *pb.Actor) {
	act.Status.UID = a.GetMetadata().GetUid()
	act.Status.State = stateName(a.GetStatus().GetState())
	act.Status.Revision = a.GetActorTemplate().GetName()
	act.Status.CrashReason, act.Status.CrashTime = "", nil
	if c := a.GetStatus().GetCrash(); c != nil {
		act.Status.CrashReason = c.GetMessage()
		t := metav1.NewTime(c.GetCrashTime().AsTime())
		act.Status.CrashTime = &t
	}
}

// finalize never calls Substrate: it records the actor for retention and
// releases immediately, so RenderedRelease teardown is never blocked. The
// retained-state controller suspends the actor asynchronously.
func (r *ActorReconciler) finalize(ctx context.Context, act *substratev1alpha1.Actor) error {
	if !controllerutil.ContainsFinalizer(act, Finalizer) {
		return nil
	}
	if id, err := naming.IdentityFromLabels(act.Labels); err == nil {
		name := id.ActorName()
		ra := &substratev1alpha1.RetainedActor{
			ObjectMeta: metav1.ObjectMeta{
				Name:       substratev1alpha1.RetainedActorName(act.Namespace, name),
				Finalizers: []string{Finalizer},
			},
			Spec: substratev1alpha1.RetainedActorSpec{
				Atespace:       act.Namespace,
				ActorName:      name,
				ActorUID:       act.Status.UID,
				ComponentUID:   id.ComponentUID,
				EnvironmentUID: id.EnvironmentUID,
				ExpiresAt:      metav1.NewTime(r.Now().Add(r.RetentionTTL)),
			},
		}
		if err := r.Create(ctx, ra); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	controllerutil.RemoveFinalizer(act, Finalizer)
	return r.Update(ctx, act)
}

func (r *ActorReconciler) event(act *substratev1alpha1.Actor, typ, reason, msgf string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(act, nil, typ, reason, reason, msgf, args...)
	}
}

func setActorCond(act *substratev1alpha1.Actor, t string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&act.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: act.Generation,
	})
}

func actorIdentityName(o client.Object) []string {
	id, err := naming.IdentityFromLabels(o.GetLabels())
	if err != nil {
		return nil
	}
	return []string{id.ActorName()}
}

func (r *ActorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(context.Background(), &substratev1alpha1.Actor{}, actorIdentityIndex, actorIdentityName); err != nil {
		return err
	}
	if err := idx.IndexField(context.Background(), &substratev1alpha1.Actor{}, templateRefIndex, func(o client.Object) []string {
		return []string{o.(*substratev1alpha1.Actor).Spec.TemplateRef.Name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&substratev1alpha1.Actor{}).
		Watches(&substratev1alpha1.ActorTemplate{}, handler.EnqueueRequestsFromMapFunc(r.forTemplate)).
		Named("actor").
		Complete(r)
}

func (r *ActorReconciler) forTemplate(ctx context.Context, o client.Object) []ctrl.Request {
	var list substratev1alpha1.ActorList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{templateRefIndex: o.GetName()}); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(list.Items))
	for _, a := range list.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&a)})
	}
	return reqs
}

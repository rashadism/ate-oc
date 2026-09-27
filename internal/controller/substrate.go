package controller

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const (
	suspendDeadline = 5 * time.Minute
	stuckSuspending = 10 * time.Minute
)

func actorRef(atespace, name string) *pb.ObjectRef {
	return &pb.ObjectRef{Atespace: atespace, Name: name}
}

func isCode(err error, c codes.Code) bool { return status.Code(err) == c }

// suspend is detached from the reconcile context: an interrupted suspend can
// leave the actor wedged in SUSPENDING (substrate#1914).
func suspend(ctx context.Context, ate pb.ControlClient, ref *pb.ObjectRef) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), suspendDeadline)
	defer cancel()
	_, err := ate.SuspendActor(ctx, &pb.SuspendActorRequest{Actor: ref})
	return err
}

// settle moves an actor one step toward SUSPENDED and reports whether it is
// there. A SUSPENDING actor is left alone unless it has been stuck for a while,
// in which case suspending again fast-forwards the unfinished workflow.
func settle(ctx context.Context, ate pb.ControlClient, a *pb.Actor, now time.Time) (bool, error) {
	ref := actorRef(a.GetMetadata().GetAtespace(), a.GetMetadata().GetName())
	switch a.GetStatus().GetState() {
	case pb.ActorState_ACTOR_STATE_SUSPENDED:
		return true, nil
	case pb.ActorState_ACTOR_STATE_RUNNING, pb.ActorState_ACTOR_STATE_PAUSED:
		return false, suspend(ctx, ate, ref)
	case pb.ActorState_ACTOR_STATE_CRASHED:
		_, err := ate.RevertActor(ctx, &pb.RevertActorRequest{Actor: ref})
		return false, err
	case pb.ActorState_ACTOR_STATE_SUSPENDING:
		if now.Sub(a.GetMetadata().GetUpdateTime().AsTime()) > stuckSuspending {
			return false, suspend(ctx, ate, ref)
		}
	}
	return false, nil
}

func stateName(s pb.ActorState) string {
	n := s.String()
	if len(n) > len("ACTOR_STATE_") {
		return n[len("ACTOR_STATE_"):]
	}
	return n
}

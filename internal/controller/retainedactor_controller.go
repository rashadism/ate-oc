package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const retainedPoll = 30 * time.Second

// RetainedActorReconciler suspends retained actors, and purges them (actor
// plus unreferenced template revisions) when the entry expires or is deleted.
type RetainedActorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Ate    pb.ControlClient
	Now    func() time.Time
}

// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=retainedactors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=retainedactors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=retainedactors/finalizers,verbs=update

func (r *RetainedActorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ra substratev1alpha1.RetainedActor
	if err := r.Get(ctx, req.NamespacedName, &ra); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ra.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.purge(ctx, &ra)
	}
	if controllerutil.AddFinalizer(&ra, Finalizer) {
		if err := r.Update(ctx, &ra); err != nil {
			return ctrl.Result{}, err
		}
	}
	now := r.Now()
	if !now.Before(ra.Spec.ExpiresAt.Time) {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &ra))
	}

	a, err := r.Ate.GetActor(ctx, &pb.GetActorRequest{Actor: actorRef(ra.Spec.Atespace, ra.Spec.ActorName)})
	if isCode(err, codes.NotFound) || (err == nil && ra.Spec.ActorUID != "" && a.GetMetadata().GetUid() != ra.Spec.ActorUID) {
		// Gone, or replaced by a new incarnation this entry does not own.
		return ctrl.Result{}, releaseRetained(ctx, r.Client, &ra)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	orig := ra.DeepCopy()
	settled, err := settle(ctx, r.Ate, a, now)
	ra.Status.Phase, ra.Status.Message = substratev1alpha1.RetainedPendingSuspend, stateName(a.GetStatus().GetState())
	if err != nil {
		ra.Status.Message = err.Error()
	}
	if settled {
		ra.Status.Phase, ra.Status.Message = substratev1alpha1.RetainedSuspended, ""
	}
	if perr := r.Status().Patch(ctx, &ra, client.MergeFrom(orig)); perr != nil {
		return ctrl.Result{}, perr
	}
	if !settled {
		return ctrl.Result{RequeueAfter: retainedPoll}, nil
	}
	return ctrl.Result{RequeueAfter: ra.Spec.ExpiresAt.Sub(now)}, nil
}

func (r *RetainedActorReconciler) purge(ctx context.Context, ra *substratev1alpha1.RetainedActor) error {
	if !controllerutil.ContainsFinalizer(ra, Finalizer) {
		return nil
	}
	_, err := r.Ate.DeleteActor(ctx, &pb.DeleteActorRequest{
		Actor:    actorRef(ra.Spec.Atespace, ra.Spec.ActorName),
		AnyState: true,
		Options:  &pb.DeleteOptions{Uid: ra.Spec.ActorUID},
	})
	// Aborted on a uid mismatch: a newer incarnation is not ours to delete.
	uidMismatch := ra.Spec.ActorUID != "" && isCode(err, codes.Aborted)
	if err != nil && !isCode(err, codes.NotFound) && !uidMismatch {
		return fmt.Errorf("delete actor: %w", err)
	}
	if err := r.collectRevisions(ctx, ra); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(ra, Finalizer)
	return r.Update(ctx, ra)
}

// collectRevisions deletes the identity's revisions no actor references,
// unless an ActorTemplate CR still owns them.
func (r *RetainedActorReconciler) collectRevisions(ctx context.Context, ra *substratev1alpha1.RetainedActor) error {
	prefix := naming.RevisionPrefixForActor(ra.Spec.ActorName)
	var templates substratev1alpha1.ActorTemplateList
	if err := r.List(ctx, &templates, client.InNamespace(ra.Spec.Atespace)); err != nil {
		return err
	}
	for _, at := range templates.Items {
		if id, err := naming.IdentityFromLabels(at.Labels); err == nil && id.TemplateRevisionPrefix() == prefix {
			return nil
		}
	}
	referenced := map[string]bool{}
	actors, err := r.Ate.ListActors(ctx, &pb.ListActorsRequest{Atespace: ra.Spec.Atespace})
	if err != nil {
		return err
	}
	for _, a := range actors.GetActors() {
		referenced[a.GetActorTemplate().GetName()] = true
	}
	resp, err := r.Ate.ListActorTemplates(ctx, &pb.ListActorTemplatesRequest{Atespace: ra.Spec.Atespace})
	if err != nil {
		return err
	}
	for _, t := range resp.GetActorTemplates() {
		m := t.GetMetadata()
		if !strings.HasPrefix(m.GetName(), prefix) || referenced[m.GetName()] {
			continue
		}
		_, err := r.Ate.DeleteActorTemplate(ctx, &pb.DeleteActorTemplateRequest{
			ActorTemplate: actorRef(ra.Spec.Atespace, m.GetName()),
			Options:       &pb.DeleteOptions{Uid: m.GetUid()},
		})
		if ignoreCodes(err, codes.NotFound) != nil {
			return err
		}
	}
	return nil
}

func (r *RetainedActorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&substratev1alpha1.RetainedActor{}).
		Named("retainedactor").
		Complete(r)
}

// OrphanScanner retains operator-named actors that have neither an Actor CR
// nor a RetainedActor entry, e.g. when a finalizer was force-removed. It
// never deletes anything directly.
type OrphanScanner struct {
	client.Client
	Ate          pb.ControlClient
	Interval     time.Duration
	RetentionTTL time.Duration
	Now          func() time.Time
}

var _ manager.Runnable = &OrphanScanner{}

func (s *OrphanScanner) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("orphan-scanner")
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.Scan(ctx); err != nil {
				log.Error(err, "orphan scan failed")
			}
		}
	}
}

func (s *OrphanScanner) Scan(ctx context.Context) error {
	spaces, err := s.Ate.ListAtespaces(ctx, &pb.ListAtespacesRequest{})
	if err != nil {
		return err
	}
	for _, as := range spaces.GetAtespaces() {
		if err := s.scanAtespace(ctx, as.GetMetadata().GetName()); err != nil {
			return err
		}
	}
	return nil
}

func (s *OrphanScanner) scanAtespace(ctx context.Context, atespace string) error {
	actors, err := s.Ate.ListActors(ctx, &pb.ListActorsRequest{Atespace: atespace})
	if err != nil {
		return err
	}
	for _, a := range actors.GetActors() {
		name := a.GetMetadata().GetName()
		if !naming.IsActorName(name) {
			continue
		}
		var crs substratev1alpha1.ActorList
		if err := s.List(ctx, &crs, client.InNamespace(atespace), client.MatchingFields{actorIdentityIndex: name}); err != nil {
			return err
		}
		if len(crs.Items) > 0 {
			continue
		}
		ra := &substratev1alpha1.RetainedActor{
			ObjectMeta: metav1.ObjectMeta{Name: substratev1alpha1.RetainedActorName(atespace, name), Finalizers: []string{Finalizer}},
			Spec: substratev1alpha1.RetainedActorSpec{
				Atespace:  atespace,
				ActorName: name,
				ActorUID:  a.GetMetadata().GetUid(),
				ExpiresAt: metav1.NewTime(s.Now().Add(s.RetentionTTL)),
			},
		}
		if err := s.Create(ctx, ra); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		logr.FromContextOrDiscard(ctx).Info("retained orphaned actor", "atespace", atespace, "actor", name)
	}
	return nil
}

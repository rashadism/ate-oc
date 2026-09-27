package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	substratev1alpha1 "github.com/rashadism/oc-substrate/api/v1alpha1"
	"github.com/rashadism/oc-substrate/internal/compile"
	"github.com/rashadism/oc-substrate/internal/naming"
	pb "github.com/rashadism/oc-substrate/third_party/ateapipb"
)

const (
	Finalizer = "substrate.openchoreo.dev/cleanup"

	CondAccepted              = "Accepted"
	CondResolvedRefs          = "ResolvedRefs"
	CondSchedulable           = "Schedulable"
	CondReady                 = "Ready"
	CondSandboxConfigOutdated = "SandboxConfigOutdated"

	revisionHistory = 3
	maxRevisions    = 10
	goldenPoll      = 2 * time.Second

	secretRefIndex    = "spec.secretRefs"
	configMapRefIndex = "spec.configMapRefs"
)

type ActorTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Ate    pb.ControlClient
	// StorageLocation is the snapshot object-store prefix; each atespace gets a sub-path.
	StorageLocation string
}

// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actortemplates,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actortemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=substrate.openchoreo.dev,resources=actortemplates/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=ate.dev,resources=workerpools,verbs=get;list;watch

func (r *ActorTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var at substratev1alpha1.ActorTemplate
	if err := r.Get(ctx, req.NamespacedName, &at); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !at.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &at)
	}
	if controllerutil.AddFinalizer(&at, Finalizer) {
		if err := r.Update(ctx, &at); err != nil {
			return ctrl.Result{}, err
		}
	}

	orig := at.DeepCopy()
	res, err := r.reconcile(ctx, &at)
	at.Status.ObservedGeneration = at.Generation
	if perr := r.Status().Patch(ctx, &at, client.MergeFrom(orig)); perr != nil {
		return ctrl.Result{}, errors.Join(err, perr)
	}
	return res, err
}

func (r *ActorTemplateReconciler) reconcile(ctx context.Context, at *substratev1alpha1.ActorTemplate) (ctrl.Result, error) {
	id, err := naming.IdentityFromLabels(at.Labels)
	if err != nil {
		setCond(at, CondAccepted, false, "MissingIdentity", err.Error())
		return ctrl.Result{}, nil
	}
	if err := r.ensureAtespace(ctx, at.Namespace); err != nil {
		return ctrl.Result{}, err
	}

	pinned, err := r.pinnedSandboxConfig(ctx, at.Namespace, id)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pinned != "" && pinned != at.Spec.SandboxConfigName {
		setCond(at, CondSandboxConfigOutdated, true, "PinnedByActor",
			fmt.Sprintf("existing actor stays on %s; migrating to %s resets its state", pinned, at.Spec.SandboxConfigName))
	} else {
		setCond(at, CondSandboxConfigOutdated, false, "UpToDate", "")
	}

	compiled, err := compile.Compile(ctx, r.Client, at, compile.Options{
		StorageLocation:   strings.TrimSuffix(r.StorageLocation, "/") + "/" + at.Namespace,
		SandboxConfigName: pinned,
	})
	var refErr *compile.RefError
	switch {
	case errors.As(err, &refErr):
		setCond(at, CondResolvedRefs, false, "RefNotFound", err.Error())
		return ctrl.Result{}, nil
	case err != nil:
		setCond(at, CondAccepted, false, "Invalid", err.Error())
		return ctrl.Result{}, nil
	}
	setCond(at, CondAccepted, true, "Accepted", "")
	setCond(at, CondResolvedRefs, true, "Resolved", "")

	fits, msg, err := r.fits(ctx, at)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !fits {
		setCond(at, CondSchedulable, false, "NoFittingWorkerPool", msg)
		return ctrl.Result{}, nil
	}
	setCond(at, CondSchedulable, true, "Fits", msg)

	at.Status.DesiredRevision = compiled.Hash
	tpl, err := r.ensureRevision(ctx, compiled.Template)
	if err != nil {
		return ctrl.Result{}, err
	}
	rev := substratev1alpha1.TemplateRevision{Hash: compiled.Hash, AteName: tpl.GetMetadata().GetName(), UID: tpl.GetMetadata().GetUid()}

	golden := tpl.GetStatus().GetGoldenSnapshotStatus()
	result := ctrl.Result{}
	var goldenErr error
	switch {
	case golden.GetGoldenTag() != nil:
		rev.Ready = true
		at.Status.LatestReadyRevision = compiled.Hash
		setCond(at, CondReady, true, "GoldenReady", "")
	case golden.GetErrorMessage() != "":
		rev.Message = golden.GetErrorMessage()
		setCond(at, CondReady, false, "GoldenFailed", rev.Message)
		if err := r.deleteRevision(ctx, at.Namespace, tpl.GetMetadata()); err != nil {
			return ctrl.Result{}, err
		}
		rev.UID = ""
		// Returning an error retries with the controller's exponential backoff.
		goldenErr = fmt.Errorf("golden snapshot for %s failed: %s", rev.AteName, rev.Message)
	default:
		setCond(at, CondReady, false, "GoldenPending", "waiting for the golden snapshot")
		result.RequeueAfter = goldenPoll
	}
	recordRevision(at, rev)

	if err := r.collect(ctx, at, id); err != nil {
		return ctrl.Result{}, err
	}
	return result, goldenErr
}

func (r *ActorTemplateReconciler) finalize(ctx context.Context, at *substratev1alpha1.ActorTemplate) error {
	if !controllerutil.ContainsFinalizer(at, Finalizer) {
		return nil
	}
	// Fast path: never block deletion on Substrate. Revisions still referenced
	// (including by retained actors) stay and are swept once unreferenced.
	if id, err := naming.IdentityFromLabels(at.Labels); err == nil {
		at.Status.Revisions = nil
		at.Status.DesiredRevision, at.Status.LatestReadyRevision = "", ""
		if err := r.collect(ctx, at, id); err != nil {
			logf.FromContext(ctx).Info("revision cleanup deferred to sweeper", "err", err)
		}
	}
	controllerutil.RemoveFinalizer(at, Finalizer)
	return r.Update(ctx, at)
}

func (r *ActorTemplateReconciler) ensureAtespace(ctx context.Context, name string) error {
	_, err := r.Ate.GetAtespace(ctx, &pb.GetAtespaceRequest{Atespace: &pb.ObjectRef{Name: name}})
	if status.Code(err) != codes.NotFound {
		return err
	}
	_, err = r.Ate.CreateAtespace(ctx, &pb.CreateAtespaceRequest{Atespace: &pb.Atespace{Metadata: &pb.ResourceMetadata{Name: name}}})
	if status.Code(err) == codes.AlreadyExists {
		return nil
	}
	return err
}

// pinnedSandboxConfig returns the SandboxConfig the identity's existing actor
// runs on, or "" when there is no actor yet.
func (r *ActorTemplateReconciler) pinnedSandboxConfig(ctx context.Context, atespace string, id naming.Identity) (string, error) {
	a, err := r.Ate.GetActor(ctx, &pb.GetActorRequest{Actor: &pb.ObjectRef{Atespace: atespace, Name: id.ActorName()}})
	if status.Code(err) == codes.NotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	t, err := r.Ate.GetActorTemplate(ctx, &pb.GetActorTemplateRequest{ActorTemplate: a.GetActorTemplate()})
	if status.Code(err) == codes.NotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return t.GetSandboxConfig().GetConfigName(), nil
}

func (r *ActorTemplateReconciler) ensureRevision(ctx context.Context, want *pb.ActorTemplate) (*pb.ActorTemplate, error) {
	ref := &pb.ObjectRef{Atespace: want.GetMetadata().GetAtespace(), Name: want.GetMetadata().GetName()}
	t, err := r.Ate.GetActorTemplate(ctx, &pb.GetActorTemplateRequest{ActorTemplate: ref})
	if status.Code(err) != codes.NotFound {
		return t, err
	}
	t, err = r.Ate.CreateActorTemplate(ctx, &pb.CreateActorTemplateRequest{ActorTemplate: want})
	if status.Code(err) == codes.AlreadyExists {
		return r.Ate.GetActorTemplate(ctx, &pb.GetActorTemplateRequest{ActorTemplate: ref})
	}
	return t, err
}

func (r *ActorTemplateReconciler) deleteRevision(ctx context.Context, atespace string, m *pb.ResourceMetadata) error {
	_, err := r.Ate.DeleteActorTemplate(ctx, &pb.DeleteActorTemplateRequest{
		ActorTemplate: &pb.ObjectRef{Atespace: atespace, Name: m.GetName()},
		Options:       &pb.DeleteOptions{Uid: m.GetUid()},
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// collect deletes this identity's revisions that no Substrate actor references
// and that fall outside the desired, latest-ready and recent history set.
// Substrate itself does not check references before deleting a template.
func (r *ActorTemplateReconciler) collect(ctx context.Context, at *substratev1alpha1.ActorTemplate, id naming.Identity) error {
	referenced, err := r.referencedTemplates(ctx, at.Namespace)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for i, rev := range at.Status.Revisions {
		if i < revisionHistory || rev.Hash == at.Status.DesiredRevision || rev.Hash == at.Status.LatestReadyRevision {
			keep[rev.AteName] = true
		}
	}

	prefix := id.TemplateRevisionPrefix()
	var token string
	for {
		resp, err := r.Ate.ListActorTemplates(ctx, &pb.ListActorTemplatesRequest{Atespace: at.Namespace, PageToken: token})
		if err != nil {
			return err
		}
		for _, t := range resp.GetActorTemplates() {
			name := t.GetMetadata().GetName()
			if !strings.HasPrefix(name, prefix) || keep[name] || referenced[name] {
				continue
			}
			if err := r.deleteRevision(ctx, at.Namespace, t.GetMetadata()); err != nil {
				return err
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	at.Status.Revisions = slices.DeleteFunc(at.Status.Revisions, func(rev substratev1alpha1.TemplateRevision) bool {
		return !keep[rev.AteName] && !referenced[rev.AteName]
	})
	return nil
}

func (r *ActorTemplateReconciler) referencedTemplates(ctx context.Context, atespace string) (map[string]bool, error) {
	refs := map[string]bool{}
	var token string
	for {
		resp, err := r.Ate.ListActors(ctx, &pb.ListActorsRequest{Atespace: atespace, PageToken: token})
		if err != nil {
			return nil, err
		}
		for _, a := range resp.GetActors() {
			refs[a.GetActorTemplate().GetName()] = true
		}
		if token = resp.GetNextPageToken(); token == "" {
			return refs, nil
		}
	}
}

// recordRevision puts rev first in the status history, newest first.
func recordRevision(at *substratev1alpha1.ActorTemplate, rev substratev1alpha1.TemplateRevision) {
	revs := slices.DeleteFunc(at.Status.Revisions, func(r substratev1alpha1.TemplateRevision) bool { return r.Hash == rev.Hash })
	revs = append([]substratev1alpha1.TemplateRevision{rev}, revs...)
	if len(revs) > maxRevisions {
		revs = revs[:maxRevisions]
	}
	at.Status.Revisions = revs
}

func setCond(at *substratev1alpha1.ActorTemplate, t string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&at.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: at.Generation,
	})
}

func refNames(at *substratev1alpha1.ActorTemplate, secret bool) []string {
	var out []string
	for _, c := range at.Spec.Containers {
		for _, src := range c.EnvFrom {
			if secret && src.SecretRef != nil {
				out = append(out, src.SecretRef.Name)
			}
			if !secret && src.ConfigMapRef != nil {
				out = append(out, src.ConfigMapRef.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if secret && e.ValueFrom.SecretKeyRef != nil {
				out = append(out, e.ValueFrom.SecretKeyRef.Name)
			}
			if !secret && e.ValueFrom.ConfigMapKeyRef != nil {
				out = append(out, e.ValueFrom.ConfigMapKeyRef.Name)
			}
		}
	}
	return out
}

func (r *ActorTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	for field, secret := range map[string]bool{secretRefIndex: true, configMapRefIndex: false} {
		if err := idx.IndexField(context.Background(), &substratev1alpha1.ActorTemplate{}, field, func(o client.Object) []string {
			return refNames(o.(*substratev1alpha1.ActorTemplate), secret)
		}); err != nil {
			return err
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&substratev1alpha1.ActorTemplate{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.referencing(secretRefIndex))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.referencing(configMapRefIndex))).
		Named("actortemplate").
		Complete(r)
}

func (r *ActorTemplateReconciler) referencing(field string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []ctrl.Request {
		var list substratev1alpha1.ActorTemplateList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{field: o.GetName()}); err != nil {
			return nil
		}
		reqs := make([]ctrl.Request, 0, len(list.Items))
		for _, at := range list.Items {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&at)})
		}
		return reqs
	}
}

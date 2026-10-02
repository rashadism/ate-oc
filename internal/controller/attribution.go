package controller

import (
	"context"
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rashadism/ate-oc/internal/naming"
)

// AttributionConfigMapName is the shared, cross-namespace ConfigMap that
// publishes component/project/environment name->UID mappings for log
// backends that can only recover names (not UIDs) from an actor's log
// identity. See SCRATCHPAD.md "Logs - component-UID attribution".
const AttributionConfigMapName = "actor-identity-attribution"

// attributionKey builds a ConfigMap data key safe to merge-patch
// independently of every other key: each reconcile only ever touches the
// keys this one ActorTemplate's identity owns.
func attributionKey(kind string, parts ...string) string {
	return kind + "." + strings.Join(parts, ".")
}

// recordIdentity publishes this identity's name->UID entries into the shared
// attribution ConfigMap, merge-patching only its own keys so concurrent
// reconciles of other components never race. A no-op when AttributionNamespace
// isn't configured, or the identity has no readable names yet (nothing a log
// backend could look up by name in that case anyway).
func (r *ActorTemplateReconciler) recordIdentity(ctx context.Context, id naming.Identity) error {
	if r.AttributionNamespace == "" || id.Component == "" || id.Environment == "" {
		return nil
	}
	entries := map[string]string{
		attributionKey("environment", id.Environment): id.EnvironmentUID,
	}
	if id.Project != "" {
		entries[attributionKey("project", id.Project)] = id.ProjectUID
		entries[attributionKey("component", id.Project, id.Environment, id.Component)] = id.ComponentUID
		if id.Namespace != "" {
			entries[attributionKey("namespace", id.Project)] = id.Namespace
		}
	}
	return r.patchAttribution(ctx, entries)
}

// removeIdentity drops this identity's component entry on ActorTemplate
// deletion. Project/environment entries are left alone since other
// components in the same scope still need them; an orphaned entry is
// otherwise harmless, never looked up again once nothing recreates it.
func (r *ActorTemplateReconciler) removeIdentity(ctx context.Context, id naming.Identity) error {
	if r.AttributionNamespace == "" || id.Component == "" || id.Environment == "" || id.Project == "" {
		return nil
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: AttributionConfigMapName, Namespace: r.AttributionNamespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	key := attributionKey("component", id.Project, id.Environment, id.Component)
	if _, ok := cm.Data[key]; !ok {
		return nil
	}
	delete(cm.Data, key)
	return r.Update(ctx, cm)
}

// patchAttribution merges entries into the shared ConfigMap's data, creating
// it first if a prior deletion (or first run) left it missing. Get-or-create
// on every call, rather than only at creation time, is what makes the
// ConfigMap self-heal from an accidental delete within one reconcile of any
// single ActorTemplate -- this operator already relies on its periodic
// resync (refResync) rather than a watch for this class of external state,
// same as Secret/ConfigMap refs elsewhere in this reconciler.
func (r *ActorTemplateReconciler) patchAttribution(ctx context.Context, entries map[string]string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: AttributionConfigMapName, Namespace: r.AttributionNamespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		cm.Data = entries
		if err := r.Create(ctx, cm); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return r.mergePatchAttribution(ctx, entries)
			}
			return err
		}
		return nil
	}
	return r.mergePatchAttribution(ctx, entries)
}

func (r *ActorTemplateReconciler) mergePatchAttribution(ctx context.Context, entries map[string]string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: AttributionConfigMapName, Namespace: r.AttributionNamespace,
	}}
	patch, err := json.Marshal(map[string]any{"data": entries})
	if err != nil {
		return err
	}
	return r.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch))
}

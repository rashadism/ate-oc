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

// AttributionConfigMapName is the shared, cross-namespace ConfigMap that lets
// a log backend resolve an actor's identity without parsing it: entries are
// keyed by the exact atespace or actor-name string already present on a log
// line, never by a value split out of either. See SCRATCHPAD.md "Logs -
// component-UID attribution".
const AttributionConfigMapName = "actor-identity-attribution"

// attributionKey builds a ConfigMap data key safe to merge-patch
// independently of every other key: each reconcile only ever touches the
// keys this one ActorTemplate's identity owns.
func attributionKey(kind string, parts ...string) string {
	return kind + "." + strings.Join(parts, ".")
}

// recordIdentity publishes this identity into the shared attribution
// ConfigMap, merge-patching only its own keys so concurrent reconciles of
// other components never race. A no-op when AttributionNamespace isn't
// configured, or the identity has no readable names yet.
//
// Keys are the exact, opaque strings a log backend already has on hand --
// the full atespace and the full actor name -- never a value split out of
// either. A log backend has no reliable way to split "dp-<cpNs>-<project>-
// <environment>-<hash>" or "<component>-<environment>-a-<hash>" into parts:
// component, project, and environment names are themselves free-form
// Kubernetes names that may contain hyphens (OpenChoreo does not forbid
// it), so no delimiter choice is unambiguous. Keying by the whole string
// instead of a parsed piece of it removes the need to parse at all.
func (r *ActorTemplateReconciler) recordIdentity(ctx context.Context, id naming.Identity, atespace string) error {
	if r.AttributionNamespace == "" || id.Component == "" || id.Environment == "" {
		return nil
	}
	entries := map[string]string{
		attributionKey("environment", id.Environment): id.EnvironmentUID,
		attributionKey("actorname", id.ActorName()):   id.Component + "|" + id.ComponentUID,
	}
	if id.Project != "" {
		entries[attributionKey("project", id.Project)] = id.ProjectUID
		if id.Namespace != "" {
			entries[attributionKey("namespace", id.Project)] = id.Namespace
		}
		if atespace != "" {
			entries[attributionKey("atespace", atespace)] = id.Project + "|" + id.Environment
		}
	}
	return r.patchAttribution(ctx, entries)
}

// removeIdentity drops this identity's actor-name entry on ActorTemplate
// deletion. Environment/project/atespace entries are left alone since other
// components in the same scope still need them; an orphaned entry is
// otherwise harmless, never looked up again once nothing recreates it.
func (r *ActorTemplateReconciler) removeIdentity(ctx context.Context, id naming.Identity) error {
	if r.AttributionNamespace == "" || id.Component == "" || id.Environment == "" {
		return nil
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: AttributionConfigMapName, Namespace: r.AttributionNamespace,
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	key := attributionKey("actorname", id.ActorName())
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

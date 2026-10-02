package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	substratev1alpha1 "github.com/rashadism/ate-oc/api/v1alpha1"
	"github.com/rashadism/ate-oc/internal/naming"
)

const attributionNS = "openchoreo-observability-plane"

func withReadableLabels(at *substratev1alpha1.ActorTemplate) *substratev1alpha1.ActorTemplate {
	at.Labels[naming.LabelComponentName] = "counter-x"
	at.Labels[naming.LabelEnvironmentName] = "development"
	at.Labels[naming.LabelProjectUID] = "proj-uid"
	at.Labels[naming.LabelProjectName] = "default"
	at.Labels[naming.LabelNamespace] = "default"
	return at
}

func (e *env) attributionData() map[string]string {
	e.t.Helper()
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: attributionNS, Name: AttributionConfigMapName}
	if err := e.r.Get(context.Background(), key, &cm); err != nil {
		return nil
	}
	return cm.Data
}

func TestRecordIdentityPublishesNameToUID(t *testing.T) {
	e := newEnv(t, true, withReadableLabels(actorTemplate()))
	e.r.AttributionNamespace = attributionNS
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	data := e.attributionData()
	want := map[string]string{
		"component.default.development.counter-x": id.ComponentUID,
		"project.default":                         "proj-uid",
		"environment.development":                 id.EnvironmentUID,
		"namespace.default":                       "default",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%q] = %q, want %q", k, data[k], v)
		}
	}
}

func TestRecordIdentityNoOpWithoutReadableNames(t *testing.T) {
	e := newEnv(t, true, actorTemplate()) // no component/environment/project name labels
	e.r.AttributionNamespace = attributionNS
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if data := e.attributionData(); data != nil {
		t.Fatalf("expected no ConfigMap, got %v", data)
	}
}

func TestRecordIdentityNoOpWithoutConfiguredNamespace(t *testing.T) {
	e := newEnv(t, true, withReadableLabels(actorTemplate())) // AttributionNamespace left empty
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if data := e.attributionData(); data != nil {
		t.Fatalf("expected no ConfigMap, got %v", data)
	}
}

func TestRecordIdentitySelfHealsDeletedConfigMap(t *testing.T) {
	e := newEnv(t, true, withReadableLabels(actorTemplate()))
	e.r.AttributionNamespace = attributionNS
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: attributionNS, Name: AttributionConfigMapName}
	if err := e.r.Get(context.Background(), key, &cm); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Delete(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	if data := e.attributionData(); data != nil {
		t.Fatalf("expected the ConfigMap gone after delete, got %v", data)
	}

	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	data := e.attributionData()
	if data["component.default.development.counter-x"] != id.ComponentUID {
		t.Fatalf("expected the entry to come back after one reconcile, got %v", data)
	}
}

func TestFinalizeRemovesComponentEntryOnly(t *testing.T) {
	e := newEnv(t, true, withReadableLabels(actorTemplate()))
	e.r.AttributionNamespace = attributionNS
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Delete(context.Background(), e.get()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	data := e.attributionData()
	if _, ok := data["component.default.development.counter-x"]; ok {
		t.Fatal("component entry should be removed once its ActorTemplate is deleted")
	}
	if data["environment.development"] != id.EnvironmentUID {
		t.Fatal("environment entry should survive one component's deletion")
	}
	if data["project.default"] != "proj-uid" {
		t.Fatal("project entry should survive one component's deletion")
	}
}

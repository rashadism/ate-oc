package v1alpha1_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

type crdSchema struct {
	structural *structuralschema.Structural
	validator  validation.SchemaValidator
}

func loadCRD(t *testing.T, file string) crdSchema {
	t.Helper()
	raw, err := os.ReadFile("../../helm/crds/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return crdSchema{structural: s, validator: v}
}

func (c crdSchema) validate(t *testing.T, obj, old string) (map[string]any, field.ErrorList) {
	t.Helper()
	o := parse(t, obj)
	structuraldefaulting.Default(o, c.structural)
	errs := validation.ValidateCustomResource(nil, o, c.validator)
	var oldObj any
	if old != "" {
		oo := parse(t, old)
		structuraldefaulting.Default(oo, c.structural)
		oldObj = oo
	}
	celErrs, _ := cel.NewValidator(c.structural, true, celconfig.PerCallLimit).
		Validate(context.Background(), nil, c.structural, o, oldObj, celconfig.RuntimeCELCostBudget)
	return o, append(errs, celErrs...)
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const validTemplate = `
apiVersion: substrate.openchoreo.dev/v1alpha1
kind: ActorTemplate
metadata: {name: t}
spec:
  sandboxClass: gvisor
  sandboxConfigName: gvisor-default
  resources: {limits: {cpu: "1", memory: 512Mi}}
  snapshot: {}
  containers:
    - name: main
      image: ghcr.io/x/app:1
      env:
        - {name: A, value: "1"}
        - {name: B, valueFrom: {secretKeyRef: {name: s, key: k}}}
`

func TestActorTemplateSchema(t *testing.T) {
	crd := loadCRD(t, "substrate.openchoreo.dev_actortemplates.yaml")

	tests := []struct {
		name    string
		obj     string
		old     string
		wantErr string
	}{
		{name: "valid", obj: validTemplate},
		{
			name:    "missing memory limit",
			obj:     strings.Replace(validTemplate, `, memory: 512Mi`, ``, 1),
			wantErr: "cpu and memory limits are required",
		},
		{
			name:    "value and valueFrom",
			obj:     strings.Replace(validTemplate, `{name: B, valueFrom`, `{name: B, value: x, valueFrom`, 1),
			wantErr: "mutually exclusive",
		},
		{name: "empty env value", obj: strings.Replace(validTemplate, `{name: A, value: "1"}`, `{name: A}`, 1)},
		{
			name: "envFrom",
			obj:  strings.Replace(validTemplate, `      env:`, "      envFrom:\n        - {configMapRef: {name: c}}\n        - {secretRef: {name: s}}\n      env:", 1),
		},
		{
			name:    "envFrom with both refs",
			obj:     strings.Replace(validTemplate, `      env:`, "      envFrom:\n        - {configMapRef: {name: c}, secretRef: {name: s}}\n      env:", 1),
			wantErr: "exactly one of secretRef or configMapRef",
		},
		{
			name:    "both key refs",
			obj:     strings.Replace(validTemplate, `{secretKeyRef: {name: s, key: k}}`, `{secretKeyRef: {name: s, key: k}, configMapKeyRef: {name: c, key: k}}`, 1),
			wantErr: "exactly one of secretKeyRef or configMapKeyRef",
		},
		{
			name:    "unknown sandbox class",
			obj:     strings.Replace(validTemplate, `sandboxClass: gvisor`, `sandboxClass: kata`, 1),
			wantErr: "sandboxClass",
		},
		{
			name:    "sandbox class change",
			obj:     strings.Replace(validTemplate, `sandboxClass: gvisor`, `sandboxClass: microvm`, 1),
			old:     validTemplate,
			wantErr: "sandboxClass is immutable",
		},
		{
			name:    "bad container name",
			obj:     strings.Replace(validTemplate, `name: main`, `name: Main_1`, 1),
			wantErr: "containers[0].name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := crd.validate(t, tt.obj, tt.old)
			assertErr(t, errs, tt.wantErr)
		})
	}

	t.Run("snapshot defaults", func(t *testing.T) {
		obj, _ := crd.validate(t, validTemplate, "")
		snap := obj["spec"].(map[string]any)["snapshot"].(map[string]any)
		want := map[string]string{"onPause": "Full", "onCommit": "Data", "onResumeFromData": "Golden"}
		for k, v := range want {
			if snap[k] != v {
				t.Errorf("snapshot.%s = %v, want %s", k, snap[k], v)
			}
		}
	})
}

const validActor = `
apiVersion: substrate.openchoreo.dev/v1alpha1
kind: Actor
metadata: {name: a}
spec:
  templateRef: {name: t}
  endpoints:
    - {name: http, port: 8080, visibility: [project, external]}
`

func TestActorSchema(t *testing.T) {
	crd := loadCRD(t, "substrate.openchoreo.dev_actors.yaml")

	tests := []struct {
		name    string
		obj     string
		wantErr string
	}{
		{name: "valid", obj: validActor},
		{
			name:    "unknown visibility",
			obj:     strings.Replace(validActor, `external]`, `public]`, 1),
			wantErr: "visibility",
		},
		{
			name:    "port out of range",
			obj:     strings.Replace(validActor, `port: 8080`, `port: 70000`, 1),
			wantErr: "port",
		},
		{
			name:    "missing templateRef",
			obj:     strings.Replace(validActor, `templateRef: {name: t}`, `paused: true`, 1),
			wantErr: "templateRef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := crd.validate(t, tt.obj, "")
			assertErr(t, errs, tt.wantErr)
		})
	}

	t.Run("idleTimeout default", func(t *testing.T) {
		obj, _ := crd.validate(t, validActor, "")
		if got := obj["spec"].(map[string]any)["idleTimeout"]; got != "5m" {
			t.Errorf("idleTimeout = %v, want 5m", got)
		}
	})
}

func assertErr(t *testing.T, errs field.ErrorList, want string) {
	t.Helper()
	if want == "" {
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		return
	}
	if !strings.Contains(errs.ToAggregate().Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, errs)
	}
}

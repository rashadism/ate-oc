package naming

import (
	"regexp"
	"testing"
)

var shortName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

func TestIdentityFromLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "both", labels: map[string]string{LabelComponentUID: "c", LabelEnvironmentUID: "e"}},
		{name: "missing env", labels: map[string]string{LabelComponentUID: "c"}, wantErr: true},
		{name: "missing component", labels: map[string]string{LabelEnvironmentUID: "e"}, wantErr: true},
		{name: "empty value", labels: map[string]string{LabelComponentUID: "", LabelEnvironmentUID: "e"}, wantErr: true},
		{name: "nil", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := IdentityFromLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNames(t *testing.T) {
	a := Identity{ComponentUID: "0b7f5c3e-1111-4c1a-9f00-000000000001", EnvironmentUID: "7d2c1a90-2222-4b2b-8e00-000000000002"}
	b := Identity{ComponentUID: "0b7f5c3e-1111-4c1a-9f00-000000000009", EnvironmentUID: a.EnvironmentUID}
	hash := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	same := Identity{ComponentUID: a.ComponentUID, EnvironmentUID: a.EnvironmentUID}
	if a.ActorName() != same.ActorName() {
		t.Fatal("actor name not deterministic")
	}
	if a.ActorName() == b.ActorName() {
		t.Fatal("different component UIDs must yield different actor names")
	}
	swapped := Identity{ComponentUID: a.EnvironmentUID, EnvironmentUID: a.ComponentUID}
	if a.ActorName() == swapped.ActorName() {
		t.Fatal("component and environment UIDs must not be interchangeable")
	}
	for _, n := range []string{a.ActorName(), a.TemplateRevisionName(hash)} {
		if !shortName.MatchString(n) {
			t.Errorf("%q is not a valid short name", n)
		}
	}
	if got, want := a.TemplateRevisionName(hash), "t-"+a.ActorName()[2:]+"-9f86d081"; got != want {
		t.Errorf("revision name = %q, want %q", got, want)
	}
}

func TestActorNameHelpers(t *testing.T) {
	id := Identity{ComponentUID: "c", EnvironmentUID: "e"}
	if !IsActorName(id.ActorName()) || IsActorName("my-counter-1") || IsActorName("a-XYZ") {
		t.Fatal("IsActorName misclassifies")
	}
	if RevisionPrefixForActor(id.ActorName()) != id.TemplateRevisionPrefix() {
		t.Fatal("revision prefix not recoverable from actor name")
	}
}

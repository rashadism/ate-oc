package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	LabelComponentUID    = "openchoreo.dev/component-uid"
	LabelEnvironmentUID  = "openchoreo.dev/environment-uid"
	LabelProjectUID      = "openchoreo.dev/project-uid"
	LabelComponentName   = "openchoreo.dev/component"
	LabelEnvironmentName = "openchoreo.dev/environment"
	LabelProjectName     = "openchoreo.dev/project"
	LabelNamespace       = "openchoreo.dev/namespace"
)

// Identity is the component × environment pair an ActorTemplate or Actor
// belongs to. Uniqueness comes from the UIDs, so a recreated same-named
// component can never reach state left by its predecessor; Component/
// Environment/Project/Namespace only make ActorName readable and identity
// attributable elsewhere, never affect uniqueness. Project and Namespace are
// optional: ActorName doesn't use either, only the attribution-table writer
// does. Namespace is the project's own Kubernetes namespace -- a separate
// value from Project's name, even though they're often equal in practice.
type Identity struct {
	ComponentUID   string
	EnvironmentUID string
	ProjectUID     string
	Component      string
	Environment    string
	Project        string
	Namespace      string
}

func IdentityFromLabels(labels map[string]string) (Identity, error) {
	id := Identity{
		ComponentUID:   labels[LabelComponentUID],
		EnvironmentUID: labels[LabelEnvironmentUID],
		ProjectUID:     labels[LabelProjectUID],
		Component:      labels[LabelComponentName],
		Environment:    labels[LabelEnvironmentName],
		Project:        labels[LabelProjectName],
		Namespace:      labels[LabelNamespace],
	}
	if id.ComponentUID == "" || id.EnvironmentUID == "" {
		return Identity{}, fmt.Errorf("labels %s and %s are required", LabelComponentUID, LabelEnvironmentUID)
	}
	return id, nil
}

func (id Identity) hash10() string {
	sum := sha256.Sum256([]byte(id.ComponentUID + "/" + id.EnvironmentUID))
	return hex.EncodeToString(sum[:])[:10]
}

// ActorName is readable when the component/environment names are known,
// falling back to the opaque shape otherwise. Either way it ends in
// "-a-<hash10>" (or is exactly "a-<hash10>"), which IsActorName and
// RevisionPrefixForActor rely on to recognize and parse it without needing
// the names back.
func (id Identity) ActorName() string {
	if id.Component == "" || id.Environment == "" {
		return "a-" + id.hash10()
	}
	return id.Component + "-" + id.Environment + "-a-" + id.hash10()
}

// TemplateRevisionName names an immutable Substrate ActorTemplate for one spec hash.
func (id Identity) TemplateRevisionName(specHash string) string {
	return id.TemplateRevisionPrefix() + specHash[:8]
}

func (id Identity) TemplateRevisionPrefix() string {
	return "t-" + id.hash10() + "-"
}

// IsActorName reports whether name has a shape ActorName produces, readable
// or opaque.
func IsActorName(name string) bool {
	return legacyActorName.MatchString(name) || actorNameSuffix.MatchString(name)
}

// RevisionPrefixForActor recovers the revision prefix of the identity that
// owns an actor, without knowing its UIDs or names.
func RevisionPrefixForActor(name string) string {
	if m := actorNameSuffix.FindStringSubmatch(name); m != nil {
		return "t-" + m[1] + "-"
	}
	return "t-" + strings.TrimPrefix(name, "a-") + "-"
}

var (
	legacyActorName = regexp.MustCompile(`^a-[0-9a-f]{10}$`)
	actorNameSuffix = regexp.MustCompile(`-a-([0-9a-f]{10})$`)
)

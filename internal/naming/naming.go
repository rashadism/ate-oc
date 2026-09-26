package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	LabelComponentUID   = "openchoreo.dev/component-uid"
	LabelEnvironmentUID = "openchoreo.dev/environment-uid"
)

// Identity is the component × environment pair an ActorTemplate or Actor
// belongs to. It is derived from UIDs so a recreated same-named component can
// never reach state left by its predecessor.
type Identity struct {
	ComponentUID   string
	EnvironmentUID string
}

func IdentityFromLabels(labels map[string]string) (Identity, error) {
	id := Identity{ComponentUID: labels[LabelComponentUID], EnvironmentUID: labels[LabelEnvironmentUID]}
	if id.ComponentUID == "" || id.EnvironmentUID == "" {
		return Identity{}, fmt.Errorf("labels %s and %s are required", LabelComponentUID, LabelEnvironmentUID)
	}
	return id, nil
}

func (id Identity) hash10() string {
	sum := sha256.Sum256([]byte(id.ComponentUID + "/" + id.EnvironmentUID))
	return hex.EncodeToString(sum[:])[:10]
}

func (id Identity) ActorName() string {
	return "a-" + id.hash10()
}

// TemplateRevisionName names an immutable Substrate ActorTemplate for one spec hash.
func (id Identity) TemplateRevisionName(specHash string) string {
	return "t-" + id.hash10() + "-" + specHash[:8]
}

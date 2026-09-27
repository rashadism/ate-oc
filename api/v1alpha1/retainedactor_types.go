package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ReleasedAnnotation marks an entry being dropped without a purge.
	ReleasedAnnotation = "substrate.openchoreo.dev/released"

	RetainedPendingSuspend = "PendingSuspend"
	RetainedSuspended      = "Suspended"
)

// RetainedActorSpec records a Substrate actor whose Actor CR is gone. The
// actor keeps its state until the entry expires or is deleted (purge), or an
// Actor CR with the same component and environment UIDs re-attaches it.
type RetainedActorSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="atespace is immutable"
	Atespace string `json:"atespace"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="actorName is immutable"
	ActorName string `json:"actorName"`
	// ActorUID is the Substrate uid of the retained incarnation; purge never
	// deletes an actor with a different uid.
	// +optional
	ActorUID string `json:"actorUID,omitempty"`
	// +optional
	ComponentUID string `json:"componentUID,omitempty"`
	// +optional
	EnvironmentUID string      `json:"environmentUID,omitempty"`
	ExpiresAt      metav1.Time `json:"expiresAt"`
}

type RetainedActorStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ract
// +kubebuilder:printcolumn:name="Atespace",type=string,JSONPath=`.spec.atespace`
// +kubebuilder:printcolumn:name="Actor",type=string,JSONPath=`.spec.actorName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.spec.expiresAt`

type RetainedActor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RetainedActorSpec   `json:"spec"`
	Status RetainedActorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type RetainedActorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RetainedActor `json:"items"`
}

// RetainedActorName is the cluster-unique entry name for an actor.
func RetainedActorName(atespace, actorName string) string {
	return atespace + "." + actorName
}

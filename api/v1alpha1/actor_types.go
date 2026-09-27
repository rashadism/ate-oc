package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=project;namespace;internal;external
type EndpointVisibility string

const (
	VisibilityProject   EndpointVisibility = "project"
	VisibilityNamespace EndpointVisibility = "namespace"
	VisibilityInternal  EndpointVisibility = "internal"
	VisibilityExternal  EndpointVisibility = "external"
)

type ActorSpec struct {
	TemplateRef LocalRef `json:"templateRef"`

	// ServiceName is the selector-less Service that fronts this actor; the
	// operator points its EndpointSlice at the front door.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// +kubebuilder:default="5m"
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// +optional
	Paused bool `json:"paused,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +optional
	Endpoints []Endpoint `json:"endpoints,omitempty"`
}

type LocalRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type Endpoint struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Port is the Service port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// TargetPort is the port the actor listens on; defaults to Port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	TargetPort int32 `json:"targetPort,omitempty"`
	// Visibility lists scopes beyond project, which always applies.
	// +optional
	Visibility []EndpointVisibility `json:"visibility,omitempty"`
}

func (e Endpoint) ActorPort() int32 {
	if e.TargetPort != 0 {
		return e.TargetPort
	}
	return e.Port
}

type ActorStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Name string `json:"name,omitempty"`

	// +optional
	UID string `json:"uid,omitempty"`

	// +optional
	State string `json:"state,omitempty"`

	// +optional
	Revision string `json:"revision,omitempty"`

	// +optional
	LastActiveTime *metav1.Time `json:"lastActiveTime,omitempty"`

	// +optional
	CrashReason string `json:"crashReason,omitempty"`

	// +optional
	CrashTime *metav1.Time `json:"crashTime,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=act
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.revision`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Actor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ActorSpec   `json:"spec"`
	Status ActorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ActorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Actor `json:"items"`
}

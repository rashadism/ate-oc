package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=gvisor;microvm
type SandboxClass string

const (
	SandboxClassGVisor  SandboxClass = "gvisor"
	SandboxClassMicroVM SandboxClass = "microvm"
)

// +kubebuilder:validation:Enum=Full;Data
type SnapshotScope string

const (
	SnapshotScopeFull SnapshotScope = "Full"
	SnapshotScopeData SnapshotScope = "Data"
)

// +kubebuilder:validation:Enum=Golden;ColdBoot
type ResumeSource string

const (
	ResumeSourceGolden   ResumeSource = "Golden"
	ResumeSourceColdBoot ResumeSource = "ColdBoot"
)

type ActorTemplateSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sandboxClass is immutable"
	SandboxClass SandboxClass `json:"sandboxClass"`

	// +kubebuilder:validation:MinLength=1
	SandboxConfigName string `json:"sandboxConfigName"`

	// +optional
	WorkerSelector *metav1.LabelSelector `json:"workerSelector,omitempty"`

	Resources ActorResources `json:"resources"`

	// +optional
	Snapshot *SnapshotPolicy `json:"snapshot,omitempty"`

	// +optional
	DurableDir *DurableDir `json:"durableDir,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +listType=map
	// +listMapKey=name
	Containers []Container `json:"containers"`
}

type ActorResources struct {
	// +kubebuilder:validation:XValidation:rule="has(self.cpu) && has(self.memory)",message="cpu and memory limits are required"
	Limits map[corev1.ResourceName]resource.Quantity `json:"limits"`
}

type SnapshotPolicy struct {
	// +kubebuilder:default=Full
	// +optional
	OnPause SnapshotScope `json:"onPause,omitempty"`
	// +kubebuilder:default=Data
	// +optional
	OnCommit SnapshotScope `json:"onCommit,omitempty"`
	// +kubebuilder:default=Golden
	// +optional
	OnResumeFromData ResumeSource `json:"onResumeFromData,omitempty"`
}

type DurableDir struct {
	// +kubebuilder:validation:Pattern=`^/.*`
	MountPath string `json:"mountPath"`
}

type Container struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// +optional
	Command []string `json:"command,omitempty"`

	// +optional
	Args []string `json:"args,omitempty"`

	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// EnvFrom adds every key of a ConfigMap or Secret; Env entries win on conflict.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	EnvFrom []EnvFromSource `json:"envFrom,omitempty"`

	// +optional
	WakeupProbe *WakeupProbe `json:"wakeupProbe,omitempty"`

	// +optional
	Capabilities []corev1.Capability `json:"capabilities,omitempty"`
}

// A variable with neither value nor valueFrom is set to the empty string, as
// OpenChoreo omits empty values when it renders env.
// +kubebuilder:validation:XValidation:rule="!(has(self.value) && has(self.valueFrom))",message="value and valueFrom are mutually exclusive"
type EnvVar struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +optional
	Value *string `json:"value,omitempty"`

	// +optional
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.secretKeyRef) != has(self.configMapKeyRef)",message="exactly one of secretKeyRef or configMapKeyRef is required"
type EnvVarSource struct {
	// +optional
	SecretKeyRef *KeyRef `json:"secretKeyRef,omitempty"`
	// +optional
	ConfigMapKeyRef *KeyRef `json:"configMapKeyRef,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.secretRef) != has(self.configMapRef)",message="exactly one of secretRef or configMapRef is required"
type EnvFromSource struct {
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`
	// +optional
	ConfigMapRef *LocalRef `json:"configMapRef,omitempty"`
}

type KeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

type WakeupProbe struct {
	HTTPGet HTTPGetAction `json:"httpGet"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type HTTPGetAction struct {
	// +kubebuilder:default=/
	// +optional
	Path string `json:"path,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

type TemplateRevision struct {
	Hash    string `json:"hash"`
	AteName string `json:"ateName"`
	// +optional
	UID string `json:"uid,omitempty"`
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

type ActorTemplateStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	DesiredRevision string `json:"desiredRevision,omitempty"`

	// +optional
	LatestReadyRevision string `json:"latestReadyRevision,omitempty"`

	// +kubebuilder:validation:MaxItems=10
	// +listType=map
	// +listMapKey=hash
	// +optional
	Revisions []TemplateRevision `json:"revisions,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=atpl
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.sandboxClass`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.latestReadyRevision`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type ActorTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ActorTemplateSpec   `json:"spec"`
	Status ActorTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ActorTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActorTemplate `json:"items"`
}

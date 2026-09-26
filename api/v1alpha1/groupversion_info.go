// Package v1alpha1 contains the substrate.openchoreo.dev v1alpha1 API.
// +kubebuilder:object:generate=true
// +groupName=substrate.openchoreo.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "substrate.openchoreo.dev", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &ActorTemplate{}, &ActorTemplateList{}, &Actor{}, &ActorList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

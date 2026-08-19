// Package v1alpha1 contains the ModelDeployment API for serving.lpogosu.dev.
// +kubebuilder:object:generate=true
// +groupName=serving.lpogosu.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group/version this API belongs to.
	GroupVersion = schema.GroupVersion{Group: "serving.lpogosu.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this group/version with a scheme.
	// It is built by hand rather than with controller-runtime's helper so that
	// this package depends on apimachinery alone and stays cheap to import.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this group/version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &ModelDeployment{}, &ModelDeploymentList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

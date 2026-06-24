/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ThunderIDInstanceSpec defines the desired state of ThunderIDInstance
type ThunderIDInstanceSpec struct {

	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// +kubebuilder:validation:Required
	Domain string `json:"domain"`

	// +optional
	TLSSecret string `json:"tlsSecret,omitempty"`

	// +optional
	ResourcesConfigMap string `json:"resourcesConfigMap,omitempty"`

	// +optional
	ResourcesYAML string `json:"resourcesYAML,omitempty"`
}

// ThunderIDInstanceStatus defines the observed state of ThunderIDInstance.
type ThunderIDInstanceStatus struct {

	// +optional
	URL string `json:"url,omitempty"`

	// +optional
	Phase string `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ThunderIDInstance is the Schema for the thunderidinstances API
type ThunderIDInstance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ThunderIDInstance
	// +required
	Spec ThunderIDInstanceSpec `json:"spec"`

	// status defines the observed state of ThunderIDInstance
	// +optional
	Status ThunderIDInstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ThunderIDInstanceList contains a list of ThunderIDInstance
type ThunderIDInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ThunderIDInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ThunderIDInstance{}, &ThunderIDInstanceList{})
		return nil
	})
}

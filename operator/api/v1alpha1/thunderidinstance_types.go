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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type AutoScalingSpec struct {
	MinReplicas                       int32 `json:"minReplicas"`
	MaxReplicas                       int32 `json:"maxReplicas"`
	TargetCPUUtilizationPercentage    int32 `json:"targetCPUUtilizationPercentage,omitempty"`
	TargetMemoryUtilizationPercentage int32 `json:"targetMemoryUtilizationPercentage,omitempty"`
}

// ThunderIDInstanceSpec defines the desired state of ThunderIDInstance
type ThunderIDInstanceSpec struct {

	// +kubebuilder:validation:Required
	Image string `json:"image"`

	Replicas int32 `json:"replicas,omitempty"`

	// +kubebuilder:validation:Required
	Domain string `json:"domain"`

	// +optional
	TLSSecret string `json:"tlsSecret,omitempty"`

	// SecuritySecret names a pre-existing Secret holding ThunderID's TLS/JWT-signing/encryption
	// key material (server.cert, server.key, signing.cert, signing.key, ecdsa-signing.cert,
	// ecdsa-signing.key, crypto.key, direct_auth_secret). When unset, the operator generates and
	// manages this Secret itself as "<name>-security".
	// +optional
	SecuritySecret string `json:"securitySecret,omitempty"`

	// +optional
	ResourcesConfigMap string `json:"resourcesConfigMap,omitempty"`

	// DataVolumeSize, when set (e.g. "1Gi"), provisions a ReadWriteOnce PersistentVolumeClaim
	// mounted at /opt/thunderid/repository/database (where the SQLite files live) and gates
	// setup.sh to run only once: the container entrypoint checks a marker file on that volume
	// and skips setup.sh on every restart after the first, instead of rerunning it (and wiping
	// the database) on every pod restart. Because the PVC is ReadWriteOnce, the Deployment
	// strategy switches to Recreate to avoid two pods mounting it at once — only meaningful with
	// replicas: 1. Leave unset to keep the existing ephemeral-SQLite, setup.sh-every-restart
	// behavior.
	// +optional
	DataVolumeSize string `json:"dataVolumeSize,omitempty"`

	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	AutoScaling *AutoScalingSpec `json:"autoScaling,omitempty"`
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
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

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

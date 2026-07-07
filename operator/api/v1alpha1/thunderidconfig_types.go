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

type ThunderIDConfigSpec struct {
	Image              string                      `json:"image"`
	Domain             string                      `json:"domain"`
	TlsSecret          string                      `json:"tlsSecret,omitempty"`
	ResourcesConfigMap string                      `json:"resourcesConfigMap,omitempty"`
	Resources          corev1.ResourceRequirements `json:"resources,omitempty"`
	AutoScaling        *AutoScalingSpec            `json:"autoScaling,omitempty"`
}

type ThunderIDConfigStatus struct {
	Phase string `json:"phase,omitempty"`
	URL   string `json:"url,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`

type ThunderIDConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ThunderIDConfigSpec   `json:"spec,omitempty"`
	Status            ThunderIDConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ThunderIDConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ThunderIDConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ThunderIDConfig{}, &ThunderIDConfigList{})
		return nil
	})
}

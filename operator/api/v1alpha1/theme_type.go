package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ThemeSpec struct {
	// +kubebuilder:validation:Required
	InstanceRef string `json:"instanceRef"`

	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Theme string `json:"theme,omitempty"`
}

type ThemeStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	ThemeID string `json:"themeID,omitempty"`

	// +optional
	Synced bool `json:"synced,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".spec.instanceRef"
// +kubebuilder:printcolumn:name="Display Name",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type Theme struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ThemeSpec   `json:"spec,omitempty"`
	Status ThemeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ThemeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Theme `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Theme{}, &ThemeList{})
		return nil
	})
}

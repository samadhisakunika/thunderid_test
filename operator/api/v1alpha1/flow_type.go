package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type FlowSpec struct {
	// +kubebuilder:validation:Required
	InstanceRef string `json:"instanceRef"`

	// +kubebuilder:validation:Required
	Handle string `json:"handle"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=AUTHENTICATION;REGISTRATION
	FlowType string `json:"flowType"`

	// +optional
	Nodes string `json:"nodes,omitempty"`

	// +optional
	IsReadOnly bool `json:"isReadOnly,omitempty"`
}

type FlowStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	FlowID string `json:"flowID,omitempty"`

	// +optional
	Synced bool `json:"synced,omitempty"`

	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".spec.instanceRef"
// +kubebuilder:printcolumn:name="Handle",type="string",JSONPath=".spec.handle"
// +kubebuilder:printcolumn:name="Flow Type",type="string",JSONPath=".spec.flowType"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type Flow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FlowSpec   `json:"spec,omitempty"`
	Status FlowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type FlowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Flow `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Flow{}, &FlowList{})
		return nil
	})
}

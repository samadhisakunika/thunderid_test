package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type UserTypeSpec struct {
	// +kubebuilder:validation:Required
	InstanceRef string `json:"instanceRef"`

	// +kubebuilder:validation:Required
	Category string `json:"category"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	OUID string `json:"ouId,omitempty"`

	// +optional
	OUHandle string `json:"ouHandle,omitempty"`

	// +optional
	AllowSelfRegistration bool `json:"allowSelfRegistration,omitempty"`

	// +optional
	SystemAttributes map[string]string `json:"systemAttributes,omitempty"`

	// +optional
	Schema string `json:"schema,omitempty"`
}

type UserTypeStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	UserTypeID string `json:"userTypeID,omitempty"`

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
// +kubebuilder:printcolumn:name="Category",type="string",JSONPath=".spec.category"
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type UserType struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserTypeSpec   `json:"spec,omitempty"`
	Status UserTypeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type UserTypeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UserType `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &UserType{}, &UserTypeList{})
		return nil
	})
}

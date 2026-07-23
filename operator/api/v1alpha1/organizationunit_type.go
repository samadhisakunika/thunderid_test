package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type OrganizationUnitSpec struct {
	// +kubebuilder:validation:Required
	InstanceRef string `json:"instanceRef"`

	// +kubebuilder:validation:Required
	Handle string `json:"handle"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Parent string `json:"parentId,omitempty"`

	// +optional
	LogoURL string `json:"logoUrl,omitempty"`

	// +optional
	CreatedAt string `json:"createdAt,omitempty"`

	// +optional
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type OrganizationUnitStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	OrganizationUnitID string `json:"organizationUnitID,omitempty"`

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
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type OrganizationUnit struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OrganizationUnitSpec   `json:"spec,omitempty"`
	Status OrganizationUnitStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OrganizationUnitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OrganizationUnit `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &OrganizationUnit{}, &OrganizationUnitList{})
		return nil
	})
}

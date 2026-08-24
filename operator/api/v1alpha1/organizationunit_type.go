package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type OrganizationUnitSpec struct {
	// config is forwarded as-is into ThunderID's own organization unit bootstrap schema
	// (handle, name, description, parentId, logoUrl, ...) — the operator does not interpret
	// it, so ThunderID's own validation and error messages are what apply.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config runtime.RawExtension `json:"config,omitempty"`
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
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".metadata.labels['thunderid\\.io/instance']"
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

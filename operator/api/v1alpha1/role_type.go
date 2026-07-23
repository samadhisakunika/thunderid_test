package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type RolePermissionSpec struct {
	// +kubebuilder:validation:Required
	ResourceServerID string `json:"resourceServerId"`

	// +kubebuilder:validation:Required
	Permissions []string `json:"permissions"`
}

type RoleAssignmentSpec struct {
	// +kubebuilder:validation:Required
	ID string `json:"id"`

	// +kubebuilder:validation:Required
	Type string `json:"type"`
}

type RoleSpec struct {
	// +kubebuilder:validation:Required
	InstanceRef string `json:"instanceRef"`

	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	OUID string `json:"ouId,omitempty"`

	// +optional
	OUHandle string `json:"ouHandle,omitempty"`

	// +optional
	Permissions []RolePermissionSpec `json:"permissions,omitempty"`

	// +optional
	Assignments []RoleAssignmentSpec `json:"assignments,omitempty"`
}

type RoleStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	RoleID string `json:"roleID,omitempty"`

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
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type Role struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoleSpec   `json:"spec,omitempty"`
	Status RoleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Role `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Role{}, &RoleList{})
		return nil
	})
}

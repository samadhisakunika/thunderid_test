package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type GroupMemberSpec struct {
	// +kubebuilder:validation:Required
	ID string `json:"id"`

	// +kubebuilder:validation:Required
	Type string `json:"type"`
}

type GroupSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	OUID string `json:"ouId,omitempty"`

	// +optional
	OUHandle string `json:"ouHandle,omitempty"`

	// +optional
	Members []GroupMemberSpec `json:"members,omitempty"`
}

type GroupStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	GroupID string `json:"groupID,omitempty"`

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
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type Group struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GroupSpec   `json:"spec,omitempty"`
	Status GroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type GroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Group `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Group{}, &GroupList{})
		return nil
	})
}

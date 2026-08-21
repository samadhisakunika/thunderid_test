package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ResourceServerActionSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Handle string `json:"handle"`

	// +optional
	Description string `json:"description,omitempty"`
}

type ResourceServerResourceSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Handle string `json:"handle"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Parent string `json:"parent,omitempty"`

	// +optional
	Actions []ResourceServerActionSpec `json:"actions,omitempty"`
}

type ResourceServerSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Handle string `json:"handle,omitempty"`

	// +kubebuilder:validation:Required
	Identifier string `json:"identifier"`

	// +optional
	Type string `json:"type,omitempty"`

	// +optional
	OUID string `json:"ouId,omitempty"`

	// +optional
	OUHandle string `json:"ouHandle,omitempty"`

	// +optional
	Delimiter string `json:"delimiter,omitempty"`

	// +optional
	Resources []ResourceServerResourceSpec `json:"resources,omitempty"`
}

type ResourceServerStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	ResourceServerID string `json:"resourceServerID,omitempty"`

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

type ResourceServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceServerSpec   `json:"spec,omitempty"`
	Status ResourceServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ResourceServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ResourceServer{}, &ResourceServerList{})
		return nil
	})
}

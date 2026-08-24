package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ResourceStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	ID string `json:"id,omitempty"`

	// +optional
	Synced bool `json:"synced,omitempty"`

	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// CredentialsSecretRef mirrors User's old status field — only set when
	// spec.resource_type == "user".
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".metadata.labels['thunderid\\.io/instance']"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.resource_type"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

// Resource is the generic ThunderID resource kind — its spec is ThunderID's own exported
// bootstrap document, pasted in essentially as-is. Paste a document like:
//
//	resource_type: application
//	id: 01900000-0000-7000-8000-000000000060
//	ouId: 01900000-0000-7000-8000-000000000001
//	name: Console
//	...
//
// directly under spec (dropping the id: line — the operator generates that itself). The
// operator reads spec.resource_type to know which ConfigMap this belongs in and
// spec.credentialsSecretRef (only meaningful when resource_type == "user") as a k8s Secret
// reference — both keys are stripped before forwarding to ThunderID. Every other key,
// including ones this operator has never heard of, passes through completely untouched.
type Resource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec runtime.RawExtension `json:"spec,omitempty"`

	Status ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Resource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Resource{}, &ResourceList{})
		return nil
	})
}

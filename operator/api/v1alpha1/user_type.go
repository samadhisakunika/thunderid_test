package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type UserSpec struct {
	// config is forwarded as-is into ThunderID's own user bootstrap schema
	// (type, ouId, attributes, ...) — the operator does not interpret it, so ThunderID's
	// own validation and error messages are what apply. Any "credentials" or "attributes"
	// keys the resolved credentialsSecretRef Secret contributes are overlaid on top of
	// whatever config sets, Secret winning.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config runtime.RawExtension `json:"config,omitempty"`

	// credentialsSecretRef is a k8s Secret reference, not a ThunderID schema field — it stays
	// a typed field because the controller reads it directly to fetch (or auto-provision) the
	// backing Secret. See UserStatus.CredentialsSecretRef for the Secret actually in effect.
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`
}

type UserStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	UserID string `json:"userID,omitempty"`

	// +optional
	Synced bool `json:"synced,omitempty"`

	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// CredentialsSecretRef is the Secret currently backing this user's credentials —
	// either spec.credentialsSecretRef, or an auto-generated "<name>-credentials"
	// Secret when spec.credentialsSecretRef was left empty.
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
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserSpec   `json:"spec,omitempty"`
	Status UserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &User{}, &UserList{})
		return nil
	})
}

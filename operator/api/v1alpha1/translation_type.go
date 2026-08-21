package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type TranslationSpec struct {
	// BCP 47 language tag (e.g. "en-US", "es", "fr-CA"). This is the unique identifier
	// for this translation set within the instance.
	// +kubebuilder:validation:Required
	Language string `json:"language"`

	// Namespace → key → translated string. Namespaces group related keys (e.g. "auth", "errors").
	// +kubebuilder:validation:Required
	Translations map[string]map[string]string `json:"translations"`
}

type TranslationStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

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
// +kubebuilder:printcolumn:name="Language",type="string",JSONPath=".spec.language"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type Translation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TranslationSpec   `json:"spec,omitempty"`
	Status TranslationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type TranslationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Translation `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Translation{}, &TranslationList{})
		return nil
	})
}

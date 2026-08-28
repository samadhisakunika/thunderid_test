package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type EnvironmentSpec struct {
	// Env holds this environment's key-value pairs, applied verbatim as the generated Secret's
	// stringData. Plaintext in the CR's spec — like any Kubernetes object's spec, it's stored in
	// etcd without the base64/RBAC/encryption-at-rest treatment Secret gets. The operator only
	// ever mirrors it into a Secret and then deletes this object (see the controller) — it never
	// reads this field back for any other purpose.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

type EnvironmentStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	Synced bool `json:"synced,omitempty"`

	// SecretName is always the Environment's own name — the generated Secret is 1:1 with it.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Secret",type="string",JSONPath=".status.secretName"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

// Environment is a one-shot trigger, not an ongoing source of truth: applying one makes the
// operator create/update a Secret (named the same as this object, holding spec.env verbatim as
// its stringData) and then delete this Environment object — see environment_controller.go. To
// change the values later, apply a new Environment with the same name; it will update the
// existing Secret's content and self-delete again. The Secret it produces is deliberately not
// owned by this object (no controller reference), since it must outlive it.
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec EnvironmentSpec `json:"spec,omitempty"`

	Status EnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Environment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Environment{}, &EnvironmentList{})
		return nil
	})
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type EnvironmentSpec struct {
	// Env holds this environment's key-value pairs, mirrored into the generated Secret's Data.
	// Plaintext only until the next reconcile: the controller overwrites each key's value here
	// with a masked marker ("thunderid-masked:<up to 4 leading chars>****:len=<n>") once it's
	// synced to the Secret, so only a short prefix (never the rest of the raw value) is visible
	// via `kubectl get/describe environment` past that point — see the controller. To change a
	// key's value later, re-apply with new plaintext for just that key; kubectl apply's 3-way
	// merge leaves already-masked sibling keys untouched.
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

	// Keys lists the currently-synced key names only (never values) — for a developer checking
	// "did my key actually get picked up" without needing to know this becomes a Secret.
	// +optional
	Keys []string `json:"keys,omitempty"`

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

// Environment mirrors spec.env into a Secret of the same name, then masks each synced key's
// value in place (see EnvironmentSpec.Env) — see environment_controller.go. The object is never
// deleted: it stays gettable forever, just with masked values, so a developer who doesn't know
// this becomes a Secret never hits a surprise NotFound. The Secret it produces is deliberately
// not owned by this object (no controller
// reference) — nothing here ever deletes this object, but keeping them decoupled means a future
// change to one can't accidentally cascade-delete the other. Do NOT put this CRD under GitOps
// (Flux) management: a continuously reconciling GitOps tool enforces live-must-match-Git and
// will fight the masking forever (reverting it back to plaintext every reconcile) — see
// operator/gitops/README.md.
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

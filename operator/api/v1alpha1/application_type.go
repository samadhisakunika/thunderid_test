package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type AssertionSpec struct {
	ValidityPeriod int64 `json:"validityPeriod,omitempty"`
}

type LoginConsentSpec struct {
	ValidityPeriod int64 `json:"validityPeriod,omitempty"`
}

type ApplicationSpec struct {
	InstanceRef               string            `json:"instanceRef"`
	Name                      string            `json:"name"`
	Description               string            `json:"description,omitempty"`
	URL                       string            `json:"url,omitempty"`
	LogoURL                   string            `json:"logoUrl,omitempty"`
	OUHandle                  string            `json:"ouHandle,omitempty"`
	OUID                      string            `json:"ouId,omitempty"`
	AuthFlowRef               string            `json:"authFlowRef,omitempty"`
	AuthFlowHandle            string            `json:"authFlowHandle,omitempty"`
	AuthFlowID                string            `json:"authFlowId,omitempty"`
	RegistrationFlowRef       string            `json:"registrationFlowRef,omitempty"`
	RegistrationFlowHandle    string            `json:"registrationFlowHandle,omitempty"`
	IsRegistrationFlowEnabled bool              `json:"isRegistrationFlowEnabled,omitempty"`
	RecoveryFlowRef           string            `json:"recoveryFlowRef,omitempty"`
	RecoveryFlowHandle        string            `json:"recoveryFlowHandle,omitempty"`
	IsRecoveryFlowEnabled     bool              `json:"isRecoveryFlowEnabled,omitempty"`
	Assertion                 *AssertionSpec    `json:"assertion,omitempty"`
	LoginConsent              *LoginConsentSpec `json:"loginConsent,omitempty"`
	AllowedUserTypes          []string          `json:"allowedUserTypes,omitempty"`
	ThemeRef                  string            `json:"themeRef,omitempty"`
	ThemeID                   string            `json:"themeId,omitempty"`
	InboundAuthConfig         string            `json:"inboundAuthConfig,omitempty"`
}

type ApplicationStatus struct {
	Phase        string             `json:"phase,omitempty"`
	Synced       bool               `json:"synced,omitempty"`
	LastSyncTime *metav1.Time       `json:"lastSyncTime,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ApplicationSpec   `json:"spec,omitempty"`
	Status            ApplicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Application{}, &ApplicationList{})
		return nil
	})
}

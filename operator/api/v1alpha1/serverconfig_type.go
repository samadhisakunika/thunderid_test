package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CORSOrigin is a single CORS origin entry — either a literal URL or a regex pattern.
// Exactly one of URL or Regex should be set.
type CORSOrigin struct {
	// Literal origin URL (e.g. "https://app.example.com").
	// +optional
	URL string `json:"url,omitempty"`

	// RE2 regular expression matching allowed origins
	// (e.g. "\\Ahttps://.*\\.preview\\.example\\.com\\z").
	// +optional
	Regex string `json:"regex,omitempty"`
}

// CORSValue holds the CORS server configuration.
type CORSValue struct {
	// +optional
	AllowedOrigins []CORSOrigin `json:"allowedOrigins,omitempty"`
}

// SessionValue holds the SSO session timeout configuration.
type SessionValue struct {
	// Idle timeout in seconds. Zero uses the ThunderID built-in default.
	// +optional
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds,omitempty"`

	// Absolute timeout in seconds. Zero uses the ThunderID built-in default.
	// +optional
	AbsoluteTimeoutSeconds int64 `json:"absoluteTimeoutSeconds,omitempty"`
}

// DefaultResourceServerConfig names the resource server that ThunderID treats
// as the default for permission checks.
type DefaultResourceServerConfig struct {
	// UUID of the ResourceServer to use as the default.
	// +kubebuilder:validation:Required
	ResourceServerID string `json:"resourceServerId"`
}

type ServerConfigSpec struct {
	// +kubebuilder:validation:Required
	InstanceRef string `json:"instanceRef"`

	// Which server configuration section this CR manages.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=cors;defaultResourceServer;session
	Name string `json:"name"`

	// Set when name == "cors".
	// +optional
	CORS *CORSValue `json:"cors,omitempty"`

	// Set when name == "session".
	// +optional
	Session *SessionValue `json:"session,omitempty"`

	// Set when name == "defaultResourceServer".
	// +optional
	DefaultResourceServer *DefaultResourceServerConfig `json:"defaultResourceServer,omitempty"`
}

type ServerConfigStatus struct {
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
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".spec.instanceRef"
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

type ServerConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerConfigSpec   `json:"spec,omitempty"`
	Status ServerConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ServerConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServerConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ServerConfig{}, &ServerConfigList{})
		return nil
	})
}

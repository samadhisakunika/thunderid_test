package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DatabaseSpec selects which database backend ThunderID connects to.
type DatabaseSpec struct {
	// Type selects the database backend. "sqlite" (default) uses the image's built-in SQLite
	// files, wiped and reseeded by setup.sh on every restart unless spec.dataVolumeSize is set.
	// "postgresql" connects to an external PostgreSQL server instead — no local database files,
	// so spec.dataVolumeSize is ignored.
	// +kubebuilder:validation:Enum=sqlite;postgresql
	// +kubebuilder:default=sqlite
	// +optional
	Type string `json:"type,omitempty"`

	// Host is the PostgreSQL server hostname. Required when type is "postgresql", ignored
	// otherwise.
	// +optional
	Host string `json:"host,omitempty"`

	// Port is the PostgreSQL server port.
	// +kubebuilder:default="5432"
	// +optional
	Port string `json:"port,omitempty"`

	// Username is the PostgreSQL user ThunderID connects as. Required when type is
	// "postgresql", ignored otherwise.
	// +optional
	Username string `json:"username,omitempty"`

	// SSLMode is the PostgreSQL SSL mode.
	// +kubebuilder:validation:Enum=require;verify-full;verify-ca;disable
	// +kubebuilder:default="require"
	// +optional
	SSLMode string `json:"sslmode,omitempty"`

	// ConfigDatabase is the database name backing the config and entity scopes.
	// +kubebuilder:default="thunderid_config"
	// +optional
	ConfigDatabase string `json:"configDatabase,omitempty"`

	// RuntimeDatabase is the database name backing the runtime_transient and
	// runtime_persistent scopes.
	// +kubebuilder:default="thunderid_runtime"
	// +optional
	RuntimeDatabase string `json:"runtimeDatabase,omitempty"`

	// SecretRef names a Secret holding the PostgreSQL password, under key "password".
	// Required when type is "postgresql", ignored otherwise.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// EmailSpec configures the SMTP client ThunderID uses for outbound email (registration
// confirmation, password reset, etc). Unset (the default) means no email section is written into
// deployment.yaml at all, matching the image's own out-of-the-box behavior.
type EmailSpec struct {
	// SMTPHost is the SMTP server hostname.
	// +kubebuilder:validation:Required
	SMTPHost string `json:"smtpHost"`

	// SMTPPort is the SMTP server port.
	// +kubebuilder:validation:Required
	SMTPPort int32 `json:"smtpPort"`

	// Username authenticates to the SMTP server. Required when EnableAuthentication is true,
	// ignored otherwise.
	// +optional
	Username string `json:"username,omitempty"`

	// SecretRef names a Secret holding the SMTP password, under key "password". Required when
	// EnableAuthentication is true, ignored otherwise.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`

	// FromAddress is the sender address on outbound email.
	// +kubebuilder:validation:Required
	FromAddress string `json:"fromAddress"`

	// EnableStartTLS upgrades the SMTP connection to TLS via STARTTLS.
	// +optional
	EnableStartTLS bool `json:"enableStartTLS,omitempty"`

	// EnableAuthentication authenticates to the SMTP server using Username/SecretRef.
	// +optional
	EnableAuthentication bool `json:"enableAuthentication,omitempty"`
}

type AutoScalingSpec struct {
	MinReplicas                       int32 `json:"minReplicas"`
	MaxReplicas                       int32 `json:"maxReplicas"`
	TargetCPUUtilizationPercentage    int32 `json:"targetCPUUtilizationPercentage,omitempty"`
	TargetMemoryUtilizationPercentage int32 `json:"targetMemoryUtilizationPercentage,omitempty"`
}

// AppConfigSpec groups every field that maps directly into ThunderID's own deployment.yaml —
// as opposed to ThunderIDInstanceSpec's other fields (image, replicas, resources, autoScaling,
// domain, tlsSecret, securitySecret, resourcesConfigMap, dataVolumeSize), which are Kubernetes/
// operator-level concerns deployment.yaml has no notion of at all.
type AppConfigSpec struct {
	// Database selects sqlite (default) or postgresql. See DatabaseSpec. When Type is sqlite (or
	// unset), spec.dataVolumeSize must be set.
	// +optional
	Database DatabaseSpec `json:"database,omitempty"`

	// Port is the port ThunderID listens on inside the pod — also used for the container port,
	// all three probes, the Service, and the Ingress backend, so it only needs setting once here.
	// +kubebuilder:default=8090
	// +optional
	Port int32 `json:"port,omitempty"`

	// TLSMinVersion is the minimum TLS version ThunderID's own HTTPS listener accepts. Unrelated
	// to spec.tlsSecret, which is the Ingress's TLS termination — this is the app's own listener.
	// +kubebuilder:validation:Enum="1.2";"1.3"
	// +kubebuilder:default="1.3"
	// +optional
	TLSMinVersion string `json:"tlsMinVersion,omitempty"`

	// JWTPreferredKeyID selects which of ThunderID's two built-in signing keys ("default-key",
	// RSA, or "ecdsa-key", ECDSA) it signs new tokens with. Both remain valid for verifying
	// already-issued tokens regardless of this setting.
	// +kubebuilder:validation:Enum=default-key;ecdsa-key
	// +kubebuilder:default="default-key"
	// +optional
	JWTPreferredKeyID string `json:"jwtPreferredKeyId,omitempty"`

	// PasskeyExtraAllowedOrigins adds WebAuthn/passkey origins beyond the instance's own public
	// URL (spec.domain, or https://localhost:<port> when unset) — e.g. a separate frontend origin
	// that also needs to register/verify passkeys against this instance.
	// +optional
	PasskeyExtraAllowedOrigins []string `json:"passkeyExtraAllowedOrigins,omitempty"`

	// Email configures outbound SMTP. Unset means no email section in deployment.yaml at all —
	// see EmailSpec.
	// +optional
	Email *EmailSpec `json:"email,omitempty"`
}

// ThunderIDInstanceSpec defines the desired state of ThunderIDInstance
type ThunderIDInstanceSpec struct {

	// +kubebuilder:validation:Required
	Image string `json:"image"`

	Replicas int32 `json:"replicas,omitempty"`

	// Domain is the externally-reachable hostname for this instance. When set, the operator
	// creates an Ingress (Host: spec.domain, TLS via spec.tlsSecret if set) so ThunderID is
	// reachable without kubectl port-forward access — e.g. a real deployment behind a real
	// DNS record. Optional: when unset (the default, and what every local/dev workflow in
	// this repo actually uses), no Ingress is created and ThunderID is only reachable via its
	// Service (kubectl port-forward svc/<name> 8090:8090 -> https://localhost:8090).
	// +optional
	Domain string `json:"domain,omitempty"`

	// +optional
	TLSSecret string `json:"tlsSecret,omitempty"`

	// SecuritySecret names a pre-existing Secret holding ThunderID's TLS/JWT-signing/encryption
	// key material (server.cert, server.key, signing.cert, signing.key, ecdsa-signing.cert,
	// ecdsa-signing.key, crypto.key, direct_auth_secret). When unset, the operator generates and
	// manages this Secret itself as "<name>-security".
	// +optional
	SecuritySecret string `json:"securitySecret,omitempty"`

	// +optional
	ResourcesConfigMap string `json:"resourcesConfigMap,omitempty"`

	// DataVolumeSize, when set (e.g. "1Gi"), provisions a ReadWriteOnce PersistentVolumeClaim
	// mounted at /opt/thunderid/repository/database (where the SQLite files live) and gates
	// setup.sh to run only once: the container entrypoint checks a marker file on that volume
	// and skips setup.sh on every restart after the first, instead of rerunning it (and wiping
	// the database) on every pod restart. Because the PVC is ReadWriteOnce, the Deployment
	// strategy switches to Recreate to avoid two pods mounting it at once — only meaningful with
	// replicas: 1. Either this or Config.Database.Type=postgresql must be set — a ThunderIDInstance
	// with neither is rejected: ephemeral, wipe-on-every-restart SQLite is not a supported configuration.
	// +optional
	DataVolumeSize string `json:"dataVolumeSize,omitempty"`

	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	AutoScaling *AutoScalingSpec `json:"autoScaling,omitempty"`

	// Config groups every field that maps directly into ThunderID's own deployment.yaml — see
	// AppConfigSpec. Kept separate from this struct's other, Kubernetes/operator-level fields.
	// +optional
	Config AppConfigSpec `json:"config,omitempty"`
}

// ThunderIDInstanceStatus defines the observed state of ThunderIDInstance.
type ThunderIDInstanceStatus struct {

	// +optional
	URL string `json:"url,omitempty"`

	// +optional
	Phase string `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ThunderIDInstance is the Schema for the thunderidinstances API
type ThunderIDInstance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ThunderIDInstance
	// +required
	Spec ThunderIDInstanceSpec `json:"spec"`

	// status defines the observed state of ThunderIDInstance
	// +optional
	Status ThunderIDInstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ThunderIDInstanceList contains a list of ThunderIDInstance
type ThunderIDInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ThunderIDInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ThunderIDInstance{}, &ThunderIDInstanceList{})
		return nil
	})
}

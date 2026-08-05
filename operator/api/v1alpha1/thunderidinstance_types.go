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

	// SecretRef names a Secret holding PostgreSQL connection credentials. Required when
	// type is "postgresql", ignored otherwise. Expected keys: host, username, password
	// (required); port (default "5432"), configDatabase (default "thunderid_config"),
	// runtimeDatabase (default "thunderid_runtime"), userDatabase (default "thunderid_users"),
	// sslmode (default "require"; also accepts "verify-full", "verify-ca", "disable") (optional).
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

type AutoScalingSpec struct {
	MinReplicas                       int32 `json:"minReplicas"`
	MaxReplicas                       int32 `json:"maxReplicas"`
	TargetCPUUtilizationPercentage    int32 `json:"targetCPUUtilizationPercentage,omitempty"`
	TargetMemoryUtilizationPercentage int32 `json:"targetMemoryUtilizationPercentage,omitempty"`
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
	// replicas: 1. Either this or Database.Type=postgresql must be set — a ThunderIDInstance with
	// neither is rejected: ephemeral, wipe-on-every-restart SQLite is not a supported configuration.
	// +optional
	DataVolumeSize string `json:"dataVolumeSize,omitempty"`

	// Database selects sqlite (default) or postgresql. See DatabaseSpec. When Type is sqlite (or
	// unset), DataVolumeSize must be set.
	// +optional
	Database DatabaseSpec `json:"database,omitempty"`

	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	AutoScaling *AutoScalingSpec `json:"autoScaling,omitempty"`
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

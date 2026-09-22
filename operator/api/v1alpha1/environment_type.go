/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EnvSection is one purpose-scoped group of key-value pairs within an Environment — see
// EnvironmentSpec's doc comment. A section is either SecretRef (an existing Secret, e.g.
// cert-manager-issued, used as-is and never touched) or Env (synced into a generated Secret named
// "<environment-name>-<section>" and then masked in place) — not both; SecretRef wins if set, Env
// is ignored. Leaving both unset means this section simply doesn't apply to this Environment.
type EnvSection struct {
	// SecretRef names an existing Secret to use for this section as-is instead of generating one
	// from Env. The operator never creates, inspects, or modifies it. Env is ignored when this is
	// set.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`

	// Env holds this section's key-value pairs, mirrored into the generated Secret
	// "<environment-name>-<section>"'s Data. Plaintext only until the next reconcile: the
	// controller overwrites each key's value here with a masked marker ("thunderid-masked:<up to
	// 4 leading chars>****:len=<n>") once it's synced, so only a short prefix (never the rest of
	// the raw value) is visible via `kubectl get/describe environment` past that point — see the
	// controller. To change a key's value later, re-apply with new plaintext for just that key;
	// kubectl apply's 3-way merge leaves already-masked sibling keys untouched. Ignored when
	// SecretRef is set.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

// EnvironmentSpec defines the desired state of Environment — one object per ThunderIDInstance,
// covering every Secret that instance needs, grouped by what the operator mounts it for:
//   - Credentials: mounted whole via envFrom, resolving {{.VAR}} tokens in Resource specs at
//     ThunderID's own boot.
//   - Security: stands in for spec.securitySecret — server.cert/key, signing.cert/key,
//     ecdsa-signing.cert/key, crypto.key, direct_auth_secret (see securityAllKeys).
//   - Database: stands in for spec.config.database.secretRef — a "password" key.
//   - Smtp: stands in for spec.config.email.secretRef — a "password" key.
//
// Any section left entirely unset (no SecretRef, no Env) simply doesn't apply — e.g. an
// Environment covering only Credentials and Database is exactly as valid as one covering all
// four. The corresponding ThunderIDInstance field (spec.securitySecret,
// spec.config.database.secretRef, spec.config.email.secretRef), when explicitly set, always wins
// over that section — these sections only take effect when the instance-side field is unset.
type EnvironmentSpec struct {
	// +optional
	Credentials EnvSection `json:"credentials,omitempty"`
	// +optional
	Security EnvSection `json:"security,omitempty"`
	// +optional
	Database EnvSection `json:"database,omitempty"`
	// +optional
	Smtp EnvSection `json:"smtp,omitempty"`
}

// EnvironmentStatus defines the observed state of Environment.
type EnvironmentStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	Synced bool `json:"synced,omitempty"`

	// SecretNames maps each section that applies (SecretRef set, or Env non-empty) to the Secret
	// name actually serving it — "<environment-name>-<section>" for a generated one, or the
	// section's own SecretRef verbatim for an adopted one. A section with neither set is absent
	// here.
	// +optional
	SecretNames map[string]string `json:"secretNames,omitempty"`

	// Keys maps each generated section (SecretRef sections are never listed here — their content
	// isn't managed by this object) to its currently-synced key names, names only, never values —
	// for a developer checking "did my key actually get picked up" without needing to know this
	// becomes a Secret.
	// +optional
	Keys map[string][]string `json:"keys,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Synced",type="boolean",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"

// Environment is the single, per-instance object covering every Secret a ThunderIDInstance needs
// — credentials, security keys, database password, SMTP password — one section each (see
// EnvironmentSpec). Each section with Env set syncs into its own generated Secret
// ("<environment-name>-<section>") and then masks that section's Env values in place, the same
// way the whole object used to; a section with SecretRef set instead just references an existing
// Secret untouched. The object is never deleted: it stays gettable forever, just with masked
// values, so a developer who doesn't know a section becomes a Secret never hits a surprise
// NotFound. The generated Secrets are deliberately not owned by this object (no controller
// reference) — nothing here ever deletes this object, but keeping them decoupled means a future
// change to one can't accidentally cascade-delete the others. Do NOT put this CRD under GitOps
// (Flux) management: a continuously reconciling GitOps tool enforces live-must-match-Git and
// will fight the masking forever (reverting it back to plaintext every reconcile) — see
// operator/gitops/README.md.
//
// Labeling this object with InstanceRefLabel (thunderid.io/instance: <name>, same label a
// Resource uses) makes that ThunderIDInstance auto-discover it — see findEnvironment in
// env_secret.go. There is no field on ThunderIDInstanceSpec naming it the other way around: the
// reference lives here, on the Environment, not on the instance. The label is optional — an
// unlabeled Environment still syncs its sections exactly the same, it's simply not wired into any
// instance. Exactly one Environment should carry a given instance's label; more than one is a
// config error the instance surfaces as an InvalidSpec event rather than picking one arbitrarily.
// A hand-written Secret for a given section can still be adopted the same way a whole
// hand-written Environment-equivalent Secret used to be: point that section's SecretRef at it, or
// — to keep the generated-Secret naming convention instead — create a thin Environment whose
// section has an empty Env and let that section's target name ("<environment-name>-<section>")
// collide with a pre-existing Secret of that exact name; its reconcile then adopts the existing
// Secret without touching its contents (see upsertSecret's "adopted" path in
// environment_controller.go). Each ThunderIDInstance-side field this Environment can stand in for
// (spec.securitySecret, spec.config.database.secretRef, spec.config.email.secretRef), when
// explicitly set, always wins over the matching section here — these sections only take effect
// when that field is left unset.
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec EnvironmentSpec `json:"spec,omitempty"`

	Status EnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EnvironmentList contains a list of Environment.
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

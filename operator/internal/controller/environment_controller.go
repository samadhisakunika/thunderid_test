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

package controller

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// maskPrefix marks a section's Env value the controller has already synced to that section's
// Secret and overwritten in place. A value not matching maskPattern is treated as new plaintext to
// sync — this prefix+suffix combination is specific enough that a real credential accidentally
// colliding with it isn't a realistic concern.
const maskPrefix = "thunderid-masked:"

var maskPattern = regexp.MustCompile(`^thunderid-masked:.*:len=\d+$`)

func isMasked(v string) bool {
	return maskPattern.MatchString(v)
}

// managedKeysAnnotation records, on a section's generated Secret, exactly which keys this
// controller last wrote there — the only keys it's ever safe to remove on a later sync. A Secret
// sharing a section's target name but never written by it (hand-made, or otherwise missing the
// annotation) carries no such record, so its keys are left alone instead of being wiped by
// treating the Secret's Data as a full replace.
const managedKeysAnnotation = "thunderid.io/managed-keys"

// managedKeySet parses secret's managedKeysAnnotation into a set of key names, or an empty set
// if the annotation is absent (a Secret this controller has never written to).
func managedKeySet(secret *corev1.Secret) map[string]bool {
	set := map[string]bool{}
	raw := secret.Annotations[managedKeysAnnotation]
	if raw == "" {
		return set
	}
	for k := range strings.SplitSeq(raw, ",") {
		if k != "" {
			set[k] = true
		}
	}
	return set
}

// maskValue reveals at most the first 4 characters of v (never all of it, even for a value 4
// characters or shorter — a short value gets a shorter reveal instead) so a developer can
// eyeball "is this the value I think it is" without the full secret ever living in etcd.
func maskValue(v string) string {
	n := len(v)
	reveal := max(min(4, n-1), 0)
	masked := v[:reveal] + strings.Repeat("*", n-reveal)
	return fmt.Sprintf("%s%s:len=%d", maskPrefix, masked, n)
}

// envSectionDef names one of EnvironmentSpec's four sections and how to reach it on a given
// spec — the single registry every per-section loop below walks, in a fixed order so status
// output (and iteration generally) is stable instead of depending on struct-field reflection.
type envSectionDef struct {
	name string
	get  func(*appsv1alpha1.EnvironmentSpec) *appsv1alpha1.EnvSection
}

var envSections = []envSectionDef{
	{"credentials", func(s *appsv1alpha1.EnvironmentSpec) *appsv1alpha1.EnvSection { return &s.Credentials }},
	{"security", func(s *appsv1alpha1.EnvironmentSpec) *appsv1alpha1.EnvSection { return &s.Security }},
	{"database", func(s *appsv1alpha1.EnvironmentSpec) *appsv1alpha1.EnvSection { return &s.Database }},
	{"smtp", func(s *appsv1alpha1.EnvironmentSpec) *appsv1alpha1.EnvSection { return &s.Smtp }},
}

// EnvironmentReconciler syncs each of an Environment's four sections with Env set into its own
// generated Secret ("<environment-name>-<section>"), then overwrites that section's synced key
// values with masked markers (up to 4 leading characters + stars + length, never the rest of the
// raw value) — see Environment's own doc comment for the reasoning. A section with SecretRef set
// instead is left entirely alone — that Secret is used as-is. Status.SecretNames/Keys reflect
// which Secret serves each applicable section, and which keys are currently synced (names only,
// no values), for debugging "did my key actually get picked up."
//
// Updating a value later means re-applying the same Environment with new plaintext for that
// key — kubectl apply's 3-way merge only sends the changed field, so already-masked sibling keys
// (in this section or any other) are untouched. Do NOT put Environment under GitOps (Flux)
// management — see operator/gitops/README.md.
type EnvironmentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=environments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;patch

// Reconcile syncs each of an Environment's sections into its own Secret (skipping SecretRef
// sections entirely, and sections with neither SecretRef nor Env set), masks each newly-synced
// key's value in place per section, then reflects the result (Phase/Synced/SecretNames/Keys) in
// status.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	obj := &appsv1alpha1.Environment{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Re-injected into ctx so every sub-function below gets this Environment's identity on its
	// own logf.FromContext(ctx) calls for free, without threading obj.Name through each of them.
	log = log.WithValues("environment", obj.Name, "namespace", obj.Namespace)
	ctx = logf.IntoContext(ctx, log)

	secretNames := make(map[string]string, len(envSections))
	keysStatus := make(map[string][]string, len(envSections))
	maskedEnvBySection := make(map[string]map[string]string, len(envSections))
	anyMaskedAny := false

	for _, sec := range envSections {
		section := sec.get(&obj.Spec)
		switch {
		case section.SecretRef != "":
			secretNames[sec.name] = section.SecretRef
		case len(section.Env) > 0:
			targetName := obj.Name + "-" + sec.name
			keys, maskedEnv, maskedAny, err := r.syncSectionSecret(ctx, obj, targetName, section.Env)
			if err != nil {
				log.Error(err, "failed to sync Secret for section", "section", sec.name)
				r.Recorder.Eventf(obj, corev1.EventTypeWarning, "SyncFailed", "%s section: %s", sec.name, err.Error())
				obj.Status.Phase = phaseError
				obj.Status.Synced = false
				_ = r.Status().Update(ctx, obj)
				return ctrl.Result{}, err
			}
			secretNames[sec.name] = targetName
			keysStatus[sec.name] = keys
			if maskedAny {
				maskedEnvBySection[sec.name] = maskedEnv
				anyMaskedAny = true
			}
		}
	}

	if anyMaskedAny {
		if err := r.maskSpec(ctx, obj, maskedEnvBySection); err != nil {
			return ctrl.Result{}, err
		}
	}

	obj.Status.Phase = "Synced"
	obj.Status.Synced = true
	obj.Status.SecretNames = secretNames
	obj.Status.Keys = keysStatus
	if err := r.Status().Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	// Only fires the reconcile that actually masked new plaintext (in any section) — an
	// already-fully-masked object is a no-op above this point, so this doesn't spam an event
	// every time something else (e.g. the status write itself) re-triggers a reconcile.
	if anyMaskedAny {
		r.Recorder.Eventf(obj, corev1.EventTypeNormal, "SecretSynced",
			"Section Secret(s) synced from this Environment's spec; synced key values are now "+
				"masked in spec (see status.keys for the current key names per section) — "+
				"re-apply with new plaintext for a key to change it.")
	}

	return ctrl.Result{}, nil
}

// syncSectionSecret makes secretName's Data an exact match for env's real values (a masked key
// keeps whatever the Secret already holds; a new-plaintext key is written then masked in place)
// and returns the current key names plus the section's fully-masked Env (for the caller to fold
// into a single spec Update across every section, once, after this returns). It doesn't mutate
// obj.Spec itself.
func (r *EnvironmentReconciler) syncSectionSecret(ctx context.Context, obj *appsv1alpha1.Environment, secretName string, env map[string]string) (
	keys []string, maskedEnv map[string]string, maskedAny bool, err error) {
	existing := &corev1.Secret{}
	getErr := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: secretName}, existing)
	found := true
	if errors.IsNotFound(getErr) {
		found = false
	} else if getErr != nil {
		return nil, nil, false, getErr
	}

	data := make(map[string][]byte, len(env))
	maskedEnv = make(map[string]string, len(env))
	keys = make([]string, 0, len(env))
	var newlyMasked []string

	for k, v := range env {
		keys = append(keys, k)
		if isMasked(v) {
			// Already synced on a previous reconcile — carry over whatever the Secret already
			// holds for this key rather than the marker itself. If the Secret lost the key (e.g.
			// someone deleted it out-of-band) there's no way to recover the real value from the
			// marker; the key is simply absent from the Secret until re-applied with plaintext.
			if found {
				if b, ok := existing.Data[k]; ok {
					data[k] = b
				}
			}
			maskedEnv[k] = v
			continue
		}
		data[k] = []byte(v)
		maskedEnv[k] = maskValue(v)
		maskedAny = true
		newlyMasked = append(newlyMasked, k)
	}
	slices.Sort(keys)

	if err := r.upsertSecret(ctx, obj, secretName, found, existing, data); err != nil {
		return nil, nil, false, err
	}

	if maskedAny {
		// Key names only, never values — same "names are safe, values never are" rule this
		// controller's own status.keys field already follows.
		slices.Sort(newlyMasked)
		logf.FromContext(ctx).Info("synced new plaintext key(s) to the Secret and masked them",
			"secret", secretName, "newlyMaskedKeys", newlyMasked, "totalKeys", len(keys))
	}

	return keys, maskedEnv, maskedAny, nil
}

// upsertSecret creates secretName if it doesn't exist yet, or otherwise merges data into it: keys
// this controller previously managed but no longer wants are dropped, this pass's data is laid on
// top, and any other pre-existing key (never managed by this section) is left alone. Whenever it
// actually creates or updates the Secret, it also stamps InstanceRefLabel onto it (mirroring
// obj's own label, if any) so a plain Secret-watch can map a content change straight back to the
// owning instance without needing to know which Environment (or section) produced it — see
// instancesForEnvironment's reuse for both Environment and Secret watches in
// thunderidinstance_controller.go.
func (r *EnvironmentReconciler) upsertSecret(ctx context.Context, obj *appsv1alpha1.Environment, secretName string, found bool, existing *corev1.Secret, data map[string][]byte) error {
	log := logf.FromContext(ctx)
	managedKeys := make([]string, 0, len(data))
	for k := range data {
		managedKeys = append(managedKeys, k)
	}
	slices.Sort(managedKeys)
	managedKeysValue := strings.Join(managedKeys, ",")

	instanceLabel := obj.Labels[appsv1alpha1.InstanceRefLabel]

	// Create if it doesn't exist yet.
	if !found {
		log.Info("creating Secret", "name", secretName, "managedKeys", managedKeys)
		var labels map[string]string
		if instanceLabel != "" {
			labels = map[string]string{appsv1alpha1.InstanceRefLabel: instanceLabel}
		}
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        secretName,
				Namespace:   obj.Namespace,
				Labels:      labels,
				Annotations: map[string]string{managedKeysAnnotation: managedKeysValue},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		})
	}

	// Merge, not replace: start from whatever the Secret already has, drop only the keys this
	// controller previously wrote (per managedKeysAnnotation) that are no longer wanted —
	// preserving the existing declarative "dropped key stops being synced" behavior — then lay
	// this pass's data on top. A key neither previously-managed nor in this pass's data (a
	// foreign key) is never touched.
	previouslyManaged := managedKeySet(existing)
	finalData := make(map[string][]byte, len(existing.Data)+len(data))
	maps.Copy(finalData, existing.Data)
	for k := range previouslyManaged {
		if _, stillWanted := data[k]; !stillWanted {
			delete(finalData, k)
		}
	}
	maps.Copy(finalData, data)

	if existing.Annotations[managedKeysAnnotation] == "" && len(existing.Data) > 0 {
		log.Info("Secret already existed with unmanaged key(s), adopting only the keys this section names", "name", secretName, "managedKeys", managedKeys)
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "AdoptedExistingSecret",
			"Secret %q already existed with key(s) not previously managed by this Environment; "+
				"those are left untouched — only %v are now managed.",
			secretName, managedKeys)
	}

	labelsMatch := instanceLabel == "" || existing.Labels[appsv1alpha1.InstanceRefLabel] == instanceLabel
	if reflect.DeepEqual(existing.Data, finalData) && existing.Annotations[managedKeysAnnotation] == managedKeysValue && labelsMatch {
		log.V(1).Info("Secret unchanged, skipping update", "name", secretName)
		return nil
	}

	log.Info("Secret content changed, updating", "name", secretName, "managedKeys", managedKeys)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: secretName}, latest); err != nil {
			return err
		}
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[managedKeysAnnotation] = managedKeysValue
		if instanceLabel != "" {
			if latest.Labels == nil {
				latest.Labels = map[string]string{}
			}
			latest.Labels[appsv1alpha1.InstanceRefLabel] = instanceLabel
		}
		latest.Data = finalData
		latest.StringData = nil
		return r.Update(ctx, latest)
	})
}

// maskSpec persists, for every section present in maskedEnvBySection, obj.Spec.<Section>.Env =
// that section's already-computed fully-masked map — one retry-wrapped Update covering every
// section that had new plaintext this pass, so the raw value of any key synced this pass is
// never visible via `kubectl get/describe environment` again.
func (r *EnvironmentReconciler) maskSpec(ctx context.Context, obj *appsv1alpha1.Environment, maskedEnvBySection map[string]map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &appsv1alpha1.Environment{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, latest); err != nil {
			return err
		}
		for _, sec := range envSections {
			maskedEnv, ok := maskedEnvBySection[sec.name]
			if !ok {
				continue
			}
			sec.get(&latest.Spec).Env = maskedEnv
		}
		if err := r.Update(ctx, latest); err != nil {
			return err
		}
		// Write the now-current ResourceVersion (and any other metadata) back into the
		// caller's obj — Reconcile's subsequent Status().Update(ctx, obj) would otherwise use
		// the ResourceVersion from before this Update and always lose a conflict against the
		// spec change just made here.
		*obj = *latest
		return nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Environment{}).
		Named("environment").
		Complete(r)
}

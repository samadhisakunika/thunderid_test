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

// maskPrefix marks a spec.env value the controller has already synced to the Secret and
// overwritten in place. A value not matching maskPattern is treated as new plaintext to sync —
// this prefix+suffix combination is specific enough that a real credential accidentally
// colliding with it isn't a realistic concern.
const maskPrefix = "thunderid-masked:"

var maskPattern = regexp.MustCompile(`^thunderid-masked:.*:len=\d+$`)

func isMasked(v string) bool {
	return maskPattern.MatchString(v)
}

// managedKeysAnnotation records, on the generated Secret, exactly which keys this controller
// last wrote there — the only keys it's ever safe to remove on a later sync. A Secret sharing
// this EnvironmentValues's name but never written by it (hand-made, or otherwise missing the
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

// EnvironmentValuesReconciler mirrors spec.env into a Secret of the same name, then overwrites each
// synced key's value in spec.env with a masked marker (up to 4 leading characters + stars +
// length, never the rest of the raw value) — see EnvironmentValues's own doc comment for the reasoning.
// Status.Keys lists which keys are currently synced (names only, no values) for debugging "did
// my key actually get picked up."
//
// Updating a value later means re-applying the same EnvironmentValues with new plaintext for that key
// — kubectl apply's 3-way merge only sends the changed field, so already-masked sibling keys are
// untouched. Do NOT put EnvironmentValues under GitOps (Flux) management — see
// operator/gitops/README.md.
type EnvironmentValuesReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=environmentvalues,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=environmentvalues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;patch

// Reconcile syncs an EnvironmentValues's spec.env into a same-named Secret and masks each newly-synced
// key's value in place, then reflects the result (Phase/Synced/SecretName/Keys) in its status.
func (r *EnvironmentValuesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the EnvironmentValues.
	obj := &appsv1alpha1.EnvironmentValues{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Re-injected into ctx so every sub-function below gets this EnvironmentValues's identity on its
	// own logf.FromContext(ctx) calls for free, without threading obj.Name through each of them.
	log = log.WithValues("environment", obj.Name, "namespace", obj.Namespace)
	ctx = logf.IntoContext(ctx, log)

	// Sync spec.env into the Secret and mask any newly-synced keys.
	keys, maskedAny, err := r.syncSecretAndMask(ctx, obj)
	if err != nil {
		log.Error(err, "failed to sync Secret from EnvironmentValues")
		r.Recorder.Event(obj, corev1.EventTypeWarning, "SyncFailed", err.Error())
		obj.Status.Phase = phaseError
		obj.Status.Synced = false
		obj.Status.SecretName = obj.Name
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, err
	}

	// Reflect the result in status.
	obj.Status.Phase = "Synced"
	obj.Status.Synced = true
	obj.Status.SecretName = obj.Name
	obj.Status.Keys = keys
	if err := r.Status().Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	// Only fires the reconcile that actually masked new plaintext — an already-fully-masked
	// object is a no-op above this point, so this doesn't spam an event every time something
	// else (e.g. the status write itself) re-triggers a reconcile.
	if maskedAny {
		r.Recorder.Eventf(obj, corev1.EventTypeNormal, "SecretSynced",
			"Secret %q synced from this EnvironmentValues's spec.env; synced key values are now masked "+
				"in spec.env (see status.keys for the current key names) — re-apply with new "+
				"plaintext for a key to change it.", obj.Name)
	}

	return ctrl.Result{}, nil
}

// syncSecretAndMask makes the Secret's Data an exact match for obj.Spec.Env's real values
// (a masked key keeps whatever the Secret already holds; a new-plaintext key is written then
// masked in place) and returns the current key names. It persists the spec mutation via Update
// only when at least one key held new plaintext this pass, so a fully-masked object converges
// instead of looping.
func (r *EnvironmentValuesReconciler) syncSecretAndMask(ctx context.Context, obj *appsv1alpha1.EnvironmentValues) ([]string, bool, error) {
	// Fetch the existing Secret, if any.
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, existing)
	found := true
	if errors.IsNotFound(err) {
		found = false
	} else if err != nil {
		return nil, false, err
	}

	data := make(map[string][]byte, len(obj.Spec.Env))
	maskedSpec := make(map[string]string, len(obj.Spec.Env))
	keys := make([]string, 0, len(obj.Spec.Env))
	var newlyMasked []string
	maskedAny := false

	// Split spec.env into already-masked (carry over) and new-plaintext (write and mask) keys.
	for k, v := range obj.Spec.Env {
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
			maskedSpec[k] = v
			continue
		}
		data[k] = []byte(v)
		maskedSpec[k] = maskValue(v)
		maskedAny = true
		newlyMasked = append(newlyMasked, k)
	}
	slices.Sort(keys)

	// Write the Secret.
	if err := r.upsertSecret(ctx, obj, found, existing, data); err != nil {
		return nil, false, err
	}

	if maskedAny {
		// Key names only, never values — same "names are safe, values never are" rule this
		// controller's own status.keys field already follows.
		slices.Sort(newlyMasked)
		logf.FromContext(ctx).Info("synced new plaintext key(s) to the Secret and masked them in spec.env",
			"newlyMaskedKeys", newlyMasked, "totalKeys", len(keys))
		if err := r.maskSpec(ctx, obj, maskedSpec); err != nil {
			return nil, false, err
		}
	}

	return keys, maskedAny, nil
}

// upsertSecret creates the Secret if it doesn't exist yet, or otherwise merges data into it:
// keys this controller previously managed but no longer wants are dropped, this pass's data is
// laid on top, and any other pre-existing key (never managed by this EnvironmentValues) is left alone.
func (r *EnvironmentValuesReconciler) upsertSecret(ctx context.Context, obj *appsv1alpha1.EnvironmentValues, found bool, existing *corev1.Secret, data map[string][]byte) error {
	log := logf.FromContext(ctx)
	// Track which keys this pass manages, for managedKeysAnnotation.
	managedKeys := make([]string, 0, len(data))
	for k := range data {
		managedKeys = append(managedKeys, k)
	}
	slices.Sort(managedKeys)
	managedKeysValue := strings.Join(managedKeys, ",")

	// Create if it doesn't exist yet.
	if !found {
		log.Info("creating Secret", "managedKeys", managedKeys)
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        obj.Name,
				Namespace:   obj.Namespace,
				Annotations: map[string]string{managedKeysAnnotation: managedKeysValue},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		})
	}

	// Merge, not replace: start from whatever the Secret already has, drop only the keys this
	// controller previously wrote (per managedKeysAnnotation) that are no longer in spec.env —
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
		log.Info("Secret already existed with unmanaged key(s), adopting only the keys spec.env names", "managedKeys", managedKeys)
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "AdoptedExistingSecret",
			"Secret %q already existed with key(s) not previously managed by this EnvironmentValues; "+
				"those are left untouched — only %v are now managed by this EnvironmentValues.",
			obj.Name, managedKeys)
	}

	if reflect.DeepEqual(existing.Data, finalData) && existing.Annotations[managedKeysAnnotation] == managedKeysValue {
		log.V(1).Info("Secret unchanged, skipping update")
		return nil
	}

	log.Info("Secret content changed, updating", "managedKeys", managedKeys)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, latest); err != nil {
			return err
		}
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[managedKeysAnnotation] = managedKeysValue
		latest.Data = finalData
		latest.StringData = nil
		return r.Update(ctx, latest)
	})
}

// maskSpec persists obj.Spec.Env = maskedSpec so the raw value of any key synced this pass is
// never visible via `kubectl get/describe environmentvalues` again.
func (r *EnvironmentValuesReconciler) maskSpec(ctx context.Context, obj *appsv1alpha1.EnvironmentValues, maskedSpec map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &appsv1alpha1.EnvironmentValues{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, latest); err != nil {
			return err
		}
		latest.Spec.Env = maskedSpec
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
func (r *EnvironmentValuesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.EnvironmentValues{}).
		Named("environmentvalues").
		Complete(r)
}

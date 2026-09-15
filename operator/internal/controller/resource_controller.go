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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const resourceFinalizer = "thunderid.io/resource-cleanup"

// appLabelKey selects a serving pod/Deployment by instance name - the same "app: <instance name>"
// label thunderidinstance_controller.go's reconcileDeployment stamps on the Deployment/pod
// template, used here to find the pods belonging to a given instance.
const appLabelKey = "app"

// thunderidContainerName is the serving container's name in the Deployment built by
// reconcileDeployment - matched here against a pod's container statuses to find its health.
const thunderidContainerName = "thunderid"

// Resource.Status.Phase values (a plain string field, not a typed enum - see ResourceStatus).
// phaseError is also used by EnvironmentReconciler for the same "something went wrong" meaning
// on Environment.Status.Phase.
const (
	phaseError   = "Error"
	phasePending = "Pending"
	phaseReady   = "Ready"
)

// resourceKinds is the single registry of every ThunderID resource kind (spec.resource_type
// value) this operator forwards — adding a new kind means adding one entry here, full stop.
// Both reconcileConfigMap's resources.yaml merge (thunderidinstance_controller.go) and
// configHash's per-type version hashing read this same slice directly instead of each keeping
// their own hand-copied list of resource types, so the operator's behavior is consistent and doesn't drift over time.
//
// Order matters: the merge loop appends each kind's ConfigMap content in this slice's order,
// and ThunderID's own bootstrap loader wants that in dependency order (organization units
// first, since everything else can reference ouId).
var resourceKinds = []struct {
	resourceType    string
	configMapSuffix string
	fileKey         string
}{
	{"organization_unit", "organizationunits", "organizationunits.yaml"},
	{"user_type", "usertypes", "usertypes.yaml"},
	{"resource_server", "resourceservers", "resourceservers.yaml"},
	{"role", "roles", "roles.yaml"},
	{"group", "groups", "groups.yaml"},
	{"user", "users", "users.yaml"},
	{"flow", "flows", "flows.yaml"},
	{"theme", "themes", "themes.yaml"},
	{"application", "applications", "applications.yaml"},
	{"translation", "translations", "translations.yaml"},
	{"server_config", "serverconfigs", "serverconfigs.yaml"},
	{"agent_type", "agenttypes", "agenttypes.yaml"},
	{"agent", "agents", "agents.yaml"},
	{"connection", "connections", "connections.yaml"},
	{"layout", "layouts", "layouts.yaml"},
	{"presentation_definition", "presentationdefinitions", "presentationdefinitions.yaml"},
	{"credential_configuration", "credentialconfigurations", "credentialconfigurations.yaml"},
}

// resourceTypeMeta indexes resourceKinds by resource_type for the O(1) lookups below — built
// once from the single ordered list above, never maintained separately.
var resourceTypeMeta = func() map[string]struct{ configMapSuffix, fileKey string } {
	m := make(map[string]struct{ configMapSuffix, fileKey string }, len(resourceKinds))
	for _, k := range resourceKinds {
		m[k.resourceType] = struct{ configMapSuffix, fileKey string }{k.configMapSuffix, k.fileKey}
	}
	return m
}()

// parseResourceSpec decodes obj.Spec.Raw into a generic map and reads resource_type out of it —
// the one key the operator itself acts on, to know which ConfigMap this belongs in. The key stays
// in the returned map and is forwarded to ThunderID along with everything else (see
// resource_type.go's doc comment).
func parseResourceSpec(raw runtime.RawExtension) (resourceType string, data map[string]any, err error) {
	data = map[string]any{}
	if len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &data); err != nil {
			return "", nil, fmt.Errorf("spec: %w", err)
		}
	}
	resourceType, _ = data["resource_type"].(string)
	return resourceType, data, nil
}

// ResourceReconciler reconciles a Resource object.
type ResourceReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=resources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile syncs one Resource into its resource_type's ConfigMap (via rebuildResourcesConfigMap)
// and tracks whether ThunderID actually picked it up, via checkThunderIDHealth.
func (r *ResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Resource.
	obj := &appsv1alpha1.Resource{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Re-injected into ctx so every sub-function below gets this resource's identity on its own
	// logf.FromContext(ctx) calls for free, without threading obj.Name through each of them.
	log = log.WithValues("resource", obj.Name, "namespace", obj.Namespace)
	ctx = logf.IntoContext(ctx, log)

	// Parse spec.resource_type out of the spec.
	resourceType, _, err := parseResourceSpec(obj.Spec)
	if err != nil {
		if !obj.DeletionTimestamp.IsZero() {
			log.Info("deleting Resource with an invalid spec, dropping finalizer without syncing")
			obj.Finalizers = slices.DeleteFunc(obj.Finalizers, func(f string) bool { return f == resourceFinalizer })
			return ctrl.Result{}, r.Update(ctx, obj)
		}
		log.Error(err, "invalid Resource spec")
		r.Recorder.Event(obj, corev1.EventTypeWarning, "InvalidSpec", err.Error())
		obj.Status.Phase = phaseError
		obj.Status.Synced = false
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, nil
	}
	log = log.WithValues("resourceType", resourceType)
	ctx = logf.IntoContext(ctx, log)

	// Deletion is handled before the instance lookup below: once the owning ThunderIDInstance
	// is itself gone, that lookup would return NotFound forever and this finalizer could never
	// clear (the object would be stuck Terminating indefinitely) if it were checked first.
	if !obj.DeletionTimestamp.IsZero() {
		if slices.Contains(obj.Finalizers, resourceFinalizer) {
			instance := &appsv1alpha1.ThunderIDInstance{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
				if client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
				// Owning instance already gone — its ConfigMaps are gone with it, nothing left
				// to remove this resource from. Fall through to just drop the finalizer.
				log.Info("owning ThunderIDInstance already gone, dropping finalizer without syncing")
			} else {
				log.Info("Resource deleted, removing it from the resources ConfigMap")
				if _, err := r.rebuildResourcesConfigMap(ctx, instance, resourceType, string(obj.UID)); err != nil {
					return ctrl.Result{}, err
				}
			}
			obj.Finalizers = slices.DeleteFunc(obj.Finalizers, func(f string) bool { return f == resourceFinalizer })
			if err := r.Update(ctx, obj); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Reject an unknown resource_type.
	if _, ok := resourceTypeMeta[resourceType]; !ok {
		log.Info("unknown spec.resource_type")
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "UnknownType", "spec.resource_type %q is not a known ThunderID resource_type", resourceType)
		obj.Status.Phase = phaseError
		obj.Status.Synced = false
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, nil
	}

	// Fetch the owning ThunderIDInstance.
	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", obj.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", obj.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	// Add the finalizer on first sight.
	if !slices.Contains(obj.Finalizers, resourceFinalizer) {
		obj.Finalizers = append(obj.Finalizers, resourceFinalizer)
		if err := r.Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Sync into the resources ConfigMap.
	changed, err := r.rebuildResourcesConfigMap(ctx, instance, resourceType, "")
	if err != nil {
		log.Error(err, "failed to sync resources ConfigMap")
		r.Recorder.Event(obj, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "ResourceSyncFailed", fmt.Sprintf("Resource %s (%s): %v", obj.Name, resourceType, err))
		obj.Status.Phase = phaseError
		obj.Status.Synced = false
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, err
	}
	// Nothing changed - just verify ThunderID health instead of re-syncing.
	if obj.Status.Phase == phasePending && !changed {
		if obj.Status.LastSyncTime != nil {
			elapsed := time.Since(obj.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, obj, instance)
	}

	if obj.Status.Phase == phaseError && !changed {
		return r.checkThunderIDHealth(ctx, obj, instance)
	}

	if !changed && obj.Status.Phase == phaseReady {
		return r.checkThunderIDHealth(ctx, obj, instance)
	}

	// Mark Pending and requeue to verify ThunderID picked it up.
	now := metav1.Now()
	r.Recorder.Event(obj, corev1.EventTypeNormal, "Synced", "Resource synced to ConfigMap, waiting for ThunderID to apply")
	obj.Status.Phase = phasePending
	obj.Status.Synced = false
	obj.Status.LastSyncTime = &now
	obj.Status.ID = string(obj.UID)
	if err := r.Status().Update(ctx, obj); err != nil {
		log.Error(err, "failed to update resource status")
		return ctrl.Result{}, err
	}

	log.Info("resource synced; requeuing in 30s to verify ThunderID health", "name", obj.Name, "type", resourceType)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// checkThunderIDHealth inspects the serving pod's container status for obj's owning instance:
// a crash sets Phase=Error (and, once escalated to CrashLoopBackOff, emits the pod's error logs
// via fetchAndEmitPodErrors); otherwise it sets Phase=Ready/Synced=true once healthy.
func (r *ResourceReconciler) checkThunderIDHealth(ctx context.Context, obj *appsv1alpha1.Resource, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Find the serving pods.
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(instance.Namespace), client.MatchingLabels{appLabelKey: instance.Name}); err != nil {
		return ctrl.Result{}, err
	}

	for _, pod := range podList.Items {
		unhealthy, issue := diagnosePod(&pod)
		if !unhealthy {
			continue
		}
		log.Info("ThunderID pod is unhealthy", "pod", pod.Name, "reason", issue.reason)

		message := issue.message
		if extra := diagnoseMissingSecret(ctx, r.Client, instance, issue); extra != "" {
			message = extra
		}
		if obj.Status.Phase != phaseError {
			obj.Status.Phase = phaseError
			obj.Status.Synced = false
			_ = r.Status().Update(ctx, obj)
		}
		if message != "" {
			r.Recorder.Event(obj, corev1.EventTypeWarning, issue.reason, message)
		}
		// CrashLoopBackOff is the only reason with real container logs worth fetching; every
		// other reason (a config-resolution failure, or a crash not yet escalated to
		// CrashLoopBackOff) keeps polling instead.
		if issue.reason != reasonCrashLoopBackOff {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.fetchAndEmitPodErrors(ctx, obj, instance.Namespace, pod.Name)
		return ctrl.Result{}, nil
	}

	// No crash found - mark Ready if not already.
	if obj.Status.Phase != phaseReady || !obj.Status.Synced {
		r.Recorder.Event(obj, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; resource is active")
		obj.Status.Phase = phaseReady
		obj.Status.Synced = true
		if err := r.Status().Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, resource ready", "name", obj.Name)
	}
	return ctrl.Result{}, nil
}

// fetchAndEmitPodErrors fetches podName's crashed container logs (preferring the previous
// instance's logs, falling back to the current one) and emits up to 5 ERROR-level lines as
// ThunderIDError events, or — if none matched — the last 5 lines of output as ThunderIDCrashed
// events instead.
func (r *ResourceReconciler) fetchAndEmitPodErrors(ctx context.Context, obj *appsv1alpha1.Resource, namespace, podName string) {
	// Fetch the crashed container's logs, preferring the previous instance.
	req := r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Previous:  true,
		Container: thunderidContainerName,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		req = r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: thunderidContainerName,
		})
		rc, err = req.Stream(ctx)
	}
	if err != nil {
		r.Recorder.Event(obj, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(obj, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff", podName))
		return
	}

	// Emit up to 5 ERROR-level lines as events.
	lines := strings.Split(string(data), "\n")
	errCount := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.Contains(line, "level=ERROR") || strings.Contains(line, `"level":"ERROR"`) {
			r.Recorder.Event(obj, corev1.EventTypeWarning, "ThunderIDError", line)
			errCount++
			if errCount >= 5 {
				break
			}
		}
	}

	// No ERROR lines found - emit the last 5 lines instead.
	if errCount == 0 {
		var tail []string
		for i := len(lines) - 1; i >= 0 && len(tail) < 5; i-- {
			if lines[i] != "" {
				tail = append([]string{lines[i]}, tail...)
			}
		}
		for _, line := range tail {
			r.Recorder.Event(obj, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildResourcesConfigMap returns true if the ConfigMap content changed. It lists every
// Resource whose spec.resource_type matches resourceType and is bound to instance, renders
// each, and writes them into the ConfigMap resourceTypeMeta maps that type to — the same
// ConfigMap the original per-kind controller for that type would have written.
func (r *ResourceReconciler) rebuildResourcesConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, resourceType, excludeUID string) (bool, error) {
	// Look up which ConfigMap/key this resource_type renders into.
	meta, ok := resourceTypeMeta[resourceType]
	if !ok {
		return false, fmt.Errorf("spec.resource_type %q is not a known ThunderID resource_type", resourceType)
	}

	// List and sort every Resource of this type bound to instance.
	list := &appsv1alpha1.ResourceList{}
	if err := r.List(ctx, list, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	slices.SortFunc(list.Items, func(a, b appsv1alpha1.Resource) int {
		return strings.Compare(a.Name, b.Name)
	})

	// Render each matching item into the ConfigMap content.
	var buf bytes.Buffer
	for _, o := range list.Items {
		itemType, itemData, err := parseResourceSpec(o.Spec)
		if err != nil {
			return false, fmt.Errorf("resource %s: %w", o.Name, err)
		}
		if itemType != resourceType {
			continue
		}
		if o.Labels[appsv1alpha1.InstanceRefLabel] != instance.Name {
			continue
		}
		if excludeUID != "" && string(o.UID) == excludeUID {
			continue
		}
		if !o.DeletionTimestamp.IsZero() {
			continue
		}
		buf.WriteString("---\n")
		buf.WriteString(r.buildResourceYAML(o, itemData))
	}
	content := buf.String()

	// Hash the content and skip if nothing changed.
	h := sha256.New()
	h.Write([]byte(content))
	version := hex.EncodeToString(h.Sum(nil))[:16]

	versionAnnotation := "thunderid.io/" + meta.configMapSuffix + "-version"
	if instance.Annotations[versionAnnotation] == version {
		return false, nil
	}

	// Get or create/update the ConfigMap.
	cmName := instance.Name + "-" + meta.configMapSuffix
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{meta.fileKey: content},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create %s ConfigMap: %w", meta.configMapSuffix, err)
		}
	} else if err != nil {
		return false, err
	} else {
		updateErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &corev1.ConfigMap{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, latest); err != nil {
				return err
			}
			if latest.Data == nil {
				latest.Data = map[string]string{}
			}
			latest.Data[meta.fileKey] = content
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update %s ConfigMap: %w", meta.configMapSuffix, updateErr)
		}
	}

	// Record the new content hash on the instance.
	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations[versionAnnotation] = version
	return true, r.Patch(ctx, instance, patch)
}

// renderResourceYAML renders data (a decoded JSON document — so only string/bool/nil/float64/
// map[string]any/[]any values appear in it) as a YAML document via gopkg.in/yaml.v3, the same
// library ThunderID's own declarative resource parsers (application, theme, flow, ou, role, ...)
// use to read it back. An earlier version of this function hand-wrote YAML instead, because every
// library tried — including this one — escapes non-BMP runes (emoji) in a marshaled string as
// \Uxxxxxxxx, and ThunderID's parser was believed unable to read that escape form (APP-1018).
// Confirmed live against a running ThunderID instance (2026-09-08) that this is no longer true:
// yaml.v3 both produces and correctly decodes \Uxxxxxxxx scalars today, so the hand-rolled writer
// is no longer needed. Marshal errors are not handled because data only ever contains values that
// came out of encoding/json.Unmarshal, all of which are directly encodable.
func renderResourceYAML(data map[string]any) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(data)
	_ = enc.Close()
	return buf.String()
}

// buildResourceYAML generates a YAML document for a single resource in the format ThunderID's
// declarative resource loader expects. data is o.Spec already decoded (see parseResourceSpec) —
// the caller parses once and passes it in rather than every item re-parsing its own spec twice.
func (r *ResourceReconciler) buildResourceYAML(o appsv1alpha1.Resource, data map[string]any) string {
	data["id"] = string(o.UID)
	return renderResourceYAML(data)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Resource{}).
		// No GenerationChangedPredicate here (unlike the ThunderIDInstance watch below):
		// generation only bumps on spec changes, but the whole point of watching Deployment is
		// to react to pod-health transitions (crash <-> healthy), which are status-only changes.
		// A GenerationChangedPredicate silently drops exactly those events, leaving Phase=Error
		// resources permanently wedged until an unrelated spec-changing rollout happens to land
		// while the pod's actually healthy. resourcesForDeployment already does its own filtering
		// (Error: always re-check; Ready/Pending: only if unavailable), so no event-level
		// predicate is needed on top.
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForDeployment)).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("resource").
		Complete(r)
}

// resourcesForDeployment maps a Deployment change to Resources that reference it.
// For Phase=Error resources: always re-trigger (pod may have recovered).
// For Phase=Ready resources: re-trigger only when the Deployment is unhealthy.
func (r *ResourceReconciler) resourcesForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	// List every Resource in this namespace.
	list := &appsv1alpha1.ResourceList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, o := range list.Items {
		if o.Labels[appsv1alpha1.InstanceRefLabel] != dep.GetName() {
			continue
		}
		switch o.Status.Phase {
		case phaseError:
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: o.Name, Namespace: o.Namespace},
			})
		case phaseReady, phasePending:
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: o.Name, Namespace: o.Namespace},
				})
			}
		}
	}
	return reqs
}

// resourcesForInstance maps a ThunderIDInstance change to every Resource bound to it.
func (r *ResourceReconciler) resourcesForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &appsv1alpha1.ResourceList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, o := range list.Items {
		if o.Labels[appsv1alpha1.InstanceRefLabel] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: o.Name, Namespace: o.Namespace},
			})
		}
	}
	return reqs
}

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const resourceFinalizer = "thunderid.io/resource-cleanup"

// resourceTypeMeta maps spec.resource_type (ThunderID's own resource_type string) to the
// ConfigMap this operator writes it into — the same names/keys the original per-kind CRDs
// (Application, Flow, ...) used, so this generic controller is a drop-in for any of them.
// Adding a new ThunderID resource kind means adding one line here — no new Go type, no new
// CRD, no new controller file.
var resourceTypeMeta = map[string]struct {
	configMapSuffix string
	fileKey         string
}{
	"application":       {"applications", "applications.yaml"},
	"flow":              {"flows", "flows.yaml"},
	"theme":             {"themes", "themes.yaml"},
	"organization_unit": {"organizationunits", "organizationunits.yaml"},
	"role":              {"roles", "roles.yaml"},
	"user_type":         {"usertypes", "usertypes.yaml"},
	"user":              {"users", "users.yaml"},
	"resource_server":   {"resourceservers", "resourceservers.yaml"},
	"group":             {"groups", "groups.yaml"},
	"server_config":     {"serverconfigs", "serverconfigs.yaml"},
	"translation":       {"translations", "translations.yaml"},
}

// parseResourceSpec decodes obj.Spec.Raw into a generic map, pulling out the one operator-only
// key (resource_type) that lives alongside ThunderID's own fields — spec is otherwise forwarded
// to ThunderID essentially as-is (see resource_type.go's doc comment).
func parseResourceSpec(raw runtime.RawExtension) (resourceType string, data map[string]interface{}, err error) {
	data = map[string]interface{}{}
	if len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &data); err != nil {
			return "", nil, fmt.Errorf("spec: %w", err)
		}
	}
	resourceType, _ = data["resource_type"].(string)
	return resourceType, data, nil
}

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

func (r *ResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	obj := &appsv1alpha1.Resource{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	resourceType, _, err := parseResourceSpec(obj.Spec)
	if err != nil {
		if !obj.DeletionTimestamp.IsZero() {
			obj.Finalizers = removeString(obj.Finalizers, resourceFinalizer)
			return ctrl.Result{}, r.Update(ctx, obj)
		}
		r.Recorder.Event(obj, corev1.EventTypeWarning, "InvalidSpec", err.Error())
		obj.Status.Phase = "Error"
		obj.Status.Synced = false
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, nil
	}

	// Deletion is handled before the instance lookup below: once the owning ThunderIDInstance
	// is itself gone, that lookup would return NotFound forever and this finalizer could never
	// clear (the object would be stuck Terminating indefinitely) if it were checked first.
	if !obj.DeletionTimestamp.IsZero() {
		if containsString(obj.Finalizers, resourceFinalizer) {
			instance := &appsv1alpha1.ThunderIDInstance{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
				if client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
				// Owning instance already gone — its ConfigMaps are gone with it, nothing left
				// to remove this resource from. Fall through to just drop the finalizer.
			} else if _, err := r.rebuildResourcesConfigMap(ctx, instance, resourceType, string(obj.UID)); err != nil {
				return ctrl.Result{}, err
			}
			obj.Finalizers = removeString(obj.Finalizers, resourceFinalizer)
			if err := r.Update(ctx, obj); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if _, ok := resourceTypeMeta[resourceType]; !ok {
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "UnknownType", "spec.resource_type %q is not a known ThunderID resource_type", resourceType)
		obj.Status.Phase = "Error"
		obj.Status.Synced = false
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, nil
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", obj.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", obj.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	if !containsString(obj.Finalizers, resourceFinalizer) {
		obj.Finalizers = append(obj.Finalizers, resourceFinalizer)
		if err := r.Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildResourcesConfigMap(ctx, instance, resourceType, "")
	if err != nil {
		log.Error(err, "failed to sync resources ConfigMap")
		r.Recorder.Event(obj, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "ResourceSyncFailed", fmt.Sprintf("Resource %s (%s): %v", obj.Name, resourceType, err))
		obj.Status.Phase = "Error"
		obj.Status.Synced = false
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, err
	}
	if obj.Status.Phase == "Pending" && !changed {
		if obj.Status.LastSyncTime != nil {
			elapsed := time.Since(obj.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, obj, instance)
	}

	if obj.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, obj, instance)
	}

	if !changed && obj.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, obj, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(obj, corev1.EventTypeNormal, "Synced", "Resource synced to ConfigMap, waiting for ThunderID to apply")
	obj.Status.Phase = "Pending"
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

func (r *ResourceReconciler) checkThunderIDHealth(ctx context.Context, obj *appsv1alpha1.Resource, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(instance.Namespace), client.MatchingLabels{"app": instance.Name}); err != nil {
		return ctrl.Result{}, err
	}

	for _, pod := range podList.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "thunderid" {
				continue
			}
			inCrashLoop := cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff"
			// LastTerminationState reflects the most recent termination ever, not the current
			// state — it stays populated forever after a single past crash, even once the
			// container has been Ready and healthy for hours since. Gating on !cs.Ready keeps
			// this catching the narrow window right after a crash but before Kubernetes has
			// escalated to CrashLoopBackOff (which inCrashLoop alone would miss), without
			// permanently flagging a fully-recovered container as still crashed (confirmed
			// live: a pod that crashed once ~an hour earlier and fully recovered stayed stuck
			// re-polling every 10s and reporting Error forever without this guard).
			crashed := inCrashLoop ||
				(!cs.Ready && cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.ExitCode != 0)
			if crashed {
				log.Info("ThunderID pod has crashed", "pod", pod.Name)
				if obj.Status.Phase != "Error" {
					obj.Status.Phase = "Error"
					obj.Status.Synced = false
					_ = r.Status().Update(ctx, obj)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, obj, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if obj.Status.Phase != "Ready" || !obj.Status.Synced {
		r.Recorder.Event(obj, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; resource is active")
		obj.Status.Phase = "Ready"
		obj.Status.Synced = true
		if err := r.Status().Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, resource ready", "name", obj.Name)
	}
	return ctrl.Result{}, nil
}

func (r *ResourceReconciler) fetchAndEmitPodErrors(ctx context.Context, obj *appsv1alpha1.Resource, namespace, podName string) {
	req := r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Previous:  true,
		Container: "thunderid",
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		req = r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: "thunderid",
		})
		rc, err = req.Stream(ctx)
	}
	if err != nil {
		r.Recorder.Event(obj, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(obj, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff", podName))
		return
	}

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
	meta, ok := resourceTypeMeta[resourceType]
	if !ok {
		return false, fmt.Errorf("spec.resource_type %q is not a known ThunderID resource_type", resourceType)
	}

	list := &appsv1alpha1.ResourceList{}
	if err := r.List(ctx, list, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Name < list.Items[j].Name
	})

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
		yamlDoc, err := r.buildResourceYAML(o, itemType, itemData)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for resource %s: %w", o.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	content := buf.String()

	h := sha256.New()
	h.Write([]byte(content))
	version := hex.EncodeToString(h.Sum(nil))[:16]

	versionAnnotation := "thunderid.io/" + meta.configMapSuffix + "-version"
	if instance.Annotations[versionAnnotation] == version {
		return false, nil
	}

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

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations[versionAnnotation] = version
	return true, r.Patch(ctx, instance, patch)
}

// buildResourceYAML generates a YAML document for a single resource in the format ThunderID's
// declarative resource loader expects. data is o.Spec already decoded (see parseResourceSpec) —
// the caller parses once and passes it in rather than every item re-parsing its own spec twice.
func (r *ResourceReconciler) buildResourceYAML(o appsv1alpha1.Resource, resourceType string, data map[string]interface{}) (string, error) {
	// flow/theme need a UUIDv7-shaped id — ThunderID requires it for these two kinds
	// specifically; every other kind accepts the k8s object's own (v4) UID as-is.
	if resourceType == "flow" || resourceType == "theme" {
		data["id"] = deterministicUUIDv7(o.ObjectMeta)
	} else {
		data["id"] = string(o.UID)
	}

	var buf bytes.Buffer
	writeYAMLMapBody(&buf, "", data)
	return buf.String(), nil
}

func (r *ResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Resource{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: o.Name, Namespace: o.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: o.Name, Namespace: o.Namespace},
				})
			}
		}
	}
	return reqs
}

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

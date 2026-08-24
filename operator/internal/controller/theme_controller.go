package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const themeFinalizer = "thunderid.io/theme-cleanup"

type ThemeReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=themes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=themes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *ThemeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	theme := &appsv1alpha1.Theme{}
	if err := r.Get(ctx, req.NamespacedName, theme); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: theme.Namespace, Name: theme.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", theme.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", theme.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	// Handle deletion: rebuild ConfigMap without this theme, then remove finalizer.
	if !theme.DeletionTimestamp.IsZero() {
		if containsString(theme.Finalizers, themeFinalizer) {
			if _, err := r.rebuildThemesConfigMap(ctx, instance, string(theme.UID)); err != nil {
				return ctrl.Result{}, err
			}
			theme.Finalizers = removeString(theme.Finalizers, themeFinalizer)
			if err := r.Update(ctx, theme); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present so we can clean up on deletion.
	// Return immediately after adding it — the Update triggers a new reconcile
	// with a fresh resourceVersion, avoiding a stale-object conflict on status update.
	if !containsString(theme.Finalizers, themeFinalizer) {
		theme.Finalizers = append(theme.Finalizers, themeFinalizer)
		if err := r.Update(ctx, theme); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildThemesConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync themes ConfigMap")
		r.Recorder.Event(theme, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "ThemeSyncFailed", fmt.Sprintf("Theme %s: %v", theme.Name, err))
		theme.Status.Phase = "Error"
		theme.Status.Synced = false
		_ = r.Status().Update(ctx, theme)
		return ctrl.Result{}, err
	}
	// Phase=="Pending" means we already synced and are waiting to check pod health.
	// Use LastSyncTime to ignore the immediate reconcile triggered by our own status update.
	if theme.Status.Phase == "Pending" && !changed {
		if theme.Status.LastSyncTime != nil {
			elapsed := time.Since(theme.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, theme, instance)
	}

	// Phase=="Error": re-check health only when driven by a Deployment watch event.
	// That reconcile is triggered by the Deployment becoming healthy, not by polling.
	if theme.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, theme, instance)
	}

	if !changed && theme.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, theme, instance)
	}

	// Content changed (or first sync) — record the time and wait for ThunderID to reload.
	now := metav1.Now()
	r.Recorder.Event(theme, corev1.EventTypeNormal, "Synced", "Theme synced to ConfigMap, waiting for ThunderID to apply")
	theme.Status.Phase = "Pending"
	theme.Status.Synced = false
	theme.Status.LastSyncTime = &now
	theme.Status.ThemeID = deterministicUUIDv7(theme.ObjectMeta)
	if err := r.Status().Update(ctx, theme); err != nil {
		log.Error(err, "failed to update theme status")
		return ctrl.Result{}, err
	}

	log.Info("theme synced; requeuing in 30s to verify ThunderID health", "name", theme.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *ThemeReconciler) checkThunderIDHealth(ctx context.Context, theme *appsv1alpha1.Theme, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
			crashed := inCrashLoop ||
				(cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.ExitCode != 0)
			if crashed {
				log.Info("ThunderID pod has crashed", "pod", pod.Name)
				if theme.Status.Phase != "Error" {
					theme.Status.Phase = "Error"
					theme.Status.Synced = false
					_ = r.Status().Update(ctx, theme)
				}
				if !inCrashLoop {
					// Container logs not yet flushed by containerd on first crash.
					// Requeue in 10s when the pod reaches stable CrashLoopBackOff.
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				// Pod is in stable CrashLoopBackOff — logs are reliably available now.
				r.fetchAndEmitPodErrors(ctx, theme, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	// Pod is healthy — only update status when transitioning out of a non-Ready state
	// to avoid a status-update → reconcile → status-update loop.
	if theme.Status.Phase != "Ready" || !theme.Status.Synced {
		r.Recorder.Event(theme, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; theme is active")
		theme.Status.Phase = "Ready"
		theme.Status.Synced = true
		if err := r.Status().Update(ctx, theme); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, theme ready", "name", theme.Name)
	}
	return ctrl.Result{}, nil
}

func (r *ThemeReconciler) fetchAndEmitPodErrors(ctx context.Context, theme *appsv1alpha1.Theme, namespace, podName string) {
	req := r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Previous:  true,
		Container: "thunderid",
	})
	rc, err := req.Stream(ctx) // opens streaming connection to thunderid pod logs
	if err != nil {
		// Previous container logs unavailable (fast crash / containerd GC); try current container.
		req = r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: "thunderid",
		})
		rc, err = req.Stream(ctx)
	}
	if err != nil {
		r.Recorder.Event(theme, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc) // read the logs
	if err != nil {
		r.Recorder.Event(theme, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(theme, corev1.EventTypeWarning, "ThunderIDError", line)
			errCount++
			if errCount >= 5 {
				break
			}
		}
	}

	if errCount == 0 {
		// No structured ERROR lines; surface the last 5 non-empty lines as context.
		var tail []string
		for i := len(lines) - 1; i >= 0 && len(tail) < 5; i-- {
			if lines[i] != "" {
				tail = append([]string{lines[i]}, tail...)
			}
		}
		for _, line := range tail {
			r.Recorder.Event(theme, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildThemesConfigMap returns true if the ConfigMap content changed. It lists
// all themes for the instance, generates YAML for each, writes them to the
// <instanceName>-themes ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific theme (used when that theme is being deleted).
func (r *ThemeReconciler) rebuildThemesConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	themeList := &appsv1alpha1.ThemeList{}
	if err := r.List(ctx, themeList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	// List() order is not guaranteed stable across calls; sort so the built
	// content (and its hash) doesn't change unless a theme actually did.
	sort.Slice(themeList.Items, func(i, j int) bool {
		return themeList.Items[i].Name < themeList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, t := range themeList.Items {
		if t.Labels[appsv1alpha1.InstanceRefLabel] != instance.Name {
			continue
		}
		if excludeUID != "" && string(t.UID) == excludeUID {
			continue
		}
		if !t.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := buildThemeYAML(t)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for theme %s: %w", t.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	themesContent := buf.String()

	h := sha256.New()
	h.Write([]byte(themesContent))
	themesVersion := hex.EncodeToString(h.Sum(nil))[:16]

	// Idempotency guard: if the annotation already reflects the current hash the
	// ConfigMap is up-to-date (avoids a false "changed" on the informer-cache
	// reconcile that fires immediately after we patch the annotation below).
	if instance.Annotations["thunderid.io/themes-version"] == themesVersion {
		return false, nil
	}

	cmName := instance.Name + "-themes"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"themes.yaml": themesContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create themes ConfigMap: %w", err)
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
			latest.Data["themes.yaml"] = themesContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update themes ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/themes-version"] = themesVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildThemeYAML generates a YAML document for a single theme in bootstrap
// format (resource_type: as a YAML key).
func buildThemeYAML(t appsv1alpha1.Theme) (string, error) {
	return renderResourceYAML(t.Spec.Config, map[string]interface{}{
		"resource_type": "theme",
		"id":            deterministicUUIDv7(t.ObjectMeta),
	})
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

func (r *ThemeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Theme{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.themesForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.themesForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("theme").
		Complete(r)
}

// themesForDeployment maps a Deployment change to Themes that reference it.
// For Phase=Error themes: always re-trigger (pod may have recovered).
// For Phase=Ready themes: re-trigger only when the Deployment is unhealthy (new pod may be crashing).
func (r *ThemeReconciler) themesForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	themeList := &appsv1alpha1.ThemeList{}
	if err := r.List(ctx, themeList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, theme := range themeList.Items {
		if theme.Labels[appsv1alpha1.InstanceRefLabel] != dep.GetName() {
			continue
		}
		switch theme.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: theme.Name, Namespace: theme.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: theme.Name, Namespace: theme.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *ThemeReconciler) themesForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	themeList := &appsv1alpha1.ThemeList{}
	if err := r.List(ctx, themeList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, theme := range themeList.Items {
		if theme.Labels[appsv1alpha1.InstanceRefLabel] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: theme.Name, Namespace: theme.Namespace},
			})
		}
	}
	return reqs
}

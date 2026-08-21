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

const translationFinalizer = "thunderid.io/translation-cleanup"

type TranslationReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=translations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=translations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *TranslationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	t := &appsv1alpha1.Translation{}
	if err := r.Get(ctx, req.NamespacedName, t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", t.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", t.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	if !t.DeletionTimestamp.IsZero() {
		if containsString(t.Finalizers, translationFinalizer) {
			if _, err := r.rebuildTranslationsConfigMap(ctx, instance, string(t.UID)); err != nil {
				return ctrl.Result{}, err
			}
			t.Finalizers = removeString(t.Finalizers, translationFinalizer)
			if err := r.Update(ctx, t); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(t.Finalizers, translationFinalizer) {
		t.Finalizers = append(t.Finalizers, translationFinalizer)
		if err := r.Update(ctx, t); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildTranslationsConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync translations ConfigMap")
		r.Recorder.Event(t, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "TranslationSyncFailed", fmt.Sprintf("Translation %s: %v", t.Name, err))
		t.Status.Phase = "Error"
		t.Status.Synced = false
		_ = r.Status().Update(ctx, t)
		return ctrl.Result{}, err
	}

	if t.Status.Phase == "Pending" && !changed {
		if t.Status.LastSyncTime != nil {
			elapsed := time.Since(t.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, t, instance)
	}

	if t.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, t, instance)
	}

	if !changed && t.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, t, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(t, corev1.EventTypeNormal, "Synced", "Translation synced to ConfigMap, waiting for ThunderID to apply")
	t.Status.Phase = "Pending"
	t.Status.Synced = false
	t.Status.LastSyncTime = &now
	if err := r.Status().Update(ctx, t); err != nil {
		log.Error(err, "failed to update translation status")
		return ctrl.Result{}, err
	}

	log.Info("translation synced; requeuing in 30s to verify ThunderID health", "name", t.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *TranslationReconciler) checkThunderIDHealth(ctx context.Context, t *appsv1alpha1.Translation, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if t.Status.Phase != "Error" {
					t.Status.Phase = "Error"
					t.Status.Synced = false
					_ = r.Status().Update(ctx, t)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, t, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if t.Status.Phase != "Ready" || !t.Status.Synced {
		r.Recorder.Event(t, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; translation is active")
		t.Status.Phase = "Ready"
		t.Status.Synced = true
		if err := r.Status().Update(ctx, t); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, translation ready", "name", t.Name)
	}
	return ctrl.Result{}, nil
}

func (r *TranslationReconciler) fetchAndEmitPodErrors(ctx context.Context, t *appsv1alpha1.Translation, namespace, podName string) {
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
		r.Recorder.Event(t, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(t, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(t, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(t, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildTranslationsConfigMap returns true if the ConfigMap content changed.
func (r *TranslationReconciler) rebuildTranslationsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	list := &appsv1alpha1.TranslationList{}
	if err := r.List(ctx, list, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Spec.Language < list.Items[j].Spec.Language
	})

	var buf bytes.Buffer
	for _, tr := range list.Items {
		if tr.Labels[appsv1alpha1.InstanceRefLabel] != instance.Name {
			continue
		}
		if excludeUID != "" && string(tr.UID) == excludeUID {
			continue
		}
		if !tr.DeletionTimestamp.IsZero() {
			continue
		}
		buf.WriteString("---\n")
		buf.WriteString(buildTranslationYAML(tr))
	}
	content := buf.String()

	h := sha256.New()
	h.Write([]byte(content))
	version := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/translations-version"] == version {
		return false, nil
	}

	cmName := instance.Name + "-translations"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"translations.yaml": content},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create translations ConfigMap: %w", err)
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
			latest.Data["translations.yaml"] = content
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update translations ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/translations-version"] = version
	return true, r.Patch(ctx, instance, patch)
}

func buildTranslationYAML(t appsv1alpha1.Translation) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# File: %s.yaml\n", t.Spec.Language)
	buf.WriteString("resource_type: translation\n")
	fmt.Fprintf(&buf, "language: %s\n", yamlSingleQuote(t.Spec.Language))
	buf.WriteString("translations:\n")

	namespaces := make([]string, 0, len(t.Spec.Translations))
	for ns := range t.Spec.Translations {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	for _, ns := range namespaces {
		fmt.Fprintf(&buf, "  %s:\n", ns)
		keyMap := t.Spec.Translations[ns]
		keys := make([]string, 0, len(keyMap))
		for k := range keyMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&buf, "    %s: %s\n", k, yamlSingleQuote(keyMap[k]))
		}
	}

	return buf.String()
}

func (r *TranslationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Translation{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.translationsForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.translationsForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("translation").
		Complete(r)
}

func (r *TranslationReconciler) translationsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	list := &appsv1alpha1.TranslationList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, t := range list.Items {
		if t.Labels[appsv1alpha1.InstanceRefLabel] != dep.GetName() {
			continue
		}
		switch t.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: t.Name, Namespace: t.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: t.Name, Namespace: t.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *TranslationReconciler) translationsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &appsv1alpha1.TranslationList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, t := range list.Items {
		if t.Labels[appsv1alpha1.InstanceRefLabel] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: t.Name, Namespace: t.Namespace},
			})
		}
	}
	return reqs
}

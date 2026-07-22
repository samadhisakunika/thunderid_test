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

const serverConfigFinalizer = "thunderid.io/serverconfig-cleanup"

type ServerConfigReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=serverconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=serverconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *ServerConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	sc := &appsv1alpha1.ServerConfig{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sc.Namespace, Name: sc.Spec.InstanceRef}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", sc.Spec.InstanceRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", sc.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !sc.DeletionTimestamp.IsZero() {
		if containsString(sc.Finalizers, serverConfigFinalizer) {
			if _, err := r.rebuildServerConfigsConfigMap(ctx, instance, string(sc.UID)); err != nil {
				return ctrl.Result{}, err
			}
			sc.Finalizers = removeString(sc.Finalizers, serverConfigFinalizer)
			if err := r.Update(ctx, sc); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(sc.Finalizers, serverConfigFinalizer) {
		sc.Finalizers = append(sc.Finalizers, serverConfigFinalizer)
		if err := r.Update(ctx, sc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildServerConfigsConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync server configs ConfigMap")
		r.Recorder.Event(sc, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "ServerConfigSyncFailed", fmt.Sprintf("ServerConfig %s: %v", sc.Name, err))
		sc.Status.Phase = "Error"
		sc.Status.Synced = false
		_ = r.Status().Update(ctx, sc)
		return ctrl.Result{}, err
	}

	if sc.Status.Phase == "Pending" && !changed {
		if sc.Status.LastSyncTime != nil {
			elapsed := time.Since(sc.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, sc, instance)
	}

	if sc.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, sc, instance)
	}

	if !changed && sc.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, sc, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(sc, corev1.EventTypeNormal, "Synced", "ServerConfig synced to ConfigMap, waiting for ThunderID to apply")
	sc.Status.Phase = "Pending"
	sc.Status.Synced = false
	sc.Status.LastSyncTime = &now
	if err := r.Status().Update(ctx, sc); err != nil {
		log.Error(err, "failed to update server config status")
		return ctrl.Result{}, err
	}

	log.Info("server config synced; requeuing in 30s to verify ThunderID health", "name", sc.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *ServerConfigReconciler) checkThunderIDHealth(ctx context.Context, sc *appsv1alpha1.ServerConfig, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if sc.Status.Phase != "Error" {
					sc.Status.Phase = "Error"
					sc.Status.Synced = false
					_ = r.Status().Update(ctx, sc)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, sc, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if sc.Status.Phase != "Ready" || !sc.Status.Synced {
		r.Recorder.Event(sc, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; server config is active")
		sc.Status.Phase = "Ready"
		sc.Status.Synced = true
		if err := r.Status().Update(ctx, sc); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, server config ready", "name", sc.Name)
	}
	return ctrl.Result{}, nil
}

func (r *ServerConfigReconciler) fetchAndEmitPodErrors(ctx context.Context, sc *appsv1alpha1.ServerConfig, namespace, podName string) {
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
		r.Recorder.Event(sc, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(sc, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(sc, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(sc, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildServerConfigsConfigMap returns true if the ConfigMap content changed.
func (r *ServerConfigReconciler) rebuildServerConfigsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	list := &appsv1alpha1.ServerConfigList{}
	if err := r.List(ctx, list, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Spec.Name < list.Items[j].Spec.Name
	})

	var buf bytes.Buffer
	for _, s := range list.Items {
		if s.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(s.UID) == excludeUID {
			continue
		}
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := buildServerConfigYAML(s)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for server config %s: %w", s.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	content := buf.String()

	h := sha256.New()
	h.Write([]byte(content))
	version := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/serverconfigs-version"] == version {
		return false, nil
	}

	cmName := instance.Name + "-serverconfigs"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"server_configs.yaml": content},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create server configs ConfigMap: %w", err)
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
			latest.Data["server_configs.yaml"] = content
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update server configs ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/serverconfigs-version"] = version
	return true, r.Patch(ctx, instance, patch)
}

func buildServerConfigYAML(s appsv1alpha1.ServerConfig) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("# resource_type: server_config\n")
	fmt.Fprintf(&buf, "name: %s\n", s.Spec.Name)
	buf.WriteString("value:\n")

	switch s.Spec.Name {
	case "cors":
		if s.Spec.CORS != nil {
			buf.WriteString("  allowedOrigins:\n")
			for _, o := range s.Spec.CORS.AllowedOrigins {
				if o.Regex != "" {
					fmt.Fprintf(&buf, "    - regex: %s\n", yamlSingleQuote(o.Regex))
				} else if o.URL != "" {
					fmt.Fprintf(&buf, "    - %s\n", yamlSingleQuote(o.URL))
				}
			}
		}
	case "session":
		if s.Spec.Session != nil {
			if s.Spec.Session.IdleTimeoutSeconds > 0 {
				fmt.Fprintf(&buf, "  idleTimeoutSeconds: %d\n", s.Spec.Session.IdleTimeoutSeconds)
			}
			if s.Spec.Session.AbsoluteTimeoutSeconds > 0 {
				fmt.Fprintf(&buf, "  absoluteTimeoutSeconds: %d\n", s.Spec.Session.AbsoluteTimeoutSeconds)
			}
		}
	case "defaultResourceServer":
		if s.Spec.DefaultResourceServer != nil {
			fmt.Fprintf(&buf, "  resourceServerId: %q\n", s.Spec.DefaultResourceServer.ResourceServerID)
		}
	default:
		return "", fmt.Errorf("unknown server config name %q", s.Spec.Name)
	}

	return buf.String(), nil
}

func (r *ServerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.ServerConfig{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.serverConfigsForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.serverConfigsForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("serverconfig").
		Complete(r)
}

func (r *ServerConfigReconciler) serverConfigsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	list := &appsv1alpha1.ServerConfigList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, sc := range list.Items {
		if sc.Spec.InstanceRef != dep.GetName() {
			continue
		}
		switch sc.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: sc.Name, Namespace: sc.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: sc.Name, Namespace: sc.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *ServerConfigReconciler) serverConfigsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &appsv1alpha1.ServerConfigList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, sc := range list.Items {
		if sc.Spec.InstanceRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: sc.Name, Namespace: sc.Namespace},
			})
		}
	}
	return reqs
}

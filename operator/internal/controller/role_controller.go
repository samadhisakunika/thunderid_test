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

const roleFinalizer = "thunderid.io/role-cleanup"

type RoleReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=roles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *RoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	role := &appsv1alpha1.Role{}
	if err := r.Get(ctx, req.NamespacedName, role); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: role.Namespace, Name: role.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", role.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", role.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	if !role.DeletionTimestamp.IsZero() {
		if containsString(role.Finalizers, roleFinalizer) {
			if _, err := r.rebuildRolesConfigMap(ctx, instance, string(role.UID)); err != nil {
				return ctrl.Result{}, err
			}
			role.Finalizers = removeString(role.Finalizers, roleFinalizer)
			if err := r.Update(ctx, role); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(role.Finalizers, roleFinalizer) {
		role.Finalizers = append(role.Finalizers, roleFinalizer)
		if err := r.Update(ctx, role); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildRolesConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync roles ConfigMap")
		r.Recorder.Event(role, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "RoleSyncFailed", fmt.Sprintf("Role %s: %v", role.Name, err))
		role.Status.Phase = "Error"
		role.Status.Synced = false
		_ = r.Status().Update(ctx, role)
		return ctrl.Result{}, err
	}
	if role.Status.Phase == "Pending" && !changed {
		if role.Status.LastSyncTime != nil {
			elapsed := time.Since(role.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, role, instance)
	}

	if role.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, role, instance)
	}

	if !changed && role.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, role, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(role, corev1.EventTypeNormal, "Synced", "Role synced to ConfigMap, waiting for ThunderID to apply")
	role.Status.Phase = "Pending"
	role.Status.Synced = false
	role.Status.LastSyncTime = &now
	role.Status.RoleID = string(role.UID)
	if err := r.Status().Update(ctx, role); err != nil {
		log.Error(err, "failed to update role status")
		return ctrl.Result{}, err
	}

	log.Info("role synced; requeuing in 30s to verify ThunderID health", "name", role.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *RoleReconciler) checkThunderIDHealth(ctx context.Context, role *appsv1alpha1.Role, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if role.Status.Phase != "Error" {
					role.Status.Phase = "Error"
					role.Status.Synced = false
					_ = r.Status().Update(ctx, role)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, role, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if role.Status.Phase != "Ready" || !role.Status.Synced {
		r.Recorder.Event(role, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; role is active")
		role.Status.Phase = "Ready"
		role.Status.Synced = true
		if err := r.Status().Update(ctx, role); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, role ready", "name", role.Name)
	}
	return ctrl.Result{}, nil
}

func (r *RoleReconciler) fetchAndEmitPodErrors(ctx context.Context, role *appsv1alpha1.Role, namespace, podName string) {
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
		r.Recorder.Event(role, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(role, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(role, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(role, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildRolesConfigMap returns true if the ConfigMap content changed. It lists all
// roles for the instance, generates YAML for each, writes them to the
// <instanceName>-roles ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific role (used when it is being deleted).
func (r *RoleReconciler) rebuildRolesConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	roleList := &appsv1alpha1.RoleList{}
	if err := r.List(ctx, roleList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(roleList.Items, func(i, j int) bool {
		return roleList.Items[i].Name < roleList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, ro := range roleList.Items {
		if ro.Labels[appsv1alpha1.InstanceRefLabel] != instance.Name {
			continue
		}
		if excludeUID != "" && string(ro.UID) == excludeUID {
			continue
		}
		if !ro.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := buildRoleYAML(ro)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for role %s: %w", ro.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	rolesContent := buf.String()

	h := sha256.New()
	h.Write([]byte(rolesContent))
	rolesVersion := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/roles-version"] == rolesVersion {
		return false, nil
	}

	cmName := instance.Name + "-roles"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"roles.yaml": rolesContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create roles ConfigMap: %w", err)
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
			latest.Data["roles.yaml"] = rolesContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update roles ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/roles-version"] = rolesVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildRoleYAML generates a YAML document for a single role in the format
// ThunderID's declarative resource loader expects.
func buildRoleYAML(ro appsv1alpha1.Role) (string, error) {
	return renderResourceYAML(ro.Spec.Config, map[string]interface{}{
		"resource_type": "role",
		"id":            string(ro.UID),
	})
}

func (r *RoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Role{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.rolesForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.rolesForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("role").
		Complete(r)
}

// rolesForDeployment maps a Deployment change to Roles that reference it.
// For Phase=Error roles: always re-trigger (pod may have recovered).
// For Phase=Ready roles: re-trigger only when the Deployment is unhealthy.
func (r *RoleReconciler) rolesForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	roleList := &appsv1alpha1.RoleList{}
	if err := r.List(ctx, roleList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, role := range roleList.Items {
		if role.Labels[appsv1alpha1.InstanceRefLabel] != dep.GetName() {
			continue
		}
		switch role.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: role.Name, Namespace: role.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: role.Name, Namespace: role.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *RoleReconciler) rolesForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	roleList := &appsv1alpha1.RoleList{}
	if err := r.List(ctx, roleList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, role := range roleList.Items {
		if role.Labels[appsv1alpha1.InstanceRefLabel] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: role.Name, Namespace: role.Namespace},
			})
		}
	}
	return reqs
}

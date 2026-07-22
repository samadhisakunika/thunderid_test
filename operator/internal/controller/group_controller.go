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

const groupFinalizer = "thunderid.io/group-cleanup"

type GroupReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=groups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=groups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	group := &appsv1alpha1.Group{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Spec.InstanceRef}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", group.Spec.InstanceRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", group.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !group.DeletionTimestamp.IsZero() {
		if containsString(group.Finalizers, groupFinalizer) {
			if _, err := r.rebuildGroupsConfigMap(ctx, instance, string(group.UID)); err != nil {
				return ctrl.Result{}, err
			}
			group.Finalizers = removeString(group.Finalizers, groupFinalizer)
			if err := r.Update(ctx, group); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(group.Finalizers, groupFinalizer) {
		group.Finalizers = append(group.Finalizers, groupFinalizer)
		if err := r.Update(ctx, group); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildGroupsConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync groups ConfigMap")
		r.Recorder.Event(group, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "GroupSyncFailed", fmt.Sprintf("Group %s: %v", group.Name, err))
		group.Status.Phase = "Error"
		group.Status.Synced = false
		_ = r.Status().Update(ctx, group)
		return ctrl.Result{}, err
	}
	if group.Status.Phase == "Pending" && !changed {
		if group.Status.LastSyncTime != nil {
			elapsed := time.Since(group.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, group, instance)
	}

	if group.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, group, instance)
	}

	if !changed && group.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, group, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(group, corev1.EventTypeNormal, "Synced", "Group synced to ConfigMap, waiting for ThunderID to apply")
	group.Status.Phase = "Pending"
	group.Status.Synced = false
	group.Status.LastSyncTime = &now
	group.Status.GroupID = string(group.UID)
	if err := r.Status().Update(ctx, group); err != nil {
		log.Error(err, "failed to update group status")
		return ctrl.Result{}, err
	}

	log.Info("group synced; requeuing in 30s to verify ThunderID health", "name", group.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *GroupReconciler) checkThunderIDHealth(ctx context.Context, group *appsv1alpha1.Group, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if group.Status.Phase != "Error" {
					group.Status.Phase = "Error"
					group.Status.Synced = false
					_ = r.Status().Update(ctx, group)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, group, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if group.Status.Phase != "Ready" || !group.Status.Synced {
		r.Recorder.Event(group, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; group is active")
		group.Status.Phase = "Ready"
		group.Status.Synced = true
		if err := r.Status().Update(ctx, group); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, group ready", "name", group.Name)
	}
	return ctrl.Result{}, nil
}

func (r *GroupReconciler) fetchAndEmitPodErrors(ctx context.Context, group *appsv1alpha1.Group, namespace, podName string) {
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
		r.Recorder.Event(group, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(group, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(group, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(group, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildGroupsConfigMap returns true if the ConfigMap content changed. It lists all
// groups for the instance, generates YAML for each, writes them to the
// <instanceName>-groups ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific group (used when it is being deleted).
func (r *GroupReconciler) rebuildGroupsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	groupList := &appsv1alpha1.GroupList{}
	if err := r.List(ctx, groupList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(groupList.Items, func(i, j int) bool {
		return groupList.Items[i].Name < groupList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, g := range groupList.Items {
		if g.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(g.UID) == excludeUID {
			continue
		}
		if !g.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := r.buildGroupYAML(ctx, g)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for group %s: %w", g.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	groupsContent := buf.String()

	h := sha256.New()
	h.Write([]byte(groupsContent))
	groupsVersion := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/groups-version"] == groupsVersion {
		return false, nil
	}

	cmName := instance.Name + "-groups"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"groups.yaml": groupsContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create groups ConfigMap: %w", err)
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
			latest.Data["groups.yaml"] = groupsContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update groups ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/groups-version"] = groupsVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildGroupYAML generates a YAML document for a single group in the format
// ThunderID's declarative resource loader expects.
func (r *GroupReconciler) buildGroupYAML(ctx context.Context, g appsv1alpha1.Group) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("# resource_type: group\n")
	fmt.Fprintf(&buf, "id: %q\n", string(g.UID))
	fmt.Fprintf(&buf, "name: %s\n", yamlSingleQuote(g.Spec.Name))
	if g.Spec.Description != "" {
		fmt.Fprintf(&buf, "description: %s\n", yamlSingleQuote(g.Spec.Description))
	}

	if g.Spec.OUID != "" {
		fmt.Fprintf(&buf, "ouId: %q\n", g.Spec.OUID)
	} else if g.Spec.OUHandle != "" {
		if uid := resolveOUHandle(ctx, r.Client, g.Namespace, g.Spec.OUHandle); uid != "" {
			fmt.Fprintf(&buf, "ouId: %q\n", uid)
		} else {
			fmt.Fprintf(&buf, "ouHandle: %s\n", yamlSingleQuote(g.Spec.OUHandle))
		}
	}

	if len(g.Spec.Members) > 0 {
		buf.WriteString("members:\n")
		for _, m := range g.Spec.Members {
			fmt.Fprintf(&buf, "  - id: %q\n", m.ID)
			fmt.Fprintf(&buf, "    type: %s\n", m.Type)
		}
	}

	return buf.String(), nil
}

func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Group{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.groupsForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.groupsForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.OrganizationUnit{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexGroupOUHandle, func() client.ObjectList { return &appsv1alpha1.GroupList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.OrganizationUnit).Spec.Handle }))).
		Named("group").
		Complete(r)
}

// groupsForDeployment maps a Deployment change to Groups that reference it.
// For Phase=Error groups: always re-trigger (pod may have recovered).
// For Phase=Ready groups: re-trigger only when the Deployment is unhealthy.
func (r *GroupReconciler) groupsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	groupList := &appsv1alpha1.GroupList{}
	if err := r.List(ctx, groupList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, group := range groupList.Items {
		if group.Spec.InstanceRef != dep.GetName() {
			continue
		}
		switch group.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: group.Name, Namespace: group.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: group.Name, Namespace: group.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *GroupReconciler) groupsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	groupList := &appsv1alpha1.GroupList{}
	if err := r.List(ctx, groupList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, group := range groupList.Items {
		if group.Spec.InstanceRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: group.Name, Namespace: group.Namespace},
			})
		}
	}
	return reqs
}

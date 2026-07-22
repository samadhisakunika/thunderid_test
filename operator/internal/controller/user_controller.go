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

const userFinalizer = "thunderid.io/user-cleanup"

type UserReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=users,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=users/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	user := &appsv1alpha1.User{}
	if err := r.Get(ctx, req.NamespacedName, user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: user.Namespace, Name: user.Spec.InstanceRef}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", user.Spec.InstanceRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", user.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !user.DeletionTimestamp.IsZero() {
		if containsString(user.Finalizers, userFinalizer) {
			if _, err := r.rebuildUsersConfigMap(ctx, instance, string(user.UID)); err != nil {
				return ctrl.Result{}, err
			}
			user.Finalizers = removeString(user.Finalizers, userFinalizer)
			if err := r.Update(ctx, user); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(user.Finalizers, userFinalizer) {
		user.Finalizers = append(user.Finalizers, userFinalizer)
		if err := r.Update(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildUsersConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync users ConfigMap")
		r.Recorder.Event(user, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "UserSyncFailed", fmt.Sprintf("User %s: %v", user.Name, err))
		user.Status.Phase = "Error"
		user.Status.Synced = false
		_ = r.Status().Update(ctx, user)
		return ctrl.Result{}, err
	}
	if user.Status.Phase == "Pending" && !changed {
		if user.Status.LastSyncTime != nil {
			elapsed := time.Since(user.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, user, instance)
	}

	if user.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, user, instance)
	}

	if !changed && user.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, user, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(user, corev1.EventTypeNormal, "Synced", "User synced to ConfigMap, waiting for ThunderID to apply")
	user.Status.Phase = "Pending"
	user.Status.Synced = false
	user.Status.LastSyncTime = &now
	user.Status.UserID = string(user.UID)
	if err := r.Status().Update(ctx, user); err != nil {
		log.Error(err, "failed to update user status")
		return ctrl.Result{}, err
	}

	log.Info("user synced; requeuing in 30s to verify ThunderID health", "name", user.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *UserReconciler) checkThunderIDHealth(ctx context.Context, user *appsv1alpha1.User, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if user.Status.Phase != "Error" {
					user.Status.Phase = "Error"
					user.Status.Synced = false
					_ = r.Status().Update(ctx, user)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, user, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if user.Status.Phase != "Ready" || !user.Status.Synced {
		r.Recorder.Event(user, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; user is active")
		user.Status.Phase = "Ready"
		user.Status.Synced = true
		if err := r.Status().Update(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, user ready", "name", user.Name)
	}
	return ctrl.Result{}, nil
}

func (r *UserReconciler) fetchAndEmitPodErrors(ctx context.Context, user *appsv1alpha1.User, namespace, podName string) {
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
		r.Recorder.Event(user, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(user, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(user, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(user, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildUsersConfigMap returns true if the ConfigMap content changed. It lists all
// users for the instance, generates YAML for each, writes them to the
// <instanceName>-users ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific user (used when it is being deleted).
func (r *UserReconciler) rebuildUsersConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	userList := &appsv1alpha1.UserList{}
	if err := r.List(ctx, userList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(userList.Items, func(i, j int) bool {
		return userList.Items[i].Name < userList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, u := range userList.Items {
		if u.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(u.UID) == excludeUID {
			continue
		}
		if !u.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := r.buildUserYAML(ctx, u)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for user %s: %w", u.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	usersContent := buf.String()

	h := sha256.New()
	h.Write([]byte(usersContent))
	usersVersion := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/users-version"] == usersVersion {
		return false, nil
	}

	cmName := instance.Name + "-users"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"users.yaml": usersContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create users ConfigMap: %w", err)
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
			latest.Data["users.yaml"] = usersContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update users ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/users-version"] = usersVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildUserYAML generates a YAML document for a single user in the format
// ThunderID's declarative resource loader expects.
func (r *UserReconciler) buildUserYAML(ctx context.Context, u appsv1alpha1.User) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("resource_type: user\n")
	fmt.Fprintf(&buf, "id: %q\n", string(u.UID))
	fmt.Fprintf(&buf, "type: %s\n", yamlSingleQuote(u.Spec.Type))

	if u.Spec.OUID != "" {
		fmt.Fprintf(&buf, "ouId: %q\n", u.Spec.OUID)
	} else if u.Spec.OUHandle != "" {
		if uid := resolveOUHandle(ctx, r.Client, u.Namespace, u.Spec.OUHandle); uid != "" {
			fmt.Fprintf(&buf, "ouId: %q\n", uid)
		} else {
			fmt.Fprintf(&buf, "ouHandle: %s\n", yamlSingleQuote(u.Spec.OUHandle))
		}
	}

	if len(u.Spec.Attributes) > 0 {
		buf.WriteString("attributes:\n")
		keys := make([]string, 0, len(u.Spec.Attributes))
		for k := range u.Spec.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&buf, "  %s: %s\n", k, yamlSingleQuote(u.Spec.Attributes[k]))
		}
	}

	return buf.String(), nil
}

func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.User{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.usersForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.usersForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.OrganizationUnit{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexUserOUHandle, func() client.ObjectList { return &appsv1alpha1.UserList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.OrganizationUnit).Spec.Handle }))).
		Named("user").
		Complete(r)
}

// usersForDeployment maps a Deployment change to Users that reference it.
// For Phase=Error users: always re-trigger (pod may have recovered).
// For Phase=Ready users: re-trigger only when the Deployment is unhealthy.
func (r *UserReconciler) usersForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	userList := &appsv1alpha1.UserList{}
	if err := r.List(ctx, userList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, user := range userList.Items {
		if user.Spec.InstanceRef != dep.GetName() {
			continue
		}
		switch user.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: user.Name, Namespace: user.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: user.Name, Namespace: user.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *UserReconciler) usersForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	userList := &appsv1alpha1.UserList{}
	if err := r.List(ctx, userList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, user := range userList.Items {
		if user.Spec.InstanceRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: user.Name, Namespace: user.Namespace},
			})
		}
	}
	return reqs
}

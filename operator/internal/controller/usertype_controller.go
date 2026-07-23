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

const userTypeFinalizer = "thunderid.io/usertype-cleanup"

type UserTypeReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=usertypes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=usertypes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *UserTypeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	ut := &appsv1alpha1.UserType{}
	if err := r.Get(ctx, req.NamespacedName, ut); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ut.Namespace, Name: ut.Spec.InstanceRef}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", ut.Spec.InstanceRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", ut.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !ut.DeletionTimestamp.IsZero() {
		if containsString(ut.Finalizers, userTypeFinalizer) {
			if _, err := r.rebuildUserTypesConfigMap(ctx, instance, string(ut.UID)); err != nil {
				return ctrl.Result{}, err
			}
			ut.Finalizers = removeString(ut.Finalizers, userTypeFinalizer)
			if err := r.Update(ctx, ut); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(ut.Finalizers, userTypeFinalizer) {
		ut.Finalizers = append(ut.Finalizers, userTypeFinalizer)
		if err := r.Update(ctx, ut); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildUserTypesConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync user types ConfigMap")
		r.Recorder.Event(ut, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "UserTypeSyncFailed", fmt.Sprintf("UserType %s: %v", ut.Name, err))
		ut.Status.Phase = "Error"
		ut.Status.Synced = false
		_ = r.Status().Update(ctx, ut)
		return ctrl.Result{}, err
	}
	if ut.Status.Phase == "Pending" && !changed {
		if ut.Status.LastSyncTime != nil {
			elapsed := time.Since(ut.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, ut, instance)
	}

	if ut.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, ut, instance)
	}

	if !changed && ut.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, ut, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(ut, corev1.EventTypeNormal, "Synced", "UserType synced to ConfigMap, waiting for ThunderID to apply")
	ut.Status.Phase = "Pending"
	ut.Status.Synced = false
	ut.Status.LastSyncTime = &now
	ut.Status.UserTypeID = string(ut.UID)
	if err := r.Status().Update(ctx, ut); err != nil {
		log.Error(err, "failed to update user type status")
		return ctrl.Result{}, err
	}

	log.Info("user type synced; requeuing in 30s to verify ThunderID health", "name", ut.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *UserTypeReconciler) checkThunderIDHealth(ctx context.Context, ut *appsv1alpha1.UserType, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if ut.Status.Phase != "Error" {
					ut.Status.Phase = "Error"
					ut.Status.Synced = false
					_ = r.Status().Update(ctx, ut)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, ut, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if ut.Status.Phase != "Ready" || !ut.Status.Synced {
		r.Recorder.Event(ut, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; user type is active")
		ut.Status.Phase = "Ready"
		ut.Status.Synced = true
		if err := r.Status().Update(ctx, ut); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, user type ready", "name", ut.Name)
	}
	return ctrl.Result{}, nil
}

func (r *UserTypeReconciler) fetchAndEmitPodErrors(ctx context.Context, ut *appsv1alpha1.UserType, namespace, podName string) {
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
		r.Recorder.Event(ut, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(ut, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(ut, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(ut, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildUserTypesConfigMap returns true if the ConfigMap content changed. It lists all
// user types for the instance, generates YAML for each, writes them to the
// <instanceName>-usertypes ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific user type (used when it is being deleted).
func (r *UserTypeReconciler) rebuildUserTypesConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	utList := &appsv1alpha1.UserTypeList{}
	if err := r.List(ctx, utList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(utList.Items, func(i, j int) bool {
		return utList.Items[i].Name < utList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, u := range utList.Items {
		if u.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(u.UID) == excludeUID {
			continue
		}
		if !u.DeletionTimestamp.IsZero() {
			continue
		}
		if u.Spec.Schema == "" {
			continue // ThunderID requires an attribute schema; skip incomplete user types
		}
		yamlDoc, err := r.buildUserTypeYAML(ctx, u)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for user type %s: %w", u.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	usertypesContent := buf.String()

	h := sha256.New()
	h.Write([]byte(usertypesContent))
	usertypesVersion := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/usertypes-version"] == usertypesVersion {
		return false, nil
	}

	cmName := instance.Name + "-usertypes"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"usertypes.yaml": usertypesContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create user types ConfigMap: %w", err)
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
			latest.Data["usertypes.yaml"] = usertypesContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update user types ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/usertypes-version"] = usertypesVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildUserTypeYAML generates a YAML document for a single user type in the format
// ThunderID's declarative resource loader expects.
func (r *UserTypeReconciler) buildUserTypeYAML(ctx context.Context, u appsv1alpha1.UserType) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("resource_type: user_type\n")
	fmt.Fprintf(&buf, "id: %q\n", string(u.UID))
	fmt.Fprintf(&buf, "category: %s\n", yamlSingleQuote(u.Spec.Category))
	fmt.Fprintf(&buf, "name: %s\n", yamlSingleQuote(u.Spec.Name))

	if u.Spec.OUID != "" {
		fmt.Fprintf(&buf, "ouId: %q\n", u.Spec.OUID)
	} else if u.Spec.OUHandle != "" {
		if uid := resolveOUHandle(ctx, r.Client, u.Namespace, u.Spec.OUHandle); uid != "" {
			fmt.Fprintf(&buf, "ouId: %q\n", uid)
		} else {
			fmt.Fprintf(&buf, "ouHandle: %s\n", yamlSingleQuote(u.Spec.OUHandle))
		}
	}

	if u.Spec.AllowSelfRegistration {
		fmt.Fprintf(&buf, "allowSelfRegistration: %v\n", u.Spec.AllowSelfRegistration)
	}

	if len(u.Spec.SystemAttributes) > 0 {
		buf.WriteString("systemAttributes:\n")
		keys := make([]string, 0, len(u.Spec.SystemAttributes))
		for k := range u.Spec.SystemAttributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&buf, "  %s: %s\n", k, yamlSingleQuote(u.Spec.SystemAttributes[k]))
		}
	}

	writeYAMLBlock(&buf, "schema", u.Spec.Schema)

	return buf.String(), nil
}

func (r *UserTypeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.UserType{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.userTypesForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.userTypesForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.OrganizationUnit{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexUserTypeOUHandle, func() client.ObjectList { return &appsv1alpha1.UserTypeList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.OrganizationUnit).Spec.Handle }))).
		Named("usertype").
		Complete(r)
}

// userTypesForDeployment maps a Deployment change to UserTypes that reference it.
// For Phase=Error user types: always re-trigger (pod may have recovered).
// For Phase=Ready user types: re-trigger only when the Deployment is unhealthy.
func (r *UserTypeReconciler) userTypesForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	utList := &appsv1alpha1.UserTypeList{}
	if err := r.List(ctx, utList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, ut := range utList.Items {
		if ut.Spec.InstanceRef != dep.GetName() {
			continue
		}
		switch ut.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ut.Name, Namespace: ut.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: ut.Name, Namespace: ut.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *UserTypeReconciler) userTypesForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	utList := &appsv1alpha1.UserTypeList{}
	if err := r.List(ctx, utList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, ut := range utList.Items {
		if ut.Spec.InstanceRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ut.Name, Namespace: ut.Namespace},
			})
		}
	}
	return reqs
}

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

const organizationUnitFinalizer = "thunderid.io/organizationunit-cleanup"

type OrganizationUnitReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *OrganizationUnitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	ou := &appsv1alpha1.OrganizationUnit{}
	if err := r.Get(ctx, req.NamespacedName, ou); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ou.Namespace, Name: ou.Spec.InstanceRef}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", ou.Spec.InstanceRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", ou.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !ou.DeletionTimestamp.IsZero() {
		if containsString(ou.Finalizers, organizationUnitFinalizer) {
			if _, err := r.rebuildOrganizationUnitsConfigMap(ctx, instance, string(ou.UID)); err != nil {
				return ctrl.Result{}, err
			}
			ou.Finalizers = removeString(ou.Finalizers, organizationUnitFinalizer)
			if err := r.Update(ctx, ou); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(ou.Finalizers, organizationUnitFinalizer) {
		ou.Finalizers = append(ou.Finalizers, organizationUnitFinalizer)
		if err := r.Update(ctx, ou); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildOrganizationUnitsConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync organization units ConfigMap")
		r.Recorder.Event(ou, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "OrganizationUnitSyncFailed", fmt.Sprintf("OrganizationUnit %s: %v", ou.Name, err))
		ou.Status.Phase = "Error"
		ou.Status.Synced = false
		_ = r.Status().Update(ctx, ou)
		return ctrl.Result{}, err
	}
	if ou.Status.Phase == "Pending" && !changed {
		if ou.Status.LastSyncTime != nil {
			elapsed := time.Since(ou.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, ou, instance)
	}

	if ou.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, ou, instance)
	}

	if !changed && ou.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, ou, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(ou, corev1.EventTypeNormal, "Synced", "OrganizationUnit synced to ConfigMap, waiting for ThunderID to apply")
	ou.Status.Phase = "Pending"
	ou.Status.Synced = false
	ou.Status.LastSyncTime = &now
	ou.Status.OrganizationUnitID = string(ou.UID)
	if err := r.Status().Update(ctx, ou); err != nil {
		log.Error(err, "failed to update organization unit status")
		return ctrl.Result{}, err
	}

	log.Info("organization unit synced; requeuing in 30s to verify ThunderID health", "name", ou.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *OrganizationUnitReconciler) checkThunderIDHealth(ctx context.Context, ou *appsv1alpha1.OrganizationUnit, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if ou.Status.Phase != "Error" {
					ou.Status.Phase = "Error"
					ou.Status.Synced = false
					_ = r.Status().Update(ctx, ou)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, ou, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if ou.Status.Phase != "Ready" || !ou.Status.Synced {
		r.Recorder.Event(ou, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; organization unit is active")
		ou.Status.Phase = "Ready"
		ou.Status.Synced = true
		if err := r.Status().Update(ctx, ou); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, organization unit ready", "name", ou.Name)
	}
	return ctrl.Result{}, nil
}

func (r *OrganizationUnitReconciler) fetchAndEmitPodErrors(ctx context.Context, ou *appsv1alpha1.OrganizationUnit, namespace, podName string) {
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
		r.Recorder.Event(ou, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(ou, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(ou, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(ou, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildOrganizationUnitsConfigMap returns true if the ConfigMap content changed. It lists
// all organization units for the instance, generates YAML for each, writes them to the
// <instanceName>-organizationunits ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific organization unit (used when it is being deleted).
func (r *OrganizationUnitReconciler) rebuildOrganizationUnitsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	ouList := &appsv1alpha1.OrganizationUnitList{}
	if err := r.List(ctx, ouList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(ouList.Items, func(i, j int) bool {
		return ouList.Items[i].Name < ouList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, o := range ouList.Items {
		if o.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(o.UID) == excludeUID {
			continue
		}
		if !o.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := r.buildOrganizationUnitYAML(ctx, o)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for organization unit %s: %w", o.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	ousContent := buf.String()

	h := sha256.New()
	h.Write([]byte(ousContent))
	ousVersion := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/organizationunits-version"] == ousVersion {
		return false, nil
	}

	cmName := instance.Name + "-organizationunits"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"organizationunits.yaml": ousContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create organization units ConfigMap: %w", err)
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
			latest.Data["organizationunits.yaml"] = ousContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update organization units ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/organizationunits-version"] = ousVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildOrganizationUnitYAML generates a YAML document for a single organization unit in
// the format ThunderID's declarative resource loader expects.
func (r *OrganizationUnitReconciler) buildOrganizationUnitYAML(ctx context.Context, o appsv1alpha1.OrganizationUnit) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("resource_type: organization_unit\n")
	fmt.Fprintf(&buf, "id: %q\n", string(o.UID))
	fmt.Fprintf(&buf, "handle: %s\n", yamlSingleQuote(o.Spec.Handle))
	fmt.Fprintf(&buf, "name: %s\n", yamlSingleQuote(o.Spec.Name))
	if o.Spec.Description != "" {
		fmt.Fprintf(&buf, "description: %s\n", yamlSingleQuote(o.Spec.Description))
	}

	if o.Spec.Parent != "" {
		fmt.Fprintf(&buf, "parent: %q\n", o.Spec.Parent)
	}

	if o.Spec.LogoURL != "" {
		fmt.Fprintf(&buf, "logoUrl: %s\n", yamlSingleQuote(o.Spec.LogoURL))
	}

	return buf.String(), nil
}

func (r *OrganizationUnitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.OrganizationUnit{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.organizationUnitsForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.organizationUnitsForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("organizationunit").
		Complete(r)
}

// organizationUnitsForDeployment maps a Deployment change to OrganizationUnits that reference it.
// For Phase=Error organization units: always re-trigger (pod may have recovered).
// For Phase=Ready organization units: re-trigger only when the Deployment is unhealthy.
func (r *OrganizationUnitReconciler) organizationUnitsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	ouList := &appsv1alpha1.OrganizationUnitList{}
	if err := r.List(ctx, ouList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, ou := range ouList.Items {
		if ou.Spec.InstanceRef != dep.GetName() {
			continue
		}
		switch ou.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ou.Name, Namespace: ou.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: ou.Name, Namespace: ou.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *OrganizationUnitReconciler) organizationUnitsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	ouList := &appsv1alpha1.OrganizationUnitList{}
	if err := r.List(ctx, ouList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, ou := range ouList.Items {
		if ou.Spec.InstanceRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ou.Name, Namespace: ou.Namespace},
			})
		}
	}
	return reqs
}

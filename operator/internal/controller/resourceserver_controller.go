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

const resourceServerFinalizer = "thunderid.io/resourceserver-cleanup"

type ResourceServerReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=resourceservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=resourceservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=organizationunits,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *ResourceServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	rs := &appsv1alpha1.ResourceServer{}
	if err := r.Get(ctx, req.NamespacedName, rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.InstanceRef}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", rs.Spec.InstanceRef)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", rs.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !rs.DeletionTimestamp.IsZero() {
		if containsString(rs.Finalizers, resourceServerFinalizer) {
			if _, err := r.rebuildResourceServersConfigMap(ctx, instance, string(rs.UID)); err != nil {
				return ctrl.Result{}, err
			}
			rs.Finalizers = removeString(rs.Finalizers, resourceServerFinalizer)
			if err := r.Update(ctx, rs); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(rs.Finalizers, resourceServerFinalizer) {
		rs.Finalizers = append(rs.Finalizers, resourceServerFinalizer)
		if err := r.Update(ctx, rs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildResourceServersConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync resource servers ConfigMap")
		r.Recorder.Event(rs, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "ResourceServerSyncFailed", fmt.Sprintf("ResourceServer %s: %v", rs.Name, err))
		rs.Status.Phase = "Error"
		rs.Status.Synced = false
		_ = r.Status().Update(ctx, rs)
		return ctrl.Result{}, err
	}
	if rs.Status.Phase == "Pending" && !changed {
		if rs.Status.LastSyncTime != nil {
			elapsed := time.Since(rs.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, rs, instance)
	}

	if rs.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, rs, instance)
	}

	if !changed && rs.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, rs, instance)
	}

	now := metav1.Now()
	r.Recorder.Event(rs, corev1.EventTypeNormal, "Synced", "ResourceServer synced to ConfigMap, waiting for ThunderID to apply")
	rs.Status.Phase = "Pending"
	rs.Status.Synced = false
	rs.Status.LastSyncTime = &now
	rs.Status.ResourceServerID = string(rs.UID)
	if err := r.Status().Update(ctx, rs); err != nil {
		log.Error(err, "failed to update resource server status")
		return ctrl.Result{}, err
	}

	log.Info("resource server synced; requeuing in 30s to verify ThunderID health", "name", rs.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *ResourceServerReconciler) checkThunderIDHealth(ctx context.Context, rs *appsv1alpha1.ResourceServer, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if rs.Status.Phase != "Error" {
					rs.Status.Phase = "Error"
					rs.Status.Synced = false
					_ = r.Status().Update(ctx, rs)
				}
				if !inCrashLoop {
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				r.fetchAndEmitPodErrors(ctx, rs, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	if rs.Status.Phase != "Ready" || !rs.Status.Synced {
		r.Recorder.Event(rs, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; resource server is active")
		rs.Status.Phase = "Ready"
		rs.Status.Synced = true
		if err := r.Status().Update(ctx, rs); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, resource server ready", "name", rs.Name)
	}
	return ctrl.Result{}, nil
}

func (r *ResourceServerReconciler) fetchAndEmitPodErrors(ctx context.Context, rs *appsv1alpha1.ResourceServer, namespace, podName string) {
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
		r.Recorder.Event(rs, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		r.Recorder.Event(rs, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(rs, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(rs, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildResourceServersConfigMap returns true if the ConfigMap content changed. It
// lists all resource servers for the instance, generates YAML for each, writes them
// to the <instanceName>-resourceservers ConfigMap, and annotates the
// ThunderIDInstance so its reconciler detects the change and triggers a rolling
// restart. excludeUID skips a specific resource server (used when it is being deleted).
func (r *ResourceServerReconciler) rebuildResourceServersConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	rsList := &appsv1alpha1.ResourceServerList{}
	if err := r.List(ctx, rsList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	sort.Slice(rsList.Items, func(i, j int) bool {
		return rsList.Items[i].Name < rsList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, s := range rsList.Items {
		if s.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(s.UID) == excludeUID {
			continue
		}
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := r.buildResourceServerYAML(ctx, s)
		if err != nil {
			return false, fmt.Errorf("failed to build YAML for resource server %s: %w", s.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	rsContent := buf.String()

	h := sha256.New()
	h.Write([]byte(rsContent))
	rsVersion := hex.EncodeToString(h.Sum(nil))[:16]

	if instance.Annotations["thunderid.io/resourceservers-version"] == rsVersion {
		return false, nil
	}

	cmName := instance.Name + "-resourceservers"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"resourceservers.yaml": rsContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create resource servers ConfigMap: %w", err)
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
			latest.Data["resourceservers.yaml"] = rsContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update resource servers ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/resourceservers-version"] = rsVersion
	return true, r.Patch(ctx, instance, patch)
}

// buildResourceServerYAML generates a YAML document for a single resource server
// in the format ThunderID's declarative resource loader expects.
func (r *ResourceServerReconciler) buildResourceServerYAML(ctx context.Context, s appsv1alpha1.ResourceServer) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("resource_type: resource_server\n")
	fmt.Fprintf(&buf, "id: %q\n", string(s.UID))
	fmt.Fprintf(&buf, "name: %s\n", yamlSingleQuote(s.Spec.Name))
	if s.Spec.Description != "" {
		fmt.Fprintf(&buf, "description: %s\n", yamlSingleQuote(s.Spec.Description))
	}
	if s.Spec.Handle != "" {
		fmt.Fprintf(&buf, "handle: %s\n", yamlSingleQuote(s.Spec.Handle))
	}
	if s.Spec.Identifier != "" {
		fmt.Fprintf(&buf, "identifier: %s\n", yamlSingleQuote(s.Spec.Identifier))
	}

	if s.Spec.OUID != "" {
		fmt.Fprintf(&buf, "ouId: %q\n", s.Spec.OUID)
	} else if s.Spec.OUHandle != "" {
		if uid := resolveOUHandle(ctx, r.Client, s.Namespace, s.Spec.OUHandle); uid != "" {
			fmt.Fprintf(&buf, "ouId: %q\n", uid)
		} else {
			fmt.Fprintf(&buf, "ouHandle: %s\n", yamlSingleQuote(s.Spec.OUHandle))
		}
	}

	if s.Spec.Delimiter != "" {
		fmt.Fprintf(&buf, "delimiter: %s\n", yamlSingleQuote(s.Spec.Delimiter))
	}

	if len(s.Spec.Resources) > 0 {
		buf.WriteString("resources:\n")
		for _, res := range s.Spec.Resources {
			fmt.Fprintf(&buf, "  - name: %s\n", yamlSingleQuote(res.Name))
			fmt.Fprintf(&buf, "    handle: %s\n", yamlSingleQuote(res.Handle))
			if res.Description != "" {
				fmt.Fprintf(&buf, "    description: %s\n", yamlSingleQuote(res.Description))
			}
			if res.Parent != "" {
				fmt.Fprintf(&buf, "    parent: %s\n", yamlSingleQuote(res.Parent))
			}
			if len(res.Actions) > 0 {
				buf.WriteString("    actions:\n")
				for _, a := range res.Actions {
					fmt.Fprintf(&buf, "      - name: %s\n", yamlSingleQuote(a.Name))
					fmt.Fprintf(&buf, "        handle: %s\n", yamlSingleQuote(a.Handle))
					if a.Description != "" {
						fmt.Fprintf(&buf, "        description: %s\n", yamlSingleQuote(a.Description))
					}
				}
			}
		}
	}

	return buf.String(), nil
}

func (r *ResourceServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.ResourceServer{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.resourceServersForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.resourceServersForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.OrganizationUnit{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexResourceServerOUHandle, func() client.ObjectList { return &appsv1alpha1.ResourceServerList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.OrganizationUnit).Spec.Handle }))).
		Named("resourceserver").
		Complete(r)
}

// resourceServersForDeployment maps a Deployment change to ResourceServers that reference it.
// For Phase=Error resource servers: always re-trigger (pod may have recovered).
// For Phase=Ready resource servers: re-trigger only when the Deployment is unhealthy.
func (r *ResourceServerReconciler) resourceServersForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	rsList := &appsv1alpha1.ResourceServerList{}
	if err := r.List(ctx, rsList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, rs := range rsList.Items {
		if rs.Spec.InstanceRef != dep.GetName() {
			continue
		}
		switch rs.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *ResourceServerReconciler) resourceServersForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	rsList := &appsv1alpha1.ResourceServerList{}
	if err := r.List(ctx, rsList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, rs := range rsList.Items {
		if rs.Spec.InstanceRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace},
			})
		}
	}
	return reqs
}

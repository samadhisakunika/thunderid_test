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
	"gopkg.in/yaml.v3"
)

const flowFinalizer = "thunderid.io/flow-cleanup"

// FlowReconciler reconciles a Flow object
type FlowReconciler struct {
	client.Client                      // let it read/write kubernetes objects
	Scheme        *runtime.Scheme      // how to convert between go type and kubrnetes api types
	Recorder      record.EventRecorder // emit visible events to kubernetes for users to see
	KubeClient    kubernetes.Interface // lower level client used specifically to fetch pod logs
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=flows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=flows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *FlowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	flow := &appsv1alpha1.Flow{}
	if err := r.Get(ctx, req.NamespacedName, flow); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: flow.Namespace, Name: flow.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", flow.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", flow.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	if !flow.DeletionTimestamp.IsZero() {
		if containsString(flow.Finalizers, flowFinalizer) {
			if _, err := r.rebuildFlowsConfigMap(ctx, instance, string(flow.UID)); err != nil {
				return ctrl.Result{}, err
			}
			flow.Finalizers = removeString(flow.Finalizers, flowFinalizer)
			if err := r.Update(ctx, flow); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(flow.Finalizers, flowFinalizer) {
		flow.Finalizers = append(flow.Finalizers, flowFinalizer)
		if err := r.Update(ctx, flow); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	changed, err := r.rebuildFlowsConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync flows ConfigMap")
		r.Recorder.Event(flow, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "FlowSyncFailed", fmt.Sprintf("Flow %s: %v", flow.Name, err))
		flow.Status.Phase = "Error"
		flow.Status.Synced = false
		_ = r.Status().Update(ctx, flow)
		return ctrl.Result{}, err
	}
	// Phase=="Pending" means we already synced and are waiting to check pod health.
	// Use LastSyncTime to ignore the immediate reconcile triggered by our own status update.
	if flow.Status.Phase == "Pending" && !changed {
		if flow.Status.LastSyncTime != nil {
			elapsed := time.Since(flow.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, flow, instance)
	}

	// Phase=="Error": re-check health only when driven by a Deployment watch event.
	// That reconcile is triggered by the Deployment becoming healthy, not by polling.
	if flow.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, flow, instance)
	}

	if !changed && flow.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, flow, instance)
	}

	// Content changed (or first sync) — record the time and wait for ThunderID to reload.
	now := metav1.Now()
	r.Recorder.Event(flow, corev1.EventTypeNormal, "Synced", "Flow synced to ConfigMap, waiting for ThunderID to apply")
	flow.Status.Phase = "Pending"
	flow.Status.Synced = false
	flow.Status.LastSyncTime = &now
	flow.Status.FlowID = deterministicUUIDv7(flow.ObjectMeta)
	if err := r.Status().Update(ctx, flow); err != nil {
		log.Error(err, "failed to update flow status")
		return ctrl.Result{}, err
	}

	log.Info("flow synced; requeuing in 30s to verify ThunderID health", "name", flow.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *FlowReconciler) checkThunderIDHealth(ctx context.Context, flow *appsv1alpha1.Flow, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if flow.Status.Phase != "Error" {
					flow.Status.Phase = "Error"
					flow.Status.Synced = false
					_ = r.Status().Update(ctx, flow)
				}
				if !inCrashLoop {
					// Container logs not yet flushed by containerd on first crash.
					// Requeue in 10s when the pod reaches stable CrashLoopBackOff.
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				// Pod is in stable CrashLoopBackOff — logs are reliably available now.
				r.fetchAndEmitPodErrors(ctx, flow, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	// Pod is healthy — only update status when transitioning out of a non-Ready state
	// to avoid a status-update → reconcile → status-update loop.
	if flow.Status.Phase != "Ready" || !flow.Status.Synced {
		r.Recorder.Event(flow, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; flow is active")
		flow.Status.Phase = "Ready"
		flow.Status.Synced = true
		if err := r.Status().Update(ctx, flow); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, flow ready", "name", flow.Name)
	}
	return ctrl.Result{}, nil
}

func (r *FlowReconciler) fetchAndEmitPodErrors(ctx context.Context, flow *appsv1alpha1.Flow, namespace, podName string) {
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
		r.Recorder.Event(flow, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc) // read the logs
	if err != nil {
		r.Recorder.Event(flow, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(flow, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(flow, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildFlowsConfigMap returns true if the ConfigMap content changed. It lists
// all flows for the instance, generates YAML for each, writes them to the
// <instanceName>-flows ConfigMap, and annotates the ThunderIDInstance so its
// reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific flow (used when that flow is being deleted).
func (r *FlowReconciler) rebuildFlowsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	log := log.FromContext(ctx)

	flowList := &appsv1alpha1.FlowList{}
	if err := r.List(ctx, flowList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	// List() order is not guaranteed stable across calls; sort so the built content (and its hash) doesn't change unless a flow actually did.
	sort.Slice(flowList.Items, func(i, j int) bool {
		return flowList.Items[i].Name < flowList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, f := range flowList.Items {
		if f.Labels[appsv1alpha1.InstanceRefLabel] != instance.Name {
			continue
		}
		if excludeUID != "" && string(f.UID) == excludeUID {
			continue
		}
		if !f.DeletionTimestamp.IsZero() {
			continue
		}
		if len(f.Spec.Nodes) == 0 {
			continue // Skip incomplete flows without nodes
		}

		flowYAML, err := buildFlowYAML(f)
		if err != nil {
			log.Error(err, "failed to generate flow YAML", "flow", f.Name)
			r.Recorder.Event(&f, corev1.EventTypeWarning, "YAMLBuildFailed", err.Error())
			return false, err
		}

		buf.WriteString("---\n")
		buf.WriteString(flowYAML)
	}
	flowsContent := buf.String()

	h := sha256.New()
	h.Write([]byte(flowsContent))
	flowsVersion := hex.EncodeToString(h.Sum(nil))[:16]

	// Idempotency guard: if the annotation already reflects the current hash the
	// ConfigMap is up-to-date (avoids a false "changed" on the informer-cache
	// reconcile that fires immediately after we patch the annotation below).
	if instance.Annotations["thunderid.io/flows-version"] == flowsVersion {
		return false, nil
	}

	cmName := instance.Name + "-flows"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"flows.yaml": flowsContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return false, fmt.Errorf("failed to create flows ConfigMap: %w", err)
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
			latest.Data["flows.yaml"] = flowsContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update flows ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/flows-version"] = flowsVersion
	return true, r.Patch(ctx, instance, patch)
}

// Struct to represent the bootstrap JSON/YAML payload for ThunderID
type thunderIDFlowPayload struct {
	ResourceType string        `yaml:"resource_type"`
	ID           string        `yaml:"id"`
	Handle       string        `yaml:"handle"`
	Name         string        `yaml:"name"`
	FlowType     string        `yaml:"flowType"`
	Nodes        []interface{} `yaml:"nodes,omitempty"`
}

// buildFlowYAML generates a YAML document for a single flow in bootstrap format
func buildFlowYAML(f appsv1alpha1.Flow) (string, error) {
	var parsedNodes []interface{}
	for _, rawNode := range f.Spec.Nodes {
		var nodeObj interface{}
		if err := json.Unmarshal(rawNode.Raw, &nodeObj); err != nil {
			return "", fmt.Errorf("failed to parse node raw JSON for flow %s: %w", f.Name, err)
		}
		parsedNodes = append(parsedNodes, nodeObj)
	}

	payload := thunderIDFlowPayload{
		ResourceType: "flow",
		ID:           deterministicUUIDv7(f.ObjectMeta),
		Handle:       f.Spec.Handle,
		Name:         f.Spec.Name,
		FlowType:     f.Spec.FlowType,
		Nodes:        parsedNodes,
	}

	out, err := yaml.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal flow payload to yaml: %w", err)
	}

	return string(out), nil
}

func (r *FlowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Flow{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.flowsForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.flowsForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("flow").
		Complete(r)
}

// flowsForDeployment maps a Deployment change to Flows that reference it.
// For Phase=Error flows: always re-trigger (pod may have recovered).
// For Phase=Ready flows: re-trigger only when the Deployment is unhealthy (new pod may be crashing).
func (r *FlowReconciler) flowsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	flowList := &appsv1alpha1.FlowList{}
	if err := r.List(ctx, flowList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, flow := range flowList.Items {
		if flow.Labels[appsv1alpha1.InstanceRefLabel] != dep.GetName() {
			continue
		}
		switch flow.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: flow.Name, Namespace: flow.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: flow.Name, Namespace: flow.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *FlowReconciler) flowsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	flowList := &appsv1alpha1.FlowList{}
	if err := r.List(ctx, flowList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, flow := range flowList.Items {
		if flow.Labels[appsv1alpha1.InstanceRefLabel] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: flow.Name, Namespace: flow.Namespace},
			})
		}
	}
	return reqs
}

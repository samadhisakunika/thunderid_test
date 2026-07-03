package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const flowFinalizer = "thunderid.io/flow-cleanup"

type FlowReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=flows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=flows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch

func (r *FlowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	flow := &appsv1alpha1.Flow{}
	if err := r.Get(ctx, req.NamespacedName, flow); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: flow.Namespace, Name: flow.Spec.InstanceRef}, instance); err != nil {
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", flow.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !flow.DeletionTimestamp.IsZero() {
		if containsString(flow.Finalizers, flowFinalizer) {
			if err := r.rebuildFlowsConfigMap(ctx, instance, string(flow.UID)); err != nil {
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

	if err := r.rebuildFlowsConfigMap(ctx, instance, ""); err != nil {
		log.Error(err, "failed to sync flows ConfigMap")
		flow.Status.Phase = "Error"
		flow.Status.Synced = false
		_ = r.Status().Update(ctx, flow)
		return ctrl.Result{}, err
	}

	flow.Status.Phase = "Ready"
	flow.Status.Synced = true
	flow.Status.FlowID = string(flow.UID)
	if err := r.Status().Update(ctx, flow); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("flow synced to ConfigMap", "name", flow.Name, "instance", instance.Name)
	return ctrl.Result{}, nil
}

func (r *FlowReconciler) rebuildFlowsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) error {
	flowList := &appsv1alpha1.FlowList{}
	if err := r.List(ctx, flowList, client.InNamespace(instance.Namespace)); err != nil {
		return err
	}

	var buf bytes.Buffer
	for _, f := range flowList.Items {
		if f.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(f.UID) == excludeUID {
			continue
		}
		if !f.DeletionTimestamp.IsZero() {
			continue
		}
		if f.Spec.Nodes == "" {
			continue
		}
		buf.WriteString("---\n")
		buf.WriteString(buildFlowYAML(f))
	}
	flowsContent := buf.String()

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
			return fmt.Errorf("failed to create flows ConfigMap: %w", err)
		}
	} else if err != nil {
		return err
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["flows.yaml"] = flowsContent
		if err := r.Update(ctx, cm); err != nil {
			return fmt.Errorf("failed to update flows ConfigMap: %w", err)
		}
	}

	h := sha256.New()
	h.Write([]byte(flowsContent))
	flowsVersion := hex.EncodeToString(h.Sum(nil))[:16]

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/flows-version"] = flowsVersion
	return r.Patch(ctx, instance, patch)
}

func buildFlowYAML(f appsv1alpha1.Flow) string {
	var buf bytes.Buffer
	buf.WriteString("# resource_type: flow\n")
	fmt.Fprintf(&buf, "id: %q\n", string(f.UID))
	fmt.Fprintf(&buf, "handle: %q\n", f.Spec.Handle)
	fmt.Fprintf(&buf, "name: %q\n", f.Spec.Name)
	fmt.Fprintf(&buf, "flowType: %q\n", f.Spec.FlowType)
	if f.Spec.Nodes != "" {
		fmt.Fprintf(&buf, "nodes: %s\n", f.Spec.Nodes)
	}
	return buf.String()
}

func (r *FlowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Flow{}).
		Named("flow").
		Complete(r)
}

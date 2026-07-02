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

const applicationFinalizer = "thunderid.io/application-cleanup"

type ApplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=flows,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=themes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	app := &appsv1alpha1.Application{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: app.Spec.InstanceRef}, instance); err != nil {
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", app.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	if !app.DeletionTimestamp.IsZero() {
		if containsString(app.Finalizers, applicationFinalizer) {
			if err := r.rebuildApplicationsConfigMap(ctx, instance, string(app.UID)); err != nil {
				return ctrl.Result{}, err
			}
			app.Finalizers = removeString(app.Finalizers, applicationFinalizer)
			if err := r.Update(ctx, app); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !containsString(app.Finalizers, applicationFinalizer) {
		app.Finalizers = append(app.Finalizers, applicationFinalizer)
		if err := r.Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.rebuildApplicationsConfigMap(ctx, instance, ""); err != nil {
		log.Error(err, "failed to sync application ConfigMap")
		app.Status.Phase = "Error"
		app.Status.Synced = false
		_ = r.Status().Update(ctx, app)
		return ctrl.Result{}, err
	}

	app.Status.Phase = "Ready"
	app.Status.Synced = true
	if err := r.Status().Update(ctx, app); err != nil {
		log.Error(err, "failed to update application status")
		return ctrl.Result{}, err
	}

	log.Info("application synced to ConfigMap", "name", app.Name, "instance", instance.Name)
	return ctrl.Result{}, nil

}

func (r *ApplicationReconciler) rebuildApplicationsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) error {
	appList := &appsv1alpha1.ApplicationList{}
	if err := r.List(ctx, appList, client.InNamespace(instance.Namespace)); err != nil {
		return err
	}

	var buf bytes.Buffer
	for _, a := range appList.Items {
		if a.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(a.UID) == excludeUID {
			continue
		}
		if !a.DeletionTimestamp.IsZero() {
			continue
		}
		yamlDoc, err := r.buildApplicationYAML(ctx, a)
		if err != nil {
			return fmt.Errorf("failed to build YAML for application %s: %w", a.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	appsContent := buf.String()

	cmName := instance.Name + "-applications"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"applications.yaml": appsContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return fmt.Errorf("failed to create applications ConfigMap: %w", err)
		}
	} else if err != nil {
		return err
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["applications.yaml"] = appsContent
		if err := r.Update(ctx, cm); err != nil {
			return fmt.Errorf("failed to update applications ConfigMap: %w", err)
		}
	}

	h := sha256.New()
	h.Write([]byte(appsContent))
	appsVersion := hex.EncodeToString(h.Sum(nil))[:16]

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/applications-version"] = appsVersion
	return r.Patch(ctx, instance, patch)
}

func (r *ApplicationReconciler) buildApplicationYAML(ctx context.Context, a appsv1alpha1.Application) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("# resource_type: application\n")
	fmt.Fprintf(&buf, "id: %q\n", string(a.UID))
	fmt.Fprintf(&buf, "name: %q\n", a.Spec.Name)
	if a.Spec.Description != "" {
		fmt.Fprintf(&buf, "description: %q\n", a.Spec.Description)
	}
	if a.Spec.OUID != "" {
		fmt.Fprintf(&buf, "ouId: %q\n", a.Spec.OUID)
	} else if a.Spec.OUHandle != "" {
		fmt.Fprintf(&buf, "ouHandle: %q\n", a.Spec.OUHandle)
	}
	if a.Spec.URL != "" {
		fmt.Fprintf(&buf, "url: %q\n", a.Spec.URL)
	}

	if a.Spec.AuthFlowID != "" {
		fmt.Fprintf(&buf, "authFlowId: %q\n", a.Spec.AuthFlowID)
	} else if a.Spec.AuthFlowHandle != "" {
		fmt.Fprintf(&buf, "authFlowHandle: %q\n", a.Spec.AuthFlowHandle)
	} else if a.Spec.AuthFlowRef != "" {
		flow := &appsv1alpha1.Flow{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.AuthFlowRef}, flow); err != nil {
			return "", fmt.Errorf("authFlowRef %q not found: %w", a.Spec.AuthFlowRef, err)
		}
		fmt.Fprintf(&buf, "authFlowHandle: %q\n", flow.Spec.Handle)
	}
	if a.Spec.RegistrationFlowHandle != "" {
		fmt.Fprintf(&buf, "registrationFlowHandle: %q\n", a.Spec.RegistrationFlowHandle)
	} else if a.Spec.RegistrationFlowRef != "" {
		flow := &appsv1alpha1.Flow{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.RegistrationFlowRef}, flow); err != nil {
			return "", fmt.Errorf("registrationFlowRef %q not found: %w", a.Spec.RegistrationFlowRef, err)
		}
		fmt.Fprintf(&buf, "registrationFlowHandle: %q\n", flow.Spec.Handle)
	}
	if a.Spec.RecoveryFlowHandle != "" {
		fmt.Fprintf(&buf, "recoveryFlowHandle: %q\n", a.Spec.RecoveryFlowHandle)
	} else if a.Spec.RecoveryFlowRef != "" {
		flow := &appsv1alpha1.Flow{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.RecoveryFlowRef}, flow); err != nil {
			return "", fmt.Errorf("recoveryFlowRef %q not found: %w", a.Spec.RecoveryFlowRef, err)
		}
		fmt.Fprintf(&buf, "recoveryFlowHandle: %q\n", flow.Spec.Handle)
	}

	if a.Spec.ThemeID != "" {
		fmt.Fprintf(&buf, "themeId: %q\n", a.Spec.ThemeID)
	} else if a.Spec.ThemeRef != "" {
		theme := &appsv1alpha1.Theme{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ThemeRef}, theme); err != nil {
			return "", fmt.Errorf("themeRef %q not found: %w", a.Spec.ThemeRef, err)
		}
		fmt.Fprintf(&buf, "themeId: %q\n", string(theme.UID))
	}

	if a.Spec.InboundAuthConfig != "" {
		fmt.Fprintf(&buf, "inboundAuthConfig: %s\n", a.Spec.InboundAuthConfig)
	}

	return buf.String(), nil
}

func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Application{}).
		Named("application").
		Complete(r)
}

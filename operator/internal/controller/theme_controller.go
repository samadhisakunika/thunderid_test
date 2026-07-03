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

const themeFinalizer = "thunderid.io/theme-cleanup"

type ThemeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=themes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=themes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch

func (r *ThemeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	theme := &appsv1alpha1.Theme{}
	if err := r.Get(ctx, req.NamespacedName, theme); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: theme.Namespace, Name: theme.Spec.InstanceRef}, instance); err != nil {
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", theme.Spec.InstanceRef)
		return ctrl.Result{}, err
	}

	// Handle deletion: rebuild ConfigMap without this theme, then remove finalizer.
	if !theme.DeletionTimestamp.IsZero() {
		if containsString(theme.Finalizers, themeFinalizer) {
			if err := r.rebuildThemesConfigMap(ctx, instance, string(theme.UID)); err != nil {
				return ctrl.Result{}, err
			}
			theme.Finalizers = removeString(theme.Finalizers, themeFinalizer)
			if err := r.Update(ctx, theme); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present so we can clean up on deletion.
	// Return immediately after adding it — the Update triggers a new reconcile
	// with a fresh resourceVersion, avoiding a stale-object conflict on status update.
	if !containsString(theme.Finalizers, themeFinalizer) {
		theme.Finalizers = append(theme.Finalizers, themeFinalizer)
		if err := r.Update(ctx, theme); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.rebuildThemesConfigMap(ctx, instance, ""); err != nil {
		log.Error(err, "failed to sync themes ConfigMap")
		theme.Status.Phase = "Error"
		theme.Status.Synced = false
		_ = r.Status().Update(ctx, theme)
		return ctrl.Result{}, err
	}

	theme.Status.Phase = "Ready"
	theme.Status.Synced = true
	theme.Status.ThemeID = string(theme.UID)
	if err := r.Status().Update(ctx, theme); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("theme synced to ConfigMap", "name", theme.Name, "instance", instance.Name)
	return ctrl.Result{}, nil
}

// rebuildThemesConfigMap lists all themes for the instance, generates YAML for each,
// writes them to the <instanceName>-themes ConfigMap, and annotates the ThunderIDInstance
// so its reconciler detects the change and triggers a rolling restart.
// excludeUID skips a specific theme (used when that theme is being deleted).
func (r *ThemeReconciler) rebuildThemesConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) error {
	themeList := &appsv1alpha1.ThemeList{}
	if err := r.List(ctx, themeList, client.InNamespace(instance.Namespace)); err != nil {
		return err
	}

	var buf bytes.Buffer
	for _, t := range themeList.Items {
		if t.Spec.InstanceRef != instance.Name {
			continue
		}
		if excludeUID != "" && string(t.UID) == excludeUID {
			continue
		}
		if !t.DeletionTimestamp.IsZero() {
			continue
		}
		if t.Spec.Theme == "" {
			continue // ThunderID requires theme JSON; skip incomplete themes
		}
		buf.WriteString("---\n")
		buf.WriteString(buildThemeYAML(t))
	}
	themesContent := buf.String()

	// Create or update the <instanceName>-themes ConfigMap.
	cmName := instance.Name + "-themes"
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: cmName}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: instance.Namespace,
			},
			Data: map[string]string{"themes.yaml": themesContent},
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		if err := r.Create(ctx, cm); err != nil {
			return fmt.Errorf("failed to create themes ConfigMap: %w", err)
		}
	} else if err != nil {
		return err
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["themes.yaml"] = themesContent
		if err := r.Update(ctx, cm); err != nil {
			return fmt.Errorf("failed to update themes ConfigMap: %w", err)
		}
	}

	// Annotate ThunderIDInstance with a hash of the themes content so the
	// ThunderIDInstance reconciler detects the change and triggers a rolling restart.
	h := sha256.New()
	h.Write([]byte(themesContent))
	themesVersion := hex.EncodeToString(h.Sum(nil))[:16]

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/themes-version"] = themesVersion
	return r.Patch(ctx, instance, patch)
}

// buildThemeYAML generates a YAML document for a single theme in the format
// ThunderID's declarative resource loader expects.
func buildThemeYAML(t appsv1alpha1.Theme) string {
	var buf bytes.Buffer
	buf.WriteString("# resource_type: theme\n")
	fmt.Fprintf(&buf, "id: %q\n", string(t.UID))
	fmt.Fprintf(&buf, "handle: %q\n", t.Name)
	fmt.Fprintf(&buf, "displayName: %q\n", t.Spec.DisplayName)
	if t.Spec.Description != "" {
		fmt.Fprintf(&buf, "description: %q\n", t.Spec.Description)
	}
	if t.Spec.Theme != "" {
		// Block scalar preserves the JSON string without requiring special quoting.
		fmt.Fprintf(&buf, "theme: |\n  %s\n", t.Spec.Theme)
	}
	return buf.String()
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

func (r *ThemeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Theme{}).
		Named("theme").
		Complete(r)
}

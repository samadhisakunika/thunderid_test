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

const applicationFinalizer = "thunderid.io/application-cleanup"

type ApplicationReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=flows,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=themes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=usertypes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	app := &appsv1alpha1.Application{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: app.Labels[appsv1alpha1.InstanceRefLabel]}, instance); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ThunderIDInstance not found, waiting for it to be created", "instanceRef", app.Labels[appsv1alpha1.InstanceRefLabel])
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Error(err, "failed to get ThunderIDInstance", "instanceRef", app.Labels[appsv1alpha1.InstanceRefLabel])
		return ctrl.Result{}, err
	}

	if !app.DeletionTimestamp.IsZero() {
		if containsString(app.Finalizers, applicationFinalizer) {
			if _, err := r.rebuildApplicationsConfigMap(ctx, instance, string(app.UID)); err != nil {
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

	changed, err := r.rebuildApplicationsConfigMap(ctx, instance, "")
	if err != nil {
		log.Error(err, "failed to sync application ConfigMap")
		r.Recorder.Event(app, corev1.EventTypeWarning, "SyncFailed", err.Error())
		r.Recorder.Event(instance, corev1.EventTypeWarning, "ApplicationSyncFailed", fmt.Sprintf("Application %s: %v", app.Name, err))
		app.Status.Phase = "Error"
		app.Status.Synced = false
		_ = r.Status().Update(ctx, app)
		return ctrl.Result{}, err
	}
	// Phase=="Pending" means we already synced and are waiting to check pod health.
	// Use LastSyncTime to ignore the immediate reconcile triggered by our own status update.
	if app.Status.Phase == "Pending" && !changed {
		if app.Status.LastSyncTime != nil {
			elapsed := time.Since(app.Status.LastSyncTime.Time)
			if elapsed < 30*time.Second {
				return ctrl.Result{RequeueAfter: 30*time.Second - elapsed}, nil
			}
		}
		return r.checkThunderIDHealth(ctx, app, instance)
	}

	// Phase=="Error": re-check health only when driven by a Deployment watch event.
	// That reconcile is triggered by the Deployment becoming healthy, not by polling.
	if app.Status.Phase == "Error" && !changed {
		return r.checkThunderIDHealth(ctx, app, instance)
	}

	if !changed && app.Status.Phase == "Ready" {
		return r.checkThunderIDHealth(ctx, app, instance)
	}

	// Content changed (or first sync) — record the time and wait for ThunderID to reload.
	now := metav1.Now()
	r.Recorder.Event(app, corev1.EventTypeNormal, "Synced", "Application synced to ConfigMap, waiting for ThunderID to apply")
	app.Status.Phase = "Pending"
	app.Status.Synced = false
	app.Status.LastSyncTime = &now
	if err := r.Status().Update(ctx, app); err != nil {
		log.Error(err, "failed to update application status")
		return ctrl.Result{}, err
	}

	log.Info("application synced; requeuing in 30s to verify ThunderID health", "name", app.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *ApplicationReconciler) checkThunderIDHealth(ctx context.Context, app *appsv1alpha1.Application, instance *appsv1alpha1.ThunderIDInstance) (ctrl.Result, error) {
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
				if app.Status.Phase != "Error" {
					app.Status.Phase = "Error"
					app.Status.Synced = false
					_ = r.Status().Update(ctx, app)
				}
				if !inCrashLoop {
					// Container logs not yet flushed by containerd on first crash.
					// Requeue in 10s when the pod reaches stable CrashLoopBackOff.
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
				// Pod is in stable CrashLoopBackOff — logs are reliably available now.
				r.fetchAndEmitPodErrors(ctx, app, instance.Namespace, pod.Name)
				return ctrl.Result{}, nil
			}
		}
	}

	// Pod is healthy — only update status when transitioning out of a non-Ready state
	// to avoid a status-update → reconcile → status-update loop.
	if app.Status.Phase != "Ready" || !app.Status.Synced {
		r.Recorder.Event(app, corev1.EventTypeNormal, "Ready", "ThunderID is healthy; application is active")
		app.Status.Phase = "Ready"
		app.Status.Synced = true
		if err := r.Status().Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("ThunderID healthy, application ready", "name", app.Name)
	}
	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) fetchAndEmitPodErrors(ctx context.Context, app *appsv1alpha1.Application, namespace, podName string) {
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
		r.Recorder.Event(app, corev1.EventTypeWarning, "ThunderIDCrashed",
			fmt.Sprintf("pod %s is in CrashLoopBackOff (could not fetch logs: %v)", podName, err))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc) // read the logs
	if err != nil {
		r.Recorder.Event(app, corev1.EventTypeWarning, "ThunderIDCrashed",
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
			r.Recorder.Event(app, corev1.EventTypeWarning, "ThunderIDError", line)
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
			r.Recorder.Event(app, corev1.EventTypeWarning, "ThunderIDCrashed", line)
		}
	}
}

// rebuildApplicationsConfigMap returns true if the ConfigMap content changed.
func (r *ApplicationReconciler) rebuildApplicationsConfigMap(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, excludeUID string) (bool, error) {
	appList := &appsv1alpha1.ApplicationList{}
	if err := r.List(ctx, appList, client.InNamespace(instance.Namespace)); err != nil {
		return false, err
	}
	// List() order is not guaranteed stable across calls; sort so the built
	// content (and its hash) doesn't change unless an application actually did.
	sort.Slice(appList.Items, func(i, j int) bool {
		return appList.Items[i].Name < appList.Items[j].Name
	})

	var buf bytes.Buffer
	for _, a := range appList.Items {
		if a.Labels[appsv1alpha1.InstanceRefLabel] != instance.Name {
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
			return false, fmt.Errorf("failed to build YAML for application %s: %w", a.Name, err)
		}
		buf.WriteString("---\n")
		buf.WriteString(yamlDoc)
	}
	appsContent := buf.String()

	h := sha256.New()
	h.Write([]byte(appsContent))
	appsVersion := hex.EncodeToString(h.Sum(nil))[:16]

	// Idempotency guard: if the annotation already reflects the current hash the
	// ConfigMap is up-to-date (avoids a false "changed" on the informer-cache
	// reconcile that fires immediately after we patch the annotation below).
	if instance.Annotations["thunderid.io/applications-version"] == appsVersion {
		return false, nil
	}

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
			return false, fmt.Errorf("failed to create applications ConfigMap: %w", err)
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
			latest.Data["applications.yaml"] = appsContent
			return r.Update(ctx, latest)
		})
		if updateErr != nil {
			return false, fmt.Errorf("failed to update applications ConfigMap: %w", updateErr)
		}
	}

	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["thunderid.io/applications-version"] = appsVersion
	return true, r.Patch(ctx, instance, patch)
}

func (r *ApplicationReconciler) buildApplicationYAML(ctx context.Context, a appsv1alpha1.Application) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("resource_type: application\n")
	fmt.Fprintf(&buf, "id: %q\n", string(a.UID))
	fmt.Fprintf(&buf, "name: %q\n", a.Spec.Name)
	if a.Spec.Description != "" {
		fmt.Fprintf(&buf, "description: %q\n", a.Spec.Description)
	}
	if a.Spec.Template != "" {
		// ThunderID's own resources.yaml schema calls this field "type" (see
		// /opt/thunderid/bootstrap/01-default-resources.yaml in the image, e.g. the built-in
		// "Console" application: "type: browser") — not "template". The CRD field is kept named
		// spec.template for clarity to operator users (it selects one of browser/fullstack/mobile/
		// m2m/custom), but must be rendered under ThunderID's actual key or it's silently ignored
		// as an unrecognized field, then rejected at bootstrap as APP-1042 "Application type is
		// required".
		fmt.Fprintf(&buf, "type: %s\n", a.Spec.Template)
	}
	if a.Spec.OUID != "" {
		fmt.Fprintf(&buf, "ouId: %q\n", a.Spec.OUID)
	} else if a.Spec.OUHandle != "" {
		if uid := resolveOUHandle(ctx, r.Client, a.Namespace, a.Spec.OUHandle); uid != "" {
			fmt.Fprintf(&buf, "ouId: %q\n", uid)
		} else {
			fmt.Fprintf(&buf, "ouHandle: %q\n", a.Spec.OUHandle)
		}
	}
	if a.Spec.URL != "" {
		fmt.Fprintf(&buf, "url: %q\n", a.Spec.URL)
	}
	if a.Spec.LogoURL != "" {
		fmt.Fprintf(&buf, "logoUrl: %s\n", yamlSingleQuote(a.Spec.LogoURL))
	}

	if a.Spec.AuthFlowID != "" {
		fmt.Fprintf(&buf, "authFlowId: %q\n", a.Spec.AuthFlowID)
	} else if a.Spec.AuthFlowHandle != "" {
		fmt.Fprintf(&buf, "authFlowHandle: %q\n", a.Spec.AuthFlowHandle)
	}
	if a.Spec.RegistrationFlowID != "" {
		fmt.Fprintf(&buf, "registrationFlowId: %q\n", a.Spec.RegistrationFlowID)
	} else if a.Spec.RegistrationFlowHandle != "" {
		fmt.Fprintf(&buf, "registrationFlowHandle: %q\n", a.Spec.RegistrationFlowHandle)
	}
	fmt.Fprintf(&buf, "isRegistrationFlowEnabled: %v\n", a.Spec.IsRegistrationFlowEnabled)

	if a.Spec.RecoveryFlowHandle != "" {
		fmt.Fprintf(&buf, "recoveryFlowHandle: %q\n", a.Spec.RecoveryFlowHandle)
	}
	fmt.Fprintf(&buf, "isRecoveryFlowEnabled: %v\n", a.Spec.IsRecoveryFlowEnabled)
	fmt.Fprintf(&buf, "isSignOutFlowEnabled: %v\n", a.Spec.IsSignOutFlowEnabled)

	allowedUserTypes := append([]string{}, a.Spec.AllowedUserTypes...)
	if len(a.Spec.AllowedUserTypeIDs) > 0 {
		userTypeList := &appsv1alpha1.UserTypeList{}
		if err := r.List(ctx, userTypeList, client.InNamespace(a.Namespace)); err != nil {
			return "", err
		}
		names := make(map[string]string, len(userTypeList.Items))
		for _, ut := range userTypeList.Items {
			names[string(ut.UID)] = ut.Spec.Name
		}
		for _, id := range a.Spec.AllowedUserTypeIDs {
			name, ok := names[id]
			if !ok {
				return "", fmt.Errorf("allowedUserTypeIds: no UserType with id %q found", id)
			}
			allowedUserTypes = append(allowedUserTypes, name)
		}
	}
	if len(allowedUserTypes) > 0 {
		buf.WriteString("allowedUserTypes:\n")
		for _, t := range allowedUserTypes {
			fmt.Fprintf(&buf, "  - %s\n", t)
		}
	}

	if a.Spec.Assertion != nil {
		fmt.Fprintf(&buf, "assertion:\n  validityPeriod: %d\n", a.Spec.Assertion.ValidityPeriod)

	}

	if a.Spec.LoginConsent != nil {
		fmt.Fprintf(&buf, "loginConsent:\n  validityPeriod: %d\n", a.Spec.LoginConsent.ValidityPeriod)
	}

	if a.Spec.ThemeHandle != "" {
		fmt.Fprintf(&buf, "themeHandle: %q\n", a.Spec.ThemeHandle)
	} else if a.Spec.ThemeID != "" {
		fmt.Fprintf(&buf, "themeId: %q\n", a.Spec.ThemeID)
	}

	writeYAMLBlock(&buf, "inboundAuthConfig", a.Spec.InboundAuthConfig)

	return buf.String(), nil
}

func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Application{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.appsForDeployment), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.ThunderIDInstance{}, handler.EnqueueRequestsFromMapFunc(r.appsForInstance),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1alpha1.Flow{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexApplicationAuthFlowHandle, func() client.ObjectList { return &appsv1alpha1.ApplicationList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.Flow).Spec.Handle }))).
		Watches(&appsv1alpha1.Flow{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexApplicationRegistrationFlowHandle, func() client.ObjectList { return &appsv1alpha1.ApplicationList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.Flow).Spec.Handle }))).
		Watches(&appsv1alpha1.Flow{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexApplicationRecoveryFlowHandle, func() client.ObjectList { return &appsv1alpha1.ApplicationList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.Flow).Spec.Handle }))).
		Watches(&appsv1alpha1.Theme{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexApplicationThemeHandle, func() client.ObjectList { return &appsv1alpha1.ApplicationList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.Theme).Spec.Handle }))).
		Watches(&appsv1alpha1.OrganizationUnit{}, handler.EnqueueRequestsFromMapFunc(
			enqueueReferencing(mgr.GetClient(), indexApplicationOUHandle, func() client.ObjectList { return &appsv1alpha1.ApplicationList{} },
				func(o client.Object) string { return o.(*appsv1alpha1.OrganizationUnit).Spec.Handle }))).
		Named("application").
		Complete(r)
}

// appsForDeployment maps a Deployment change to Applications that reference it.
// For Phase=Error apps: always re-trigger (pod may have recovered).
// For Phase=Ready apps: re-trigger only when the Deployment is unhealthy (new pod may be crashing).
func (r *ApplicationReconciler) appsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	appList := &appsv1alpha1.ApplicationList{}
	if err := r.List(ctx, appList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, app := range appList.Items {
		if app.Labels[appsv1alpha1.InstanceRefLabel] != dep.GetName() {
			continue
		}
		switch app.Status.Phase {
		case "Error":
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
			})
		case "Ready", "Pending":
			if dep.Status.UnavailableReplicas > 0 {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
				})
			}
		}
	}
	return reqs
}

func (r *ApplicationReconciler) appsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	appList := &appsv1alpha1.ApplicationList{}
	if err := r.List(ctx, appList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, app := range appList.Items {
		if app.Labels[appsv1alpha1.InstanceRefLabel] == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
			})
		}
	}
	return reqs
}

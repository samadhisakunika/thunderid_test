/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256" //compute the config checksum
	_ "embed"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

//go:embed deployment-config.yaml
var deploymentConfigTemplate string

// ThunderIDInstanceReconciler reconciles a ThunderIDInstance object
type ThunderIDInstanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// RBAC annotations for the controller
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;services;secrets;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

func (r *ThunderIDInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	// Fetch the ThunderIDInstance from kubernetes
	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	publicURL := "https://" + instance.Spec.Domain

	resourcesContent, err := r.reconcileConfigMap(ctx, instance, publicURL)
	if err != nil {
		log.Error(err, "failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	if err := r.reconcileHPA(ctx, instance); err != nil {
		log.Error(err, "failed to reconcile HPA")
		return ctrl.Result{}, err
	}
	// Hash includes every resource-kind's version annotation so pod rolls when any of them changes.
	configHash := func() string {
		h := sha256.New()
		h.Write([]byte(publicURL + instance.Spec.ResourcesConfigMap +
			instance.Annotations["thunderid.io/serverconfigs-version"] +
			instance.Annotations["thunderid.io/organizationunits-version"] +
			instance.Annotations["thunderid.io/resourceservers-version"] +
			instance.Annotations["thunderid.io/themes-version"] +
			instance.Annotations["thunderid.io/flows-version"] +
			instance.Annotations["thunderid.io/usertypes-version"] +
			instance.Annotations["thunderid.io/users-version"] +
			instance.Annotations["thunderid.io/groups-version"] +
			instance.Annotations["thunderid.io/roles-version"] +
			instance.Annotations["thunderid.io/applications-version"] +
			instance.Annotations["thunderid.io/translations-version"]))
		return hex.EncodeToString(h.Sum(nil))[:16]
	}()

	securitySecretName, err := r.reconcileSecuritySecret(ctx, instance)
	if err != nil {
		log.Error(err, "failed to reconcile security Secret")
		r.Recorder.Event(instance, corev1.EventTypeWarning, "SecuritySecretError", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.reconcileDataVolume(ctx, instance); err != nil {
		log.Error(err, "failed to reconcile data PersistentVolumeClaim")
		r.Recorder.Event(instance, corev1.EventTypeWarning, "DataVolumeError", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.reconcileDeployment(ctx, instance, configHash, resourcesContent != "", securitySecretName); err != nil {
		log.Error(err, "failed to reconcile Deployment")
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, instance); err != nil {
		log.Error(err, "failed to reconcile Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileIngress(ctx, instance); err != nil {
		log.Error(err, "failed to reconcile Ingress")
		return ctrl.Result{}, err
	}

	// Re-fetch before status update — annotations may have been patched by sub-controllers.
	// Skip the write if status is already correct to avoid triggering a reconcile loop.
	statusErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &appsv1alpha1.ThunderIDInstance{}
		if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
			return err
		}
		if latest.Status.URL == publicURL && latest.Status.Phase == "Running" {
			return nil
		}
		latest.Status.URL = publicURL
		latest.Status.Phase = "Running"
		return r.Status().Update(ctx, latest)
	})
	if statusErr != nil {
		log.Error(statusErr, "failed to update status")
		return ctrl.Result{}, statusErr
	}

	log.Info("reconciled ThunderIDInstance", "url", publicURL)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ThunderIDInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.ThunderIDInstance{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Service{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&networkingv1.Ingress{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		//Owns(&corev1.ConfigMap{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
		// Only react to the instance's own config ConfigMap, not sub-controller CMs.
		// Sub-controller CM changes are signalled via ThunderIDInstance annotation patches,
		// which AnnotationChangedPredicate on For() already catches.
		//	return strings.HasSuffix(obj.GetName(), "-config")
		//}))).
		Named("thunderidinstance").
		Complete(r)
}
func (r *ThunderIDInstanceReconciler) reconcileConfigMap(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, publicURL string) (string, error) {

	configContent := fmt.Sprintf(deploymentConfigTemplate, publicURL, publicURL, publicURL)

	// Load base resources from external ConfigMap.
	var resourcesContent string
	if instance.Spec.ResourcesConfigMap != "" {
		externalCM := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      instance.Spec.ResourcesConfigMap,
			Namespace: instance.Namespace,
		}, externalCM); err != nil {
			return "", err
		}
		resourcesContent = externalCM.Data["resources.yaml"]
	}

	// Append custom resources from each sub-controller's ConfigMap into the same
	// resources.yaml so ThunderID loads them in a single pass on startup.
	for _, res := range []struct{ suffix, key string }{
		{"-organizationunits", "organizationunits.yaml"},
		{"-usertypes", "usertypes.yaml"},
		{"-resourceservers", "resourceservers.yaml"},
		{"-roles", "roles.yaml"},
		{"-groups", "groups.yaml"},
		{"-users", "users.yaml"},
		{"-flows", "flows.yaml"},
		{"-themes", "themes.yaml"},
		{"-applications", "applications.yaml"},
		{"-translations", "translations.yaml"},
		{"-serverconfigs", "server_configs.yaml"},
	} {
		appended, err := r.appendResourceConfigMap(ctx, instance, res.suffix, res.key)
		if err != nil {
			return "", err
		}
		resourcesContent += appended
	}

	data := map[string]string{
		"deployment.yaml": configContent,
	}
	if resourcesContent != "" {
		data["resources.yaml"] = resourcesContent
	}

	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name + "-config", Namespace: instance.Namespace}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Name + "-config",
				Namespace: instance.Namespace,
			},
			Data: data,
		}
		_ = ctrl.SetControllerReference(instance, cm, r.Scheme)
		return resourcesContent, r.Create(ctx, cm)
	}
	if err != nil {
		return "", err
	}
	// Skip update if content is unchanged to avoid triggering unnecessary reconciles.
	if cm.Data["deployment.yaml"] == data["deployment.yaml"] && cm.Data["resources.yaml"] == data["resources.yaml"] {
		return resourcesContent, nil
	}
	cm.Data = data
	return resourcesContent, r.Update(ctx, cm)
}

func (r *ThunderIDInstanceReconciler) appendResourceConfigMap(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, suffix, key string) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      instance.Name + suffix,
		Namespace: instance.Namespace,
	}, cm); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if content := cm.Data[key]; content != "" {
		return "\n" + content, nil
	}
	return "", nil
}

// reconcileDataVolume ensures the PersistentVolumeClaim backing setup.sh's once-only marker
// (see reconcileDeployment) exists when spec.dataVolumeSize is set. The claim's storage
// request is not reconciled after creation — resizing an existing instance's volume is left
// to the user (edit the PVC directly, subject to what the StorageClass allows).
func (r *ThunderIDInstanceReconciler) reconcileDataVolume(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) error {
	if instance.Spec.DataVolumeSize == "" {
		return nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name + "-data", Namespace: instance.Namespace}, pvc)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	qty, err := resource.ParseQuantity(instance.Spec.DataVolumeSize)
	if err != nil {
		return fmt.Errorf("invalid spec.dataVolumeSize %q: %w", instance.Spec.DataVolumeSize, err)
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name + "-data",
			Namespace: instance.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	_ = ctrl.SetControllerReference(instance, pvc, r.Scheme)
	return r.Create(ctx, pvc)
}

func (r *ThunderIDInstanceReconciler) reconcileDeployment(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, configHash string, hasResources bool,
	securitySecretName string) error {
	replicas := instance.Spec.Replicas
	configMapName := instance.Name + "-config"

	hasDataVolume := instance.Spec.DataVolumeSize != ""

	// The thunderid image runs as a non-root user (uid/gid 10001, see the image's Dockerfile).
	// A freshly provisioned PVC is mounted root-owned, so without fsGroup the container can't
	// write its "setup ran once" marker to it — setup.sh succeeds but the following touch fails
	// with "Permission denied", and the container exits non-zero and crash-loops. ConfigMap/Secret/
	// emptyDir volumes don't need this: kubelet mounts those world-writable by default.
	var podSecurityContext *corev1.PodSecurityContext
	if hasDataVolume {
		thunderidGID := int64(10001)
		podSecurityContext = &corev1.PodSecurityContext{FSGroup: &thunderidGID}
	}

	// setupCmd normally reruns setup.sh (and its SQLite reinitialization) on every container
	// start. With a persistent data volume, repository/database survives restarts, so setup.sh
	// only needs to run once: gate it behind a marker file written on that same volume.
	setupCmd := "./setup.sh"
	if hasDataVolume {
		setupCmd = "[ -f repository/database/.setup-complete ] || { ./setup.sh && touch repository/database/.setup-complete; }"
	}
	startCmd := fmt.Sprintf("cd /opt/thunderid && %s && exec ./start.sh", setupCmd)
	if hasResources {
		startCmd = fmt.Sprintf("cd /opt/thunderid && %s && exec ./start.sh /opt/thunderid/resources.yaml", setupCmd)
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "config",
			MountPath: "/opt/thunderid/repository/conf/deployment.yaml",
			SubPath:   "deployment.yaml",
		},
	}
	if hasResources {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "config",
			MountPath: "/opt/thunderid/resources.yaml",
			SubPath:   "resources.yaml",
		})
	}

	// Mounted individually (not as a whole directory) onto /opt/thunderid/config/{certs,secrets}
	// so we don't clobber the image's other baked-in files under /opt/thunderid/config (e.g.
	// default.json, resources/templates/*). setup.sh only (re)generates a cert/key pair when it
	// finds the file missing, so pre-supplying all of them here — stable across reconciles, see
	// reconcileSecuritySecret — keeps the JWT signing key (and TLS/encryption keys) identical
	// across pod restarts and across replicas.
	for _, f := range securityCertMountKeys {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "security",
			MountPath: "/opt/thunderid/config/certs/" + f,
			SubPath:   f,
		})
	}
	// direct_auth_secret is handled separately from the cert/key files above: setup.sh
	// unconditionally chmod's it (600) even when reusing an existing value, and Secret-backed
	// volumes are always mounted read-only, so a direct subPath mount here would crash setup.sh
	// with "Read-only file system". Instead it's seeded into a writable emptyDir by an init
	// container, which is where the chmod actually happens.
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "direct-auth-secret",
		MountPath: "/opt/thunderid/config/secrets",
	})
	if hasDataVolume {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "data",
			MountPath: "/opt/thunderid/repository/database",
		})
	}

	defaultMode := int32(0644)
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					DefaultMode:          &defaultMode,
				},
			},
		},
		{
			Name: "security",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  securitySecretName,
					DefaultMode: &defaultMode,
				},
			},
		},
		{
			Name:         "direct-auth-secret",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	if hasDataVolume {
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: instance.Name + "-data",
				},
			},
		})
	}

	initContainers := []corev1.Container{
		{
			Name:  "seed-direct-auth-secret",
			Image: instance.Spec.Image,
			Command: []string{"/bin/sh", "-c", fmt.Sprintf(
				"cp /run/security-seed/%[1]s /opt/thunderid/config/secrets/%[1]s && chmod 600 /opt/thunderid/config/secrets/%[1]s",
				securityKeyDirectAuth)},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "security", MountPath: "/run/security-seed", ReadOnly: true},
				{Name: "direct-auth-secret", MountPath: "/opt/thunderid/config/secrets"},
			},
		},
	}
	if hasDataVolume {
		// fsGroup (set below on the pod's SecurityContext) isn't honored by every volume
		// backend — notably plain hostPath PVs, which is what minikube's default storage
		// class provisions. Chown explicitly, as root, so the non-root "thunderid" container
		// can write its setup-complete marker regardless of what's backing the PVC.
		root := int64(0)
		initContainers = append(initContainers, corev1.Container{
			Name:            "fix-data-volume-permissions",
			Image:           instance.Spec.Image,
			Command:         []string{"chown", "-R", "10001:10001", "/opt/thunderid/repository/database"},
			SecurityContext: &corev1.SecurityContext{RunAsUser: &root},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/opt/thunderid/repository/database"},
			},
		})
	}

	// The data PVC is ReadWriteOnce, so a surging RollingUpdate (the default) would strand the
	// new pod Pending trying to mount a volume the old pod still holds. Recreate tears the old
	// pod down first.
	strategy := appsv1.DeploymentStrategy{}
	if hasDataVolume {
		strategy.Type = appsv1.RecreateDeploymentStrategyType
	}

	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, dep)
	if errors.IsNotFound(err) {
		if replicas == 0 {
			// Always start at 1, even with autoscaling on — creating with 0 replicas
			// makes the HPA treat the Deployment as intentionally paused and it will
			// never scale it up. The HPA takes over cleanly from 1 replica onward.
			replicas = 1
		}
		dep = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Name,
				Namespace: instance.Namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Strategy: strategy,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": instance.Name},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": instance.Name},
						Annotations: map[string]string{"thunderid.io/config-hash": configHash},
					},
					Spec: corev1.PodSpec{
						SecurityContext: podSecurityContext,
						InitContainers:  initContainers,
						Containers: []corev1.Container{
							{
								Name:    "thunderid",
								Image:   instance.Spec.Image,
								Command: []string{"/bin/bash", "-c", startCmd},
								Env: []corev1.EnvVar{
									{Name: "ADMIN_USERNAME", Value: "admin"},
									{Name: "ADMIN_PASSWORD", Value: "admin"},
								},
								Ports:        []corev1.ContainerPort{{ContainerPort: 8090, Protocol: corev1.ProtocolTCP}},
								VolumeMounts: volumeMounts,
								Resources:    instance.Spec.Resources,
								LivenessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/health/liveness",
											Port:   intstr.FromInt(8090),
											Scheme: corev1.URISchemeHTTPS,
										},
									},
									InitialDelaySeconds: 1,
									PeriodSeconds:       3,
									FailureThreshold:    3,
									SuccessThreshold:    1,
									TimeoutSeconds:      1,
								},
								ReadinessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/health/readiness",
											Port:   intstr.FromInt(8090),
											Scheme: corev1.URISchemeHTTPS,
										},
									},
									InitialDelaySeconds: 1,
									PeriodSeconds:       2,
									FailureThreshold:    6,
									SuccessThreshold:    1,
									TimeoutSeconds:      1,
								},
							},
						},
						Volumes: volumes,
					},
				},
			},
		}
		_ = ctrl.SetControllerReference(instance, dep, r.Scheme)
		return r.Create(ctx, dep)
	}
	if err != nil {
		return err
	}

	// Snapshot before mutating — used for targeted diff below.
	originalHash := dep.Spec.Template.Annotations["thunderid.io/config-hash"]
	originalImage := dep.Spec.Template.Spec.Containers[0].Image
	originalCmd := ""
	if len(dep.Spec.Template.Spec.Containers[0].Command) == 3 {
		originalCmd = dep.Spec.Template.Spec.Containers[0].Command[2]
	}
	originalResources := dep.Spec.Template.Spec.Containers[0].Resources.DeepCopy()
	originalReplicas := dep.Spec.Replicas
	originalLivenessProbe := dep.Spec.Template.Spec.Containers[0].LivenessProbe.DeepCopy()
	originalReadinessProbe := dep.Spec.Template.Spec.Containers[0].ReadinessProbe.DeepCopy()
	originalSecurityContext := dep.Spec.Template.Spec.SecurityContext.DeepCopy()
	originalInitContainers := dep.Spec.Template.Spec.InitContainers

	// Check for stale bootstrap volume that the old code left behind, and whether the security
	// Secret volume (added later) already points at the Secret we want — deployments created
	// before it existed, or whose spec.securitySecret just changed, need an update to pick that
	// up even if nothing else about them changed.
	hasBootstrapVolume := false
	securityVolumeCurrent := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "bootstrap" {
			hasBootstrapVolume = true
		}
		if v.Name == "security" && v.Secret != nil && v.Secret.SecretName == securitySecretName {
			securityVolumeCurrent = true
		}
	}

	if instance.Spec.AutoScaling == nil && instance.Spec.Replicas > 0 {
		r := instance.Spec.Replicas
		dep.Spec.Replicas = &r
	}
	dep.Spec.Strategy = strategy

	dep.Spec.Template.Spec.Containers[0].Image = instance.Spec.Image
	dep.Spec.Template.Spec.Containers[0].Command = []string{"/bin/bash", "-c", startCmd}
	dep.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "ADMIN_USERNAME", Value: "admin"},
		{Name: "ADMIN_PASSWORD", Value: "admin"},
	}
	dep.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts
	dep.Spec.Template.Spec.Containers[0].Resources = instance.Spec.Resources
	dep.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/health/liveness",
				Port:   intstr.FromInt(8090),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: 1,
		PeriodSeconds:       3,
		FailureThreshold:    3,
		SuccessThreshold:    1,
		TimeoutSeconds:      1,
	}
	dep.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/health/readiness",
				Port:   intstr.FromInt(8090),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: 1,
		PeriodSeconds:       2,
		FailureThreshold:    6,
		SuccessThreshold:    1,
		TimeoutSeconds:      1,
	}
	dep.Spec.Template.Spec.Volumes = volumes
	dep.Spec.Template.Spec.InitContainers = initContainers
	dep.Spec.Template.Spec.SecurityContext = podSecurityContext
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["thunderid.io/config-hash"] = configHash

	// Compare only the fields we actively manage — avoids false diffs from
	// Kubernetes-defaulted fields (DefaultMode, MountPropagation, etc.).
	replicasMatch := instance.Spec.AutoScaling != nil || instance.Spec.Replicas == 0 ||
		(originalReplicas != nil && *originalReplicas == instance.Spec.Replicas)
	if !hasBootstrapVolume &&
		securityVolumeCurrent &&
		originalHash == configHash &&
		originalImage == instance.Spec.Image &&
		originalCmd == startCmd &&
		equality.Semantic.DeepEqual(originalResources, &instance.Spec.Resources) &&
		equality.Semantic.DeepEqual(originalLivenessProbe, dep.Spec.Template.Spec.Containers[0].LivenessProbe) &&
		equality.Semantic.DeepEqual(originalReadinessProbe, dep.Spec.Template.Spec.Containers[0].ReadinessProbe) &&
		equality.Semantic.DeepEqual(originalSecurityContext, podSecurityContext) &&
		equality.Semantic.DeepEqual(originalInitContainers, initContainers) &&
		replicasMatch {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, latest); err != nil {
			return err
		}
		latest.Spec = dep.Spec
		return r.Update(ctx, latest)
	})

}

func (r *ThunderIDInstanceReconciler) reconcileService(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, svc)
	if errors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Name,
				Namespace: instance.Namespace,
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": instance.Name},
				Ports: []corev1.ServicePort{
					{
						Port:       8090,
						TargetPort: intstr.FromInt(8090),
						Protocol:   corev1.ProtocolTCP,
					},
				},
			},
		}
		_ = ctrl.SetControllerReference(instance, svc, r.Scheme)
		return r.Create(ctx, svc)
	}
	return err
}

func (r *ThunderIDInstanceReconciler) reconcileIngress(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) error {
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, ing)
	if errors.IsNotFound(err) {
		ing = &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Name,
				Namespace: instance.Namespace,
				Annotations: map[string]string{
					"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
					"nginx.ingress.kubernetes.io/ssl-redirect":     "true",
				},
			},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{
					{
						Host: instance.Spec.Domain,
						IngressRuleValue: networkingv1.IngressRuleValue{
							HTTP: &networkingv1.HTTPIngressRuleValue{
								Paths: []networkingv1.HTTPIngressPath{
									{
										Path:     "/",
										PathType: &pathType,
										Backend: networkingv1.IngressBackend{
											Service: &networkingv1.IngressServiceBackend{
												Name: instance.Name,
												Port: networkingv1.ServiceBackendPort{
													Number: 8090,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		if instance.Spec.TLSSecret != "" {
			ing.Spec.TLS = []networkingv1.IngressTLS{
				{
					Hosts:      []string{instance.Spec.Domain},
					SecretName: instance.Spec.TLSSecret,
				},
			}
		}
		_ = ctrl.SetControllerReference(instance, ing, r.Scheme)
		return r.Create(ctx, ing)
	}
	return err
}

func (r *ThunderIDInstanceReconciler) reconcileHPA(ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) error {
	hpaKey := types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}

	if instance.Spec.AutoScaling == nil {
		// No autoscaling desired — delete HPA if it exists.
		existing := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := r.Get(ctx, hpaKey, existing); err == nil {
			return r.Delete(ctx, existing)
		}
		return nil
	}

	as := instance.Spec.AutoScaling
	metrics := buildMetrics(as)

	desired := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       instance.Name,
			},
			MinReplicas: &as.MinReplicas,
			MaxReplicas: as.MaxReplicas,
			Metrics:     metrics,
		},
	}
	_ = ctrl.SetControllerReference(instance, desired, r.Scheme)

	existing := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, hpaKey, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = desired.Spec
	return r.Patch(ctx, existing, patch)
}

func buildMetrics(as *appsv1alpha1.AutoScalingSpec) []autoscalingv2.MetricSpec {
	var metrics []autoscalingv2.MetricSpec

	if as.TargetCPUUtilizationPercentage > 0 {
		pct := as.TargetCPUUtilizationPercentage
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &pct,
				},
			},
		})
	}

	if as.TargetMemoryUtilizationPercentage > 0 {
		pct := as.TargetMemoryUtilizationPercentage
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceMemory,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &pct,
				},
			},
		})
	}

	// Default to 80% CPU if no metrics were specified.
	if len(metrics) == 0 {
		cpu := int32(80)
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &cpu,
				},
			},
		})
	}

	return metrics
}

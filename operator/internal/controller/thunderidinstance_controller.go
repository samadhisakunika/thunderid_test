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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

//go:embed deployment-config.yaml
var deploymentConfigTemplate string

// ThunderIDInstanceReconciler reconciles a ThunderIDInstance object
type ThunderIDInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC annotations for the controller
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
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
	// Hash includes themes-version and flows-version annotations so pod rolls when either changes.
	configHash := func() string {
		h := sha256.New()
		h.Write([]byte(publicURL + instance.Spec.ResourcesConfigMap +
			instance.Annotations["thunderid.io/themes-version"] +
			instance.Annotations["thunderid.io/flows-version"] +
			instance.Annotations["thunderid.io/applications-version"]))
		return hex.EncodeToString(h.Sum(nil))[:16]
	}()

	if err := r.reconcileDeployment(ctx, instance, configHash, resourcesContent != ""); err != nil {
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

	// Re-fetch before status update — annotations may have been patched by the
	// ThemeReconciler between our initial Get and here, making our copy stale.
	statusErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &appsv1alpha1.ThunderIDInstance{}
		if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
			return err
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
		For(&appsv1alpha1.ThunderIDInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&corev1.ConfigMap{}).
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

	// Append theme documents if the themes ConfigMap exists.
	themesCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      instance.Name + "-themes",
		Namespace: instance.Namespace,
	}, themesCM); err == nil {
		if themesYAML := themesCM.Data["themes.yaml"]; themesYAML != "" {
			resourcesContent += "\n" + themesYAML
		}
	} else if !errors.IsNotFound(err) {
		return "", err
	}

	// Append flow documents if the flows ConfigMap exists.
	flowsCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      instance.Name + "-flows",
		Namespace: instance.Namespace,
	}, flowsCM); err == nil {
		if flowsYAML := flowsCM.Data["flows.yaml"]; flowsYAML != "" {
			resourcesContent += "\n" + flowsYAML
		}
	} else if !errors.IsNotFound(err) {
		return "", err
	}

	// Append application documents if the applications ConfigMap exists.
	appsCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      instance.Name + "-applications",
		Namespace: instance.Namespace,
	}, appsCM); err == nil {
		if appsYAML := appsCM.Data["applications.yaml"]; appsYAML != "" {
			resourcesContent += "\n" + appsYAML
		}
	} else if !errors.IsNotFound(err) {
		return "", err
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

func (r *ThunderIDInstanceReconciler) reconcileDeployment(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, configHash string, hasResources bool) error {
	replicas := instance.Spec.Replicas
	configMapName := instance.Name + "-config"

	startCmd := "cd /opt/thunderid && ./setup.sh && ./start.sh"
	if hasResources {
		startCmd = "cd /opt/thunderid && ./setup.sh && ./start.sh /opt/thunderid/resources.yaml"
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

	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
				},
			},
		},
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
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": instance.Name},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": instance.Name},
						Annotations: map[string]string{"thunderid.io/config-hash": configHash},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:         "thunderid",
								Image:        instance.Spec.Image,
								Command:      []string{"/bin/bash", "-c", startCmd},
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
									InitialDelaySeconds: 15,
									PeriodSeconds:       10,
									FailureThreshold:    3,
								},
								ReadinessProbe: &corev1.Probe{
									ProbeHandler: corev1.ProbeHandler{
										HTTPGet: &corev1.HTTPGetAction{
											Path:   "/health/readiness",
											Port:   intstr.FromInt(8090),
											Scheme: corev1.URISchemeHTTPS,
										},
									},
									InitialDelaySeconds: 10,
									PeriodSeconds:       5,
									FailureThreshold:    6,
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

	original := dep.DeepCopy() // <-- new: snapshot before mutating

	if instance.Spec.AutoScaling == nil && instance.Spec.Replicas > 0 {
		r := instance.Spec.Replicas
		dep.Spec.Replicas = &r
	}

	dep.Spec.Template.Spec.Containers[0].Image = instance.Spec.Image
	dep.Spec.Template.Spec.Containers[0].Command = []string{"/bin/bash", "-c", startCmd}
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
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
	dep.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/health/readiness",
				Port:   intstr.FromInt(8090),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       5,
		FailureThreshold:    6,
	}
	dep.Spec.Template.Spec.Volumes = volumes
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["thunderid.io/config-hash"] = configHash
	if equality.Semantic.DeepEqual(original.Spec, dep.Spec) {
		return nil
	}
	return r.Update(ctx, dep)

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

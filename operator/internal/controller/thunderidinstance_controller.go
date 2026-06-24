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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
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

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=thunderidinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

func (r *ThunderIDInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	publicURL := "https://" + instance.Spec.Domain

	if err := r.reconcileConfigMap(ctx, instance, publicURL); err != nil {
		log.Error(err, "failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	configHash := func() string {
		h := sha256.New()
		h.Write([]byte(publicURL + instance.Spec.ResourcesYAML + instance.Spec.ResourcesConfigMap))
		return hex.EncodeToString(h.Sum(nil))[:16]
	}()

	if err := r.reconcileDeployment(ctx, instance, configHash); err != nil {
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

	instance.Status.URL = publicURL
	instance.Status.Phase = "Running"
	if err := r.Status().Update(ctx, instance); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	log.Info("reconciled ThunderIDInstance", "url", publicURL)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ThunderIDInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.ThunderIDInstance{}).
		Named("thunderidinstance").
		Complete(r)
}
func (r *ThunderIDInstanceReconciler) reconcileConfigMap(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, publicURL string) error {

	configContent := fmt.Sprintf(deploymentConfigTemplate, publicURL, publicURL, publicURL)

	data := map[string]string{
		"deployment.yaml": configContent,
	}
	if instance.Spec.ResourcesYAML != "" {
		data["resources.yaml"] = instance.Spec.ResourcesYAML
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
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	cm.Data = data
	return r.Update(ctx, cm)
}

func (r *ThunderIDInstanceReconciler) reconcileDeployment(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance, configHash string) error {
	replicas := instance.Spec.Replicas
	configMapName := instance.Name + "-config"

	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, dep)
	if errors.IsNotFound(err) {
		dep = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Name,
				Namespace: instance.Namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": instance.Name,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": instance.Name},
						Annotations: map[string]string{"thunderid.io/config-hash": configHash},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "thunderid",
								Image: instance.Spec.Image,
								Command: []string{"/bin/bash", "-c", func() string {
									if instance.Spec.ResourcesYAML != "" || instance.Spec.ResourcesConfigMap != "" {
										return "cd /opt/thunderid && ./setup.sh && ./start.sh /opt/thunderid/resources.yaml"
									}
									return "cd /opt/thunderid && ./setup.sh && ./start.sh"
								}()},

								Ports: []corev1.ContainerPort{
									{ContainerPort: 8090, Protocol: corev1.ProtocolTCP},
								},
								VolumeMounts: func() []corev1.VolumeMount {
									mounts := []corev1.VolumeMount{
										{
											Name:      "config",
											MountPath: "/opt/thunderid/repository/conf/deployment.yaml",
											SubPath:   "deployment.yaml",
										},
									}
									if instance.Spec.ResourcesYAML != "" {
										mounts = append(mounts, corev1.VolumeMount{
											Name:      "config",
											MountPath: "/opt/thunderid/resources.yaml",
											SubPath:   "resources.yaml",
										})
									} else if instance.Spec.ResourcesConfigMap != "" {
										mounts = append(mounts, corev1.VolumeMount{
											Name:      "resources",
											MountPath: "/opt/thunderid/resources.yaml",
											SubPath:   "resources.yaml",
										})
									}
									return mounts
								}(),
							},
						},
						Volumes: func() []corev1.Volume {
							vols := []corev1.Volume{
								{
									Name: "config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
										},
									},
								},
							}
							if instance.Spec.ResourcesConfigMap != "" {
								vols = append(vols, corev1.Volume{
									Name: "resources",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: instance.Spec.ResourcesConfigMap,
											},
										},
									},
								})
							}
							return vols
						}(),
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
	dep.Spec.Replicas = &replicas
	dep.Spec.Template.Spec.Containers[0].Image = instance.Spec.Image
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["thunderid.io/config-hash"] = configHash
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

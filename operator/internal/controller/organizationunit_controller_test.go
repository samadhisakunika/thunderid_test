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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

var _ = Describe("OrganizationUnit Controller", func() {
	const (
		instanceName = "test-tid-ou"
		namespace    = "default"
		crName       = "test-ou-ctrl"
	)

	ctx := context.Background()
	instanceKey := types.NamespacedName{Name: instanceName, Namespace: namespace}
	ouKey := types.NamespacedName{Name: crName, Namespace: namespace}
	cmKey := types.NamespacedName{Name: instanceName + "-organizationunits", Namespace: namespace}

	var instance *appsv1alpha1.ThunderIDInstance

	BeforeEach(func() {
		instance = &appsv1alpha1.ThunderIDInstance{}
		err := k8sClient.Get(ctx, instanceKey, instance)
		if err != nil && errors.IsNotFound(err) {
			instance = &appsv1alpha1.ThunderIDInstance{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: namespace},
				Spec:       appsv1alpha1.ThunderIDInstanceSpec{Domain: "test.example.com", Image: "thunderid:latest"},
			}
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
		}

		ou := &appsv1alpha1.OrganizationUnit{}
		err = k8sClient.Get(ctx, ouKey, ou)
		if err != nil && errors.IsNotFound(err) {
			ou = &appsv1alpha1.OrganizationUnit{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: appsv1alpha1.OrganizationUnitSpec{
					InstanceRef: instanceName,
					Handle:      "test-ou-handle",
					Name:        "Test Organization Unit",
					Description: "A test OU",
				},
			}
			Expect(k8sClient.Create(ctx, ou)).To(Succeed())
		}
	})

	AfterEach(func() {
		ou := &appsv1alpha1.OrganizationUnit{}
		if err := k8sClient.Get(ctx, ouKey, ou); err == nil {
			ou.Finalizers = nil
			_ = k8sClient.Update(ctx, ou)
			_ = k8sClient.Delete(ctx, ou)
		}
		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, cmKey, cm); err == nil {
			_ = k8sClient.Delete(ctx, cm)
		}
		inst := &appsv1alpha1.ThunderIDInstance{}
		if err := k8sClient.Get(ctx, instanceKey, inst); err == nil {
			_ = k8sClient.Delete(ctx, inst)
		}
	})

	newReconciler := func() *OrganizationUnitReconciler {
		return &OrganizationUnitReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: newNoopRecorder(),
		}
	}

	It("should add finalizer on first reconcile", func() {
		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())

		ou := &appsv1alpha1.OrganizationUnit{}
		Expect(k8sClient.Get(ctx, ouKey, ou)).To(Succeed())
		Expect(ou.Finalizers).To(ContainElement(organizationUnitFinalizer))
	})

	It("should create the organizationunits ConfigMap on second reconcile", func() {
		r := newReconciler()
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("organizationunits.yaml"))

		yaml := cm.Data["organizationunits.yaml"]
		Expect(yaml).To(ContainSubstring("resource_type: organization_unit"))
		Expect(yaml).To(ContainSubstring("handle: 'test-ou-handle'"))
		Expect(yaml).To(ContainSubstring("name: 'Test Organization Unit'"))
	})

	It("should stamp the version annotation on ThunderIDInstance", func() {
		r := newReconciler()
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())

		inst := &appsv1alpha1.ThunderIDInstance{}
		Expect(k8sClient.Get(ctx, instanceKey, inst)).To(Succeed())
		Expect(inst.Annotations).To(HaveKey("thunderid.io/organizationunits-version"))
		Expect(inst.Annotations["thunderid.io/organizationunits-version"]).NotTo(BeEmpty())
	})

	It("should be idempotent across reconciles", func() {
		r := newReconciler()
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())

		cm1 := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm1)).To(Succeed())
		firstContent := cm1.Data["organizationunits.yaml"]

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())

		cm2 := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm2)).To(Succeed())
		Expect(cm2.Data["organizationunits.yaml"]).To(Equal(firstContent))
	})

	It("should emit parent in YAML when parentId is set", func() {
		ou := &appsv1alpha1.OrganizationUnit{}
		Expect(k8sClient.Get(ctx, ouKey, ou)).To(Succeed())
		ou.Spec.Parent = "019ef85a-0eab-7d82-b9a9-62b8be7efe8d"
		Expect(k8sClient.Update(ctx, ou)).To(Succeed())

		r := newReconciler()
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: ouKey})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
		Expect(cm.Data["organizationunits.yaml"]).To(ContainSubstring("parent:"))
	})
})

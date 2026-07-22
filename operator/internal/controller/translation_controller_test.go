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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

var _ = Describe("Translation Controller", func() {
	const (
		instanceName = "test-tid-translation"
		namespace    = "default"
		language     = "en-US"
		crName       = "test-translation-en"
	)

	ctx := context.Background()

	instanceKey := types.NamespacedName{Name: instanceName, Namespace: namespace}
	translationKey := types.NamespacedName{Name: crName, Namespace: namespace}
	cmKey := types.NamespacedName{Name: instanceName + "-translations", Namespace: namespace}

	var instance *appsv1alpha1.ThunderIDInstance

	BeforeEach(func() {
		instance = &appsv1alpha1.ThunderIDInstance{}
		err := k8sClient.Get(ctx, instanceKey, instance)
		if err != nil && errors.IsNotFound(err) {
			instance = &appsv1alpha1.ThunderIDInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: namespace,
				},
				Spec: appsv1alpha1.ThunderIDInstanceSpec{
					Domain: "test.example.com",
					Image:  "thunderid:latest",
				},
			}
			Expect(k8sClient.Create(ctx, instance)).To(Succeed())
		}

		tr := &appsv1alpha1.Translation{}
		err = k8sClient.Get(ctx, translationKey, tr)
		if err != nil && errors.IsNotFound(err) {
			tr = &appsv1alpha1.Translation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      crName,
					Namespace: namespace,
				},
				Spec: appsv1alpha1.TranslationSpec{
					InstanceRef: instanceName,
					Language:    language,
					Translations: map[string]map[string]string{
						"auth": {
							"login":  "Log in",
							"logout": "Log out",
						},
						"errors": {
							"generic": "Something went wrong",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, tr)).To(Succeed())
		}
	})

	AfterEach(func() {
		tr := &appsv1alpha1.Translation{}
		if err := k8sClient.Get(ctx, translationKey, tr); err == nil {
			tr.Finalizers = nil
			_ = k8sClient.Update(ctx, tr)
			_ = k8sClient.Delete(ctx, tr)
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

	It("should add finalizer on first reconcile", func() {
		reconciler := &TranslationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: newNoopRecorder(),
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		tr := &appsv1alpha1.Translation{}
		Expect(k8sClient.Get(ctx, translationKey, tr)).To(Succeed())
		Expect(tr.Finalizers).To(ContainElement(translationFinalizer))
	})

	It("should create the translations ConfigMap on second reconcile", func() {
		reconciler := &TranslationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: newNoopRecorder(),
		}

		// First reconcile adds finalizer.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		// Second reconcile builds the ConfigMap.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("translations.yaml"))

		yaml := cm.Data["translations.yaml"]
		Expect(yaml).To(ContainSubstring("language: 'en-US'"))
		Expect(yaml).To(ContainSubstring("auth:"))
		Expect(yaml).To(ContainSubstring("login: 'Log in'"))
		Expect(yaml).To(ContainSubstring("logout: 'Log out'"))
		Expect(yaml).To(ContainSubstring("errors:"))
		Expect(yaml).To(ContainSubstring("generic: 'Something went wrong'"))
	})

	It("should stamp the version annotation on the ThunderIDInstance", func() {
		reconciler := &TranslationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: newNoopRecorder(),
		}

		_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		inst := &appsv1alpha1.ThunderIDInstance{}
		Expect(k8sClient.Get(ctx, instanceKey, inst)).To(Succeed())
		Expect(inst.Annotations).To(HaveKey("thunderid.io/translations-version"))
		Expect(inst.Annotations["thunderid.io/translations-version"]).NotTo(BeEmpty())
	})

	It("should produce stable YAML across reconciles (idempotent)", func() {
		reconciler := &TranslationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: newNoopRecorder(),
		}

		_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		cm1 := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm1)).To(Succeed())
		firstContent := cm1.Data["translations.yaml"]

		// Reconcile again — content must not change.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		cm2 := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm2)).To(Succeed())
		Expect(cm2.Data["translations.yaml"]).To(Equal(firstContent))
	})

	It("should output namespaces and keys in sorted order", func() {
		reconciler := &TranslationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: newNoopRecorder(),
		}

		_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: translationKey})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
		yaml := cm.Data["translations.yaml"]

		authIdx := strings.Index(yaml, "auth:")
		errorsIdx := strings.Index(yaml, "errors:")
		Expect(authIdx).To(BeNumerically("<", errorsIdx), "auth: should come before errors: in sorted output")

		loginIdx := strings.Index(yaml, "login:")
		logoutIdx := strings.Index(yaml, "logout:")
		Expect(loginIdx).To(BeNumerically("<", logoutIdx), "login: should come before logout:")
	})
})

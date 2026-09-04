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
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// Shared fixtures for every *_test.go file in this package that only needs an in-memory fake
// client (no real API server / envtest) - fast, no external binaries, one scheme builder per
// reconciler type instead of each test file inventing its own.

func newFakeReconcilerScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = autoscalingv2.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = appsv1alpha1.AddToScheme(scheme)
	return scheme
}

func newFakeReconciler(objs ...runtime.Object) *ThunderIDInstanceReconciler {
	scheme := newFakeReconcilerScheme()
	return &ThunderIDInstanceReconciler{
		// WithStatusSubresource is required for a CRD type: without it the fake client's
		// Status().Update() doesn't track the status subresource separately and errors instead
		// of applying the update.
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.ThunderIDInstance{}).
			WithRuntimeObjects(objs...).Build(),
		Scheme: scheme,
	}
}

// newFakeReconcilerWithInterceptor is newFakeReconciler plus the ability to make a specific
// client call fail on command (see sigs.k8s.io/controller-runtime/pkg/client/interceptor) — for
// exercising a Reconcile branch that only fires when a write genuinely fails, which the plain
// fake client never does on its own for a well-formed request.
func newFakeReconcilerWithInterceptor(funcs interceptor.Funcs, objs ...runtime.Object) *ThunderIDInstanceReconciler {
	scheme := newFakeReconcilerScheme()
	return &ThunderIDInstanceReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.ThunderIDInstance{}).
			WithInterceptorFuncs(funcs).WithRuntimeObjects(objs...).Build(),
		Scheme: scheme,
	}
}

func newFakeResourceReconcilerScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = appsv1alpha1.AddToScheme(scheme)
	return scheme
}

func newFakeResourceReconciler(objs ...runtime.Object) *ResourceReconciler {
	scheme := newFakeResourceReconcilerScheme()
	return &ResourceReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.Resource{}).
			WithRuntimeObjects(objs...).Build(),
		Scheme: scheme,
	}
}

// newFakeResourceReconcilerWithInterceptor is newFakeResourceReconciler plus the ability to make
// a specific client call fail on command — see newFakeReconcilerWithInterceptor's doc comment.
func newFakeResourceReconcilerWithInterceptor(funcs interceptor.Funcs, objs ...runtime.Object) *ResourceReconciler {
	scheme := newFakeResourceReconcilerScheme()
	return &ResourceReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.Resource{}).
			WithInterceptorFuncs(funcs).WithRuntimeObjects(objs...).Build(),
		Scheme: scheme,
	}
}

func newFakeEnvironmentReconcilerScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1alpha1.AddToScheme(scheme)
	return scheme
}

func newFakeEnvironmentReconciler(objs ...runtime.Object) *EnvironmentReconciler {
	scheme := newFakeEnvironmentReconcilerScheme()
	return &EnvironmentReconciler{
		// WithStatusSubresource is required for a CRD type: without it the fake client's
		// Status().Update() doesn't track the status subresource separately and errors instead
		// of applying the update.
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.Environment{}).
			WithRuntimeObjects(objs...).Build(),
		Scheme: scheme,
	}
}

// newFakeEnvironmentReconcilerWithInterceptor is newFakeEnvironmentReconciler plus the ability to
// make a specific client call fail on command — see newFakeReconcilerWithInterceptor's doc comment.
func newFakeEnvironmentReconcilerWithInterceptor(funcs interceptor.Funcs, objs ...runtime.Object) *EnvironmentReconciler {
	scheme := newFakeEnvironmentReconcilerScheme()
	return &EnvironmentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.Environment{}).
			WithInterceptorFuncs(funcs).WithRuntimeObjects(objs...).Build(),
		Scheme: scheme,
	}
}

func fakeRecorder() record.EventRecorder {
	return record.NewFakeRecorder(20)
}

func reconcileRequest(name, namespace string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func rawSpec(json string) runtime.RawExtension {
	return runtime.RawExtension{Raw: []byte(json)}
}

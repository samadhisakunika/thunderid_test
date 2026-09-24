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
	"errors"
	"testing"

	"github.com/stretchr/testify/suite"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// This file exercises ThunderIDInstanceReconciler.Reconcile's nine "a sub-step failed" wrapper
// branches — each is just `if err := r.subStep(...); err != nil { log+event; return err }`, and
// none had ever been made to actually fail. Where the sub-step's own logic can fail on bad input
// (a dangling secretRef, an unparseable quantity), that's simpler and used instead of an
// interceptor. Where the only way to fail is a write genuinely erroring, WithInterceptorFuncs
// forces exactly one call to fail without touching any of the others in the same Reconcile.

func baselineSQLiteInstance() *appsv1alpha1.ThunderIDInstance {
	return &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Image:          "thunderid:latest",
			DataVolumeSize: "1Gi",
		},
	}
}

type ReconcileFailureTestSuite struct {
	suite.Suite
}

func TestReconcileFailureTestSuite(t *testing.T) {
	suite.Run(t, new(ReconcileFailureTestSuite))
}

func (suite *ReconcileFailureTestSuite) TestReconcileConfigMapErrorPropagates() {
	instance := baselineSQLiteInstance()
	wantErr := errors.New("simulated ConfigMap create failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*corev1.ConfigMap); ok {
				return wantErr
			}
			return c.Create(ctx, o, opts...)
		},
	}, instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileFailureTestSuite) TestResolveEnvSecretHashErrorPropagates() {
	instance := baselineSQLiteInstance()
	env := &appsv1alpha1.EnvironmentValues{
		ObjectMeta: metav1.ObjectMeta{
			Name: "missing-secret", Namespace: instance.Namespace,
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: instance.Name},
		},
	}
	r := newFakeReconciler(instance, env)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.Contains(err.Error(), "missing-secret")
}

func (suite *ReconcileFailureTestSuite) TestReconcileHPAErrorPropagates() {
	instance := baselineSQLiteInstance()
	instance.Spec.AutoScaling = &appsv1alpha1.AutoScalingSpec{MinReplicas: 1, MaxReplicas: 3}
	wantErr := errors.New("simulated HPA create failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*autoscalingv2.HorizontalPodAutoscaler); ok {
				return wantErr
			}
			return c.Create(ctx, o, opts...)
		},
	}, instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileFailureTestSuite) TestReconcileSecuritySecretErrorPropagates() {
	instance := baselineSQLiteInstance()
	instance.Spec.SecuritySecret = "missing-secret"
	r := newFakeReconciler(instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.Contains(err.Error(), "securitySecret")
}

func (suite *ReconcileFailureTestSuite) TestReconcileBootstrapJobErrorPropagates() {
	instance := postgresInstance()
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	wantErr := errors.New("simulated Job create failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*batchv1.Job); ok {
				return wantErr
			}
			return c.Create(ctx, o, opts...)
		},
	}, instance, dbSecret)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileFailureTestSuite) TestReconcileDataVolumeErrorPropagates() {
	instance := baselineSQLiteInstance()
	instance.Spec.DataVolumeSize = "not-a-valid-quantity"
	r := newFakeReconciler(instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.Contains(err.Error(), "dataVolumeSize")
}

func (suite *ReconcileFailureTestSuite) TestReconcileDeploymentErrorPropagates() {
	instance := baselineSQLiteInstance()
	wantErr := errors.New("simulated Deployment create failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*appsv1.Deployment); ok {
				return wantErr
			}
			return c.Create(ctx, o, opts...)
		},
	}, instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileFailureTestSuite) TestReconcileServiceErrorPropagates() {
	instance := baselineSQLiteInstance()
	wantErr := errors.New("simulated Service create failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*corev1.Service); ok {
				return wantErr
			}
			return c.Create(ctx, o, opts...)
		},
	}, instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileFailureTestSuite) TestReconcileIngressErrorPropagates() {
	instance := baselineSQLiteInstance()
	instance.Spec.Domain = "id.example.com"
	wantErr := errors.New("simulated Ingress create failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*networkingv1.Ingress); ok {
				return wantErr
			}
			return c.Create(ctx, o, opts...)
		},
	}, instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileFailureTestSuite) TestReconcileStatusUpdateRetryGetFailure() {
	instance := baselineSQLiteInstance()
	wantErr := errors.New("simulated re-fetch failure")

	// ThunderIDInstance is fetched by Get exactly twice in one successful Reconcile: once at
	// the very top, and once inside the final status-update retry loop. None of the nine
	// sub-steps in between ever Get a ThunderIDInstance themselves, so counting to the second
	// call cleanly targets just that inner Get without needing to distinguish it any other way.
	getCount := 0
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*appsv1alpha1.ThunderIDInstance); ok {
				getCount++
				if getCount == 2 {
					return wantErr
				}
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("inst"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

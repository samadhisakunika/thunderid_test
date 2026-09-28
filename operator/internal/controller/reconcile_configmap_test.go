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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// reconcileConfigMap, appendResourceConfigMap, and reconcileDataVolume's own internal error
// paths and untested logic branches - one level deeper than the "Reconcile dispatches to
// sub-step, sub-step fails" wrappers already covered in reconcile_failure_test.go.

type ReconcileConfigMapTestSuite struct {
	suite.Suite
}

func TestReconcileConfigMapTestSuite(t *testing.T) {
	suite.Run(t, new(ReconcileConfigMapTestSuite))
}

func (suite *ReconcileConfigMapTestSuite) TestDatabaseConfigErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
				Config: appsv1alpha1.DatabaseBackendSpec{Type: "postgres", Postgres: &appsv1alpha1.PostgresBackendSpec{
					Hostname: "h", Username: "u", Name: "n",
				}},
				// PasswordRef deliberately unset - resolveDatabaseConfig errors immediately.
			}},
		},
	}
	r := newFakeReconciler(instance)
	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().Error(err)
	suite.Contains(err.Error(), "passwordRef")
}

func (suite *ReconcileConfigMapTestSuite) TestEmailConfigErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Email: &appsv1alpha1.EmailSpec{
				SMTP: appsv1alpha1.SMTPConfigSpec{
					Host: "smtp.example.com", Port: 587, FromAddress: "noreply@example.com",
					EnableAuthentication: true,
				},
				// SecretRef deliberately unset.
			}},
		},
	}
	r := newFakeReconciler(instance)
	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().Error(err)
	suite.Contains(err.Error(), "secretRef")
}

func (suite *ReconcileConfigMapTestSuite) TestPasskeyExtraOriginsRendered() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Passkey: appsv1alpha1.PasskeyConfigSpec{
				ExtraAllowedOrigins: []string{"https://extra.example.com"},
			}},
		},
	}
	r := newFakeReconciler(instance)
	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().NoError(err)

	cm := &corev1.ConfigMap{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "inst-config", Namespace: "default"}, cm))
	suite.Contains(cm.Data["deployment.yaml"], "https://extra.example.com")
}

func (suite *ReconcileConfigMapTestSuite) TestExternalResourcesConfigMapNotFoundPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{ResourcesConfigMap: "does-not-exist"},
	}
	r := newFakeReconciler(instance)
	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().Error(err)
}

func (suite *ReconcileConfigMapTestSuite) TestExternalResourcesConfigMapContentIncluded() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{ResourcesConfigMap: "base-resources"},
	}
	baseCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "base-resources", Namespace: "default"},
		Data:       map[string]string{"resources.yaml": "---\nresource_type: organization_unit\nname: 'default'\n"},
	}
	r := newFakeReconciler(instance, baseCM)
	resourcesContent, baseResourcesContent, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().NoError(err)
	suite.Contains(resourcesContent, "organization_unit")
	suite.Equal(baseResourcesContent, resourcesContent, "no per-type ConfigMaps exist, so base content is the whole thing")

	cm := &corev1.ConfigMap{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "inst-config", Namespace: "default"}, cm))
	suite.Contains(cm.Data["resources.yaml"], "organization_unit")
}

func (suite *ReconcileConfigMapTestSuite) TestAppendResourceConfigMapLoopErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	wantErr := errors.New("simulated per-type ConfigMap get failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if key.Name == "inst-agents" {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)

	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileConfigMapTestSuite) TestConfigMapGetErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	wantErr := errors.New("simulated config ConfigMap get failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if key.Name == "inst-config" {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)

	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileConfigMapTestSuite) TestConfigMapRetryGetErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-config", Namespace: "default"},
		Data:       map[string]string{"deployment.yaml": "stale content that will differ from a freshly rendered template"},
	}
	wantErr := errors.New("simulated retry-get failure")
	getCount := 0
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if key.Name == "inst-config" {
				getCount++
				if getCount == 2 {
					return wantErr
				}
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance, existingCM)

	_, _, _, _, err := r.reconcileConfigMap(context.Background(), instance, "https://localhost:8090")
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileConfigMapTestSuite) TestAppendResourceConfigMapGetErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	wantErr := errors.New("simulated get failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.ConfigMap); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)

	_, err := r.appendResourceConfigMap(context.Background(), instance, "-presentationdefinitions", "presentationdefinitions.yaml")
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *ReconcileConfigMapTestSuite) TestReconcileDataVolumeGetErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{DataVolumeSize: "1Gi"},
	}
	wantErr := errors.New("simulated PVC get failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.PersistentVolumeClaim); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)

	err := r.reconcileDataVolume(context.Background(), instance)
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

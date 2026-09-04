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
	"testing"

	stderrors "errors"

	"github.com/stretchr/testify/suite"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

type WatchMappingTestSuite struct {
	suite.Suite
}

func TestWatchMappingTestSuite(t *testing.T) {
	suite.Run(t, new(WatchMappingTestSuite))
}

func (suite *WatchMappingTestSuite) TestInstancesForEnvSecretNoMatches() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "other-secret"},
	}
	r := newFakeReconciler(instance)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "env-secret", Namespace: "default"}}

	reqs := r.instancesForEnvSecret(context.Background(), secret)
	suite.Empty(reqs)
}

func (suite *WatchMappingTestSuite) TestInstancesForEnvSecretMatchesByNameAndNamespace() {
	matching := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "env-secret"},
	}
	differentNamespace := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "other-ns", Namespace: "other"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "env-secret"},
	}
	notReferencing := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "not-referencing", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "some-other-secret"},
	}
	r := newFakeReconciler(matching, differentNamespace, notReferencing)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "env-secret", Namespace: "default"}}

	reqs := r.instancesForEnvSecret(context.Background(), secret)
	suite.Require().Len(reqs, 1)
	suite.Equal("matching", reqs[0].Name)
}

func (suite *WatchMappingTestSuite) TestInstancesForEnvSecretListErrorReturnsNil() {
	wantErr := stderrors.New("simulated list failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if _, ok := l.(*appsv1alpha1.ThunderIDInstanceList); ok {
				return wantErr
			}
			return c.List(ctx, l, opts...)
		},
	})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "env-secret", Namespace: "default"}}

	reqs := r.instancesForEnvSecret(context.Background(), secret)
	suite.Nil(reqs)
}

func (suite *WatchMappingTestSuite) TestMapServingPodToInstanceIgnoresNonPodObjects() {
	r := newFakeReconciler()
	reqs := r.mapServingPodToInstance(context.Background(), &corev1.Secret{})
	suite.Nil(reqs)
}

func (suite *WatchMappingTestSuite) TestMapServingPodToInstanceIgnoresPodWithoutAppLabel() {
	r := newFakeReconciler()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	reqs := r.mapServingPodToInstance(context.Background(), pod)
	suite.Nil(reqs)
}

func (suite *WatchMappingTestSuite) TestMapServingPodToInstanceNoBootstrapJobJustMapsRequest() {
	r := newFakeReconciler()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Labels: map[string]string{"app": "inst"}}}

	reqs := r.mapServingPodToInstance(context.Background(), pod)
	suite.Require().Len(reqs, 1)
	suite.Equal(types.NamespacedName{Name: "inst", Namespace: "default"}, reqs[0].NamespacedName)
}

func (suite *WatchMappingTestSuite) TestMapServingPodToInstanceLeavesHealthyBootstrapJobAlone() {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "inst-bootstrap-abc", Namespace: "default",
			Labels: map[string]string{"app": "inst", "thunderid.io/role": "bootstrap"},
		},
	}
	r := newFakeReconciler(job)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Labels: map[string]string{"app": "inst"}}}

	reqs := r.mapServingPodToInstance(context.Background(), pod)
	suite.Require().Len(reqs, 1)

	err := r.Get(context.Background(), types.NamespacedName{Name: "inst-bootstrap-abc", Namespace: "default"}, &batchv1.Job{})
	suite.NoError(err, "a Job that isn't Failed must not be deleted")
}

func (suite *WatchMappingTestSuite) TestMapServingPodToInstanceDeletesFailedBootstrapJob() {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "inst-bootstrap-abc", Namespace: "default",
			Labels: map[string]string{"app": "inst", "thunderid.io/role": "bootstrap"},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}},
		},
	}
	r := newFakeReconciler(job)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Labels: map[string]string{"app": "inst"}}}

	reqs := r.mapServingPodToInstance(context.Background(), pod)
	suite.Require().Len(reqs, 1, "still returns the reconcile request even though the Job is being cleaned up")

	err := r.Get(context.Background(), types.NamespacedName{Name: "inst-bootstrap-abc", Namespace: "default"}, &batchv1.Job{})
	suite.True(errors.IsNotFound(err), "a Failed bootstrap Job for the pod's instance must be deleted so the next reconcile retries fresh")
}

func (suite *WatchMappingTestSuite) TestBootstrapAttempt() {
	cases := []struct {
		name  string
		label string
		want  int
	}{
		{"missing label defaults to 1", "", 1},
		{"parses a valid attempt", "3", 3},
		{"unparseable falls back to 1", "abc", 1},
		{"zero falls back to 1", "0", 1},
		{"negative falls back to 1", "-2", 1},
	}
	for _, c := range cases {
		suite.Run(c.name, func() {
			job := &batchv1.Job{}
			if c.label != "" {
				job.Labels = map[string]string{bootstrapAttemptLabel: c.label}
			}
			suite.Equal(c.want, bootstrapAttempt(job))
		})
	}
}

func (suite *WatchMappingTestSuite) TestBootstrapJobFailed() {
	cases := []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{"no conditions", &batchv1.Job{}, false},
		{"failed true", &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
		}}}, true},
		{"failed false", &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
		}}}, false},
		{"complete, not failed", &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}}}, false},
	}
	for _, c := range cases {
		suite.Run(c.name, func() {
			suite.Equal(c.want, bootstrapJobFailed(c.job))
		})
	}
}

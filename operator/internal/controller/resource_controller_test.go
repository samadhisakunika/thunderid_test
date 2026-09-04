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

	"github.com/stretchr/testify/suite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

type ResourceControllerTestSuite struct {
	suite.Suite
}

func TestResourceControllerTestSuite(t *testing.T) {
	suite.Run(t, new(ResourceControllerTestSuite))
}

func (suite *ResourceControllerTestSuite) TestParseResourceSpecValid() {
	resourceType, data, err := parseResourceSpec(rawSpec(`{"resource_type":"application","name":"Console"}`))
	suite.Require().NoError(err)
	suite.Equal("application", resourceType)
	suite.Equal("Console", data["name"])
}

func (suite *ResourceControllerTestSuite) TestParseResourceSpecEmpty() {
	resourceType, data, err := parseResourceSpec(runtime.RawExtension{})
	suite.Require().NoError(err)
	suite.Empty(resourceType)
	suite.Empty(data)
}

func (suite *ResourceControllerTestSuite) TestParseResourceSpecMalformedJSON() {
	_, _, err := parseResourceSpec(rawSpec(`{not json`))
	suite.Error(err)
}

func (suite *ResourceControllerTestSuite) TestBuildResourceYAMLUsesK8sUIDForOrdinaryTypes() {
	r := newFakeResourceReconciler()
	obj := appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{UID: types.UID("11111111-1111-1111-1111-111111111111")}}

	got, err := r.buildResourceYAML(obj, "application", map[string]interface{}{"name": "Console"})
	suite.Require().NoError(err)
	suite.Contains(got, "id: '11111111-1111-1111-1111-111111111111'")
}

func (suite *ResourceControllerTestSuite) TestBuildResourceYAMLUsesUUIDv7ForFlowAndTheme() {
	r := newFakeResourceReconciler()
	obj := appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{
		UID:               types.UID("11111111-1111-1111-1111-111111111111"),
		CreationTimestamp: metav1.Now(),
	}}

	for _, resourceType := range []string{"flow", "theme"} {
		got, err := r.buildResourceYAML(obj, resourceType, map[string]interface{}{"name": "x"})
		suite.Require().NoError(err)
		want := deterministicUUIDv7(obj.ObjectMeta)
		suite.Contains(got, "id: '"+want+"'")
		// Must not be the raw k8s UID for these two kinds.
		suite.NotContains(got, "id: '11111111-1111-1111-1111-111111111111'")
	}
}

func (suite *ResourceControllerTestSuite) TestRebuildResourcesConfigMapCreatesAndSkipsUnchanged() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	app := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-app", Namespace: "default",
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: rawSpec(`{"resource_type":"application","name":"Console"}`),
	}
	r := newFakeResourceReconciler(instance, app)

	changed, err := r.rebuildResourcesConfigMap(context.Background(), instance, "application", "")
	suite.Require().NoError(err)
	suite.True(changed, "first write must report a change")

	cm := &corev1.ConfigMap{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "inst-applications", Namespace: "default"}, cm))
	suite.Contains(cm.Data["applications.yaml"], "name: 'Console'")

	// The version annotation landed on the instance, so a second call with identical input
	// is a no-op (this is what keeps an unrelated resource_type's reconcile from restarting
	// the pod - see rebuildResourcesConfigMap's doc comment).
	updated := &appsv1alpha1.ThunderIDInstance{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "inst", Namespace: "default"}, updated))

	changedAgain, err := r.rebuildResourcesConfigMap(context.Background(), updated, "application", "")
	suite.Require().NoError(err)
	suite.False(changedAgain, "second call with unchanged content must not report a change")
}

func (suite *ResourceControllerTestSuite) TestRebuildResourcesConfigMapUnknownType() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	r := newFakeResourceReconciler(instance)

	_, err := r.rebuildResourcesConfigMap(context.Background(), instance, "not-a-real-type", "")
	suite.Require().Error(err)
	suite.Contains(err.Error(), "not-a-real-type")
}

func (suite *ResourceControllerTestSuite) TestRebuildResourcesConfigMapExcludesDeletedAndOtherInstances() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	included := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}},
		Spec:       rawSpec(`{"resource_type":"role","name":"keep-me"}`),
	}
	otherInstance := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "some-other-instance"}},
		Spec:       rawSpec(`{"resource_type":"role","name":"not-mine"}`),
	}
	otherType := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "wrong-type", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}},
		Spec:       rawSpec(`{"resource_type":"group","name":"not-a-role"}`),
	}
	r := newFakeResourceReconciler(instance, included, otherInstance, otherType)

	_, err := r.rebuildResourcesConfigMap(context.Background(), instance, "role", "")
	suite.Require().NoError(err)

	cm := &corev1.ConfigMap{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "inst-roles", Namespace: "default"}, cm))
	suite.Contains(cm.Data["roles.yaml"], "keep-me")
	suite.NotContains(cm.Data["roles.yaml"], "not-mine")
	suite.NotContains(cm.Data["roles.yaml"], "not-a-role")
}

func (suite *ResourceControllerTestSuite) TestRebuildResourcesConfigMapExcludesUID() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	// Unlike a real API server, the fake client's WithRuntimeObjects doesn't auto-generate a UID
	// for a seeded object - set one explicitly so excludeUID has something real to match against
	// (this is the finalizer-cleanup path's own use, see Reconcile's deletion branch).
	beingDeleted := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "leaving", Namespace: "default", UID: "leaving-uid",
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: rawSpec(`{"resource_type":"role","name":"leaving-role"}`),
	}
	r := newFakeResourceReconciler(instance, beingDeleted)

	_, err := r.rebuildResourcesConfigMap(context.Background(), instance, "role", "leaving-uid")
	suite.Require().NoError(err)

	cm := &corev1.ConfigMap{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "inst-roles", Namespace: "default"}, cm))
	suite.NotContains(cm.Data["roles.yaml"], "leaving-role")
}

func (suite *ResourceControllerTestSuite) TestResourcesForDeploymentFiltersByPhaseAndHealth() {
	list := []appsv1alpha1.Resource{
		{ObjectMeta: metav1.ObjectMeta{Name: "errored", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}}, Status: appsv1alpha1.ResourceStatus{Phase: "Error"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ready-healthy", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}}, Status: appsv1alpha1.ResourceStatus{Phase: "Ready"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other-instance", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "not-inst"}}, Status: appsv1alpha1.ResourceStatus{Phase: "Error"}},
	}
	objs := make([]runtime.Object, len(list))
	for i := range list {
		obj := list[i]
		objs[i] = &obj
	}
	r := newFakeResourceReconciler(objs...)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Status:     appsv1.DeploymentStatus{UnavailableReplicas: 0},
	}
	reqs := r.resourcesForDeployment(context.Background(), dep)

	var names []string
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	// Error-phase always re-triggers regardless of health; Ready-phase only re-triggers when
	// unavailable (0 here, so it's excluded); a different instance's Resource is never included.
	suite.Contains(names, "errored")
	suite.NotContains(names, "ready-healthy")
	suite.NotContains(names, "other-instance")
}

func (suite *ResourceControllerTestSuite) TestResourcesForDeploymentIncludesReadyWhenUnavailable() {
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-unhealthy", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}},
		Status:     appsv1alpha1.ResourceStatus{Phase: "Ready"},
	}
	r := newFakeResourceReconciler(obj)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Status:     appsv1.DeploymentStatus{UnavailableReplicas: 1},
	}
	reqs := r.resourcesForDeployment(context.Background(), dep)
	suite.Require().Len(reqs, 1)
	suite.Equal("ready-unhealthy", reqs[0].Name)
}

func (suite *ResourceControllerTestSuite) TestResourcesForInstance() {
	mine := &appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}}}
	notMine := &appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "not-mine", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "other"}}}
	r := newFakeResourceReconciler(mine, notMine)

	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	reqs := r.resourcesForInstance(context.Background(), instance)

	suite.Require().Len(reqs, 1)
	suite.Equal("mine", reqs[0].Name)
}

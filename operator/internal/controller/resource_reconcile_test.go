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
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

type ResourceReconcileTestSuite struct {
	suite.Suite
}

func TestResourceReconcileTestSuite(t *testing.T) {
	suite.Run(t, new(ResourceReconcileTestSuite))
}

func (suite *ResourceReconcileTestSuite) TestReconcileNotFound() {
	r := newFakeResourceReconciler()
	_, err := r.Reconcile(context.Background(), reconcileRequest("missing", "default"))
	suite.NoError(err)
}

// Reconcile's parseResourceSpec-error branch (invalid JSON in spec.Raw) turns out to be
// unreachable through any real object: runtime.RawExtension validates that Raw is well-formed
// JSON when it's marshaled, so no client — fake or real API server — can ever persist a Resource
// whose spec.Raw fails to parse back out. Confirmed by trying to seed exactly that: the fake
// client rejects it at Create time with "invalid character ... looking for beginning of object
// key string", the same validation a real apiserver's own decode/re-encode round trip would
// enforce before this code ever sees the object. Not worth chasing coverage on dead code.

func (suite *ResourceReconcileTestSuite) TestReconcileDeletionInstanceGoneStillClearsFinalizer() {
	now := metav1.Now()
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "r", Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{resourceFinalizer},
			Labels:            map[string]string{appsv1alpha1.InstanceRefLabel: "does-not-exist"},
		},
		Spec: rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	r := newFakeResourceReconciler(obj)
	_, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.NoError(err, "owning instance already gone must not block finalizer removal")
}

func (suite *ResourceReconcileTestSuite) TestReconcileUnknownResourceType() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}},
		Spec:       rawSpec(`{"resource_type":"not_a_real_type"}`),
	}
	r := newFakeResourceReconciler(instance, obj)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.NoError(err)

	updated := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, updated))
	suite.Equal("Error", updated.Status.Phase)
}

func (suite *ResourceReconcileTestSuite) TestReconcileInstanceNotFoundRequeues() {
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "does-not-exist"}},
		Spec:       rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	r := newFakeResourceReconciler(obj)

	res, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)
	suite.Greater(res.RequeueAfter.Seconds(), 0.0)
}

func (suite *ResourceReconcileTestSuite) TestReconcileAddsFinalizerBeforeSyncing() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}},
		Spec:       rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	r := newFakeResourceReconciler(instance, obj)

	_, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)

	updated := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, updated))
	suite.Contains(updated.Finalizers, resourceFinalizer)
	suite.Empty(updated.Status.Phase, "finalizer-add reconcile returns before touching status")
}

func (suite *ResourceReconcileTestSuite) TestReconcileSyncFailureSetsErrorAndReturnsErr() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "r", Namespace: "default",
			Finalizers: []string{resourceFinalizer},
			Labels:     map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	// A sibling Resource with unparseable spec — this actually fails rebuildResourcesConfigMap's
	// own r.List call, not the per-item parseResourceSpec inside its loop: the fake client
	// chokes unmarshaling the malformed RawExtension while building the list, before any item
	// is ever iterated. Confirmed via an isolated coverage run showing List's own error branch
	// hit and the in-loop parse-error branch still unreached. Same root cause as the
	// known-unreachable "invalid spec" Reconcile branch, one layer up — malformed RawExtension
	// breaks list-enumeration too, not just per-item decode.
	sibling := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default", Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"}},
		Spec:       rawSpec(`{not json`),
	}
	r := newFakeResourceReconciler(instance, obj, sibling)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Error(err)

	updated := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, updated))
	suite.Equal("Error", updated.Status.Phase)
}

func (suite *ResourceReconcileTestSuite) TestReconcileFirstSyncSetsPendingAndRequeues() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "r", Namespace: "default",
			Finalizers: []string{resourceFinalizer},
			Labels:     map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	r := newFakeResourceReconciler(instance, obj)
	r.Recorder = fakeRecorder()

	res, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)
	suite.InDelta(30, res.RequeueAfter.Seconds(), 1)

	updated := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, updated))
	suite.Equal("Pending", updated.Status.Phase)
	suite.False(updated.Status.Synced)
	suite.NotNil(updated.Status.LastSyncTime)
	suite.Equal(string(updated.UID), updated.Status.ID)
}

func (suite *ResourceReconcileTestSuite) TestReconcilePendingWithinWindowSkipsHealthCheck() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "r", Namespace: "default",
			Finalizers: []string{resourceFinalizer},
			Labels:     map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	r := newFakeResourceReconciler(instance, obj)
	r.Recorder = fakeRecorder()

	// First reconcile: nothing synced yet, so this sets Phase=Pending.
	_, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)

	// Second reconcile immediately after: content is unchanged (changed=false) and
	// LastSyncTime is seconds old, well within the 30s window — must requeue without ever
	// calling checkThunderIDHealth (which, given zero matching pods, would flip this straight
	// to Ready and prove the guard didn't hold).
	res, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)
	suite.Greater(res.RequeueAfter.Seconds(), 0.0)

	updated := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, updated))
	suite.Equal("Pending", updated.Status.Phase, "health check must not have run yet")
}

func (suite *ResourceReconcileTestSuite) TestReconcileUnchangedErrorRunsHealthCheck() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "r", Namespace: "default",
			Finalizers: []string{resourceFinalizer},
			Labels:     map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: rawSpec(`{"resource_type":"role","name":"x"}`),
	}
	r := newFakeResourceReconciler(instance, obj)
	r.Recorder = fakeRecorder()

	// First reconcile actually syncs the content (changed=true), landing on Phase=Pending.
	_, err := r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)

	// Flip to Error out-of-band, as a pod crash would via checkThunderIDHealth — without
	// touching spec, so the ConfigMap content is still exactly what's already been synced.
	current := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, current))
	current.Status.Phase = "Error"
	suite.Require().NoError(r.Status().Update(context.Background(), current))

	// Second reconcile: content is unchanged (changed=false), so the Phase=="Error" branch
	// must run checkThunderIDHealth directly — no matching pods means no crash found, so it
	// should flip to Ready.
	_, err = r.Reconcile(context.Background(), reconcileRequest("r", "default"))
	suite.Require().NoError(err)

	updated := &appsv1alpha1.Resource{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, updated))
	suite.Equal("Ready", updated.Status.Phase)
	suite.True(updated.Status.Synced)
}

func (suite *ResourceReconcileTestSuite) TestCheckThunderIDHealthNoPodsBecomesReady() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	obj := &appsv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Status:     appsv1alpha1.ResourceStatus{Phase: "Pending"},
	}
	r := newFakeResourceReconciler(instance, obj)
	r.Recorder = fakeRecorder()

	_, err := r.checkThunderIDHealth(context.Background(), obj, instance)
	suite.Require().NoError(err)
	suite.Equal("Ready", obj.Status.Phase)
	suite.True(obj.Status.Synced)
}

func (suite *ResourceReconcileTestSuite) TestCheckThunderIDHealthCrashNotLoopingRequeuesWithoutFetchingLogs() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-pod", Namespace: "default", Labels: map[string]string{"app": "inst"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "thunderid",
			Ready: false,
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
			},
		}}},
	}
	obj := &appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
	r := newFakeResourceReconciler(instance, pod, obj)
	r.Recorder = fakeRecorder()
	// KubeClient deliberately left nil: fetchAndEmitPodErrors would panic dereferencing it, so
	// this test only passes if the "not actually CrashLoopBackOff yet" branch really does skip
	// calling it.

	res, err := r.checkThunderIDHealth(context.Background(), obj, instance)
	suite.Require().NoError(err)
	suite.Equal("Error", obj.Status.Phase)
	suite.InDelta(10, res.RequeueAfter.Seconds(), 1)
}

func (suite *ResourceReconcileTestSuite) TestCheckThunderIDHealthCrashLoopBackOffFetchesLogs() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-pod", Namespace: "default", Labels: map[string]string{"app": "inst"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "thunderid",
			Ready: false,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	obj := &appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
	r := newFakeResourceReconciler(instance, pod, obj)
	fr := record.NewFakeRecorder(20)
	r.Recorder = fr
	// fakePods.GetLogs defaults to returning the literal "fake logs" (client-go's own stub) -
	// no "level=ERROR" marker in it, so this exercises the tail-fallback branch of
	// fetchAndEmitPodErrors, proving checkThunderIDHealth's CrashLoopBackOff path really does
	// call it (see the two focused fetchAndEmitPodErrors tests below for the other branches).
	r.KubeClient = k8sfake.NewSimpleClientset()

	res, err := r.checkThunderIDHealth(context.Background(), obj, instance)
	suite.Require().NoError(err)
	suite.Equal("Error", obj.Status.Phase)
	suite.Equal(float64(0), res.RequeueAfter.Seconds(), "CrashLoopBackOff path returns immediately, logs already handled")

	suite.Require().NotEmpty(fr.Events)
	suite.Contains(<-fr.Events, "fake logs")
}

// podLogsReactor makes a fake Clientset's Pods().GetLogs(...).Stream() return content (or fail)
// instead of client-go's own "fake logs" stub - GetLogs is implemented as an invokable Action
// with Subresource "log" (see fake_pod_expansion.go), so a plain reactor keyed on verb+resource
// alone would also catch ordinary pod Gets; filtering on GetSubresource() is what scopes it to
// just the log fetch.
var errStreamFailed = errors.New("simulated log stream failure")

func podLogsReactor(content string, err error) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, apimachineryruntime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		if err != nil {
			return true, nil, err
		}
		return true, &apimachineryruntime.Unknown{Raw: []byte(content)}, nil
	}
}

func (suite *ResourceReconcileTestSuite) TestFetchAndEmitPodErrorsStreamFailsEmitsFetchFailure() {
	obj := &appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
	r := newFakeResourceReconciler(obj)
	fr := record.NewFakeRecorder(20)
	r.Recorder = fr
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("get", "pods", podLogsReactor("", errStreamFailed))
	r.KubeClient = cs

	r.fetchAndEmitPodErrors(context.Background(), obj, "default", "crashing-pod")

	suite.Require().NotEmpty(fr.Events)
	suite.Contains(<-fr.Events, "could not fetch logs")
}

func (suite *ResourceReconcileTestSuite) TestFetchAndEmitPodErrorsParsesErrorLines() {
	obj := &appsv1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
	r := newFakeResourceReconciler(obj)
	fr := record.NewFakeRecorder(20)
	r.Recorder = fr
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("get", "pods", podLogsReactor(
		"level=INFO msg=starting\n"+
			`{"level":"ERROR","msg":"boom one"}`+"\n"+
			"level=ERROR msg=\"boom two\"\n"+
			"level=INFO msg=done\n", nil))
	r.KubeClient = cs

	r.fetchAndEmitPodErrors(context.Background(), obj, "default", "crashing-pod")

	suite.Require().Len(fr.Events, 2, "only the two ERROR lines should be emitted, not the INFO ones")
	suite.Contains(<-fr.Events, "boom one")
	suite.Contains(<-fr.Events, "boom two")
}

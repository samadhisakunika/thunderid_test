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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// configErrorReasons are container Waiting reasons that mean the container never started because
// Kubernetes couldn't resolve something the pod spec referenced — a missing (or wrong-namespace)
// Secret/ConfigMap, or an image that can't be pulled. None of these ever produce a termination
// state, since the container never ran, so the crash checks below never catch them on their own.

// reasonCrashLoopBackOff is the container Waiting reason Kubernetes uses once it's given up
// retrying a repeatedly-crashing container — checked separately from configErrorReasons since it
// means "started and crashed," not "never started."
const reasonCrashLoopBackOff = "CrashLoopBackOff"

var configErrorReasons = map[string]bool{
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
}

// podIssue is what diagnosePod found wrong with the thunderid container.
type podIssue struct {
	reason  string
	message string
}

// diagnosePod inspects pod's thunderid container for two disjoint failure shapes: one that
// started and then crashed (CrashLoopBackOff, or terminated non-zero since its last Ready — see
// the !cs.Ready comment below for why that specific check is gated the way it is), and one that
// never started at all because Kubernetes couldn't resolve something in the pod spec
// (configErrorReasons). The second shape has no termination state to inspect, so it needs its own
// branch entirely separate from the crash logic — a pod stuck on a missing envFrom Secret, for
// example, sits in Waiting with no LastTerminationState.Terminated ever set.
func diagnosePod(pod *corev1.Pod) (unhealthy bool, issue podIssue) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != thunderidContainerName {
			continue
		}
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			if reason == reasonCrashLoopBackOff || configErrorReasons[reason] {
				return true, podIssue{reason: reason, message: cs.State.Waiting.Message}
			}
		}
		// LastTerminationState reflects the most recent termination ever, not the current state —
		// it stays populated forever after a single past crash, even once the container has been
		// Ready and healthy for hours since. Gating on !cs.Ready keeps this catching the narrow
		// window right after a crash but before Kubernetes has escalated to CrashLoopBackOff,
		// without permanently flagging a fully-recovered container as still crashed.
		if !cs.Ready && cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.ExitCode != 0 {
			return true, podIssue{reason: "Crashed", message: cs.LastTerminationState.Terminated.Message}
		}
	}
	return false, podIssue{}
}

// diagnoseMissingSecret looks for the specific, easy-to-hit cause behind a CreateContainerConfigError:
// a Secret name in instance's own spec (envSecretRef, config.database.secretRef,
// config.email.secretRef) that exists somewhere in the cluster, just not in instance's namespace.
// Kubelet's own "secret \"X\" not found" message only ever means "not found in this pod's
// namespace" — it has no way to know whether X exists elsewhere, so this is the only place that
// can tell a genuinely-missing Secret apart from one that's simply in the wrong namespace.
func diagnoseMissingSecret(ctx context.Context, c client.Client, instance *appsv1alpha1.ThunderIDInstance, issue podIssue) string {
	if issue.reason != "CreateContainerConfigError" {
		return ""
	}

	var names []string
	if instance.Spec.EnvSecretRef != "" {
		names = append(names, instance.Spec.EnvSecretRef)
	}
	if instance.Spec.Config.Database != nil && instance.Spec.Config.Database.SecretRef != "" {
		names = append(names, instance.Spec.Config.Database.SecretRef)
	}
	if instance.Spec.Config.Email != nil && instance.Spec.Config.Email.SecretRef != "" {
		names = append(names, instance.Spec.Config.Email.SecretRef)
	}

	for _, name := range names {
		local := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: name}, local); err == nil {
			continue // exists right where it should - not the problem
		}
		all := &corev1.SecretList{}
		if err := c.List(ctx, all); err != nil {
			continue
		}
		for _, s := range all.Items {
			if s.Name == name && s.Namespace != instance.Namespace {
				return fmt.Sprintf(
					"Secret %q not found in namespace %q, but a Secret with that name exists in namespace %q — Secrets must be in the same namespace as the instance (check envSecretRef, database.secretRef, email.secretRef)",
					name, instance.Namespace, s.Namespace)
			}
		}
	}
	return ""
}

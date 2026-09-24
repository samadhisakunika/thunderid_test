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
	"encoding/hex"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// findEnvironment returns the single EnvironmentValues in instance's namespace labeled
// thunderid.io/instance=<instance.Name> (see InstanceRefLabel's doc comment), or nil if none
// exists — an instance with no such EnvironmentValues simply has no env Secret. More than one is a
// config error: envSecretRef used to be a single field, and silently picking one of several
// candidates would hide a real misconfiguration instead of surfacing it.
func findEnvironment(ctx context.Context, c client.Client, instance *appsv1alpha1.ThunderIDInstance) (*appsv1alpha1.EnvironmentValues, error) {
	list := &appsv1alpha1.EnvironmentValuesList{}
	if err := c.List(ctx, list, client.InNamespace(instance.Namespace),
		client.MatchingLabels{appsv1alpha1.InstanceRefLabel: instance.Name}); err != nil {
		return nil, err
	}
	switch len(list.Items) {
	case 0:
		return nil, nil
	case 1:
		return &list.Items[0], nil
	default:
		names := make([]string, len(list.Items))
		for i, e := range list.Items {
			names[i] = e.Name
		}
		return nil, fmt.Errorf("multiple EnvironmentValues objects labeled %s=%s: %v", appsv1alpha1.InstanceRefLabel, instance.Name, names)
	}
}

// resolveEnvSecretHash returns "" when no EnvironmentValues is labeled for instance (see
// findEnvironment), or a hash of its Secret's content otherwise — folded into configHash so the
// pod restarts (and re-resolves every {{.VAR}} token in resources.yaml) whenever the Secret's
// values change, the same way editing spec.config.database/email forces a restart. Errors if the
// label resolves ambiguously or the Secret itself is missing, surfaced as an InvalidSpec event
// rather than crash-looping the pod.
func (r *ThunderIDInstanceReconciler) resolveEnvSecretHash(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) (string, error) {
	env, err := findEnvironment(ctx, r.Client, instance)
	if err != nil {
		return "", err
	}
	if env == nil {
		return "", nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: env.Name, Namespace: instance.Namespace,
	}, secret); err != nil {
		return "", fmt.Errorf("environment %q's Secret: %w", env.Name, err)
	}

	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write(secret.Data[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

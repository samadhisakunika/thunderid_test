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

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// resolveEnvSecretHash returns "" when spec.envSecretRef is unset, or a hash of the named
// Secret's content otherwise — folded into configHash so the pod restarts (and re-resolves every
// {{.VAR}} token in resources.yaml) whenever the Secret's values change, the same way editing
// spec.config.database/email forces a restart today. Errors if the Secret is set but missing,
// surfaced as an InvalidSpec event rather than crash-looping the pod.
func (r *ThunderIDInstanceReconciler) resolveEnvSecretHash(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) (string, error) {
	if instance.Spec.EnvSecretRef == "" {
		return "", nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: instance.Spec.EnvSecretRef, Namespace: instance.Namespace,
	}, secret); err != nil {
		return "", fmt.Errorf("spec.envSecretRef %q: %w", instance.Spec.EnvSecretRef, err)
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

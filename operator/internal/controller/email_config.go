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
	"k8s.io/apimachinery/pkg/types"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const (
	emailSecretKeyPassword = "password"

	// emailPasswordMountPath is where the password key of spec.config.email.secretRef is mounted into
	// the container. Referenced via a file:// URI, the same pattern deployment.yaml already uses
	// for direct_auth_secret, crypto.key, and the database password.
	emailPasswordMountPath = "config/secrets/email_password"

	// emailBlockMarkerLine is the placeholder line in deployment-config.yaml that
	// reconcileConfigMap replaces with the resolved email: section, or removes entirely when
	// spec.config.email is unset — matching the image's own out-of-the-box behavior of no email config.
	emailBlockMarkerLine = `__EMAIL_BLOCK__: placeholder # replaced whole-line by reconcileConfigMap with the email: section, or removed if spec.config.email is unset`
)

// resolveEmailConfig returns the YAML to substitute for emailBlockMarkerLine — "" when
// spec.config.email is unset, so the template ends up with no email: section at all. The password (when
// spec.config.email.smtp.enableAuthentication is set) is kept out of the ConfigMap and resolved at
// runtime via a file:// reference, the same way resolveDatabaseConfig handles the database password.
// secretName is the Secret actually holding that password — spec.config.email.secretRef if set,
// else the smtp section of the Environment labeled for this instance, "" if authentication is
// disabled.
func (r *ThunderIDInstanceReconciler) resolveEmailConfig(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) (block string, secretName string, err error) {
	email := instance.Spec.Config.Email
	if email == nil {
		return "", "", nil
	}

	password := ""
	if email.SMTP.EnableAuthentication {
		resolvedRef, err := resolveSectionSecretRef(ctx, r.Client, instance, email.SecretRef,
			func(e *appsv1alpha1.Environment) appsv1alpha1.EnvSection { return e.Spec.Smtp }, "smtp")
		if err != nil {
			return "", "", err
		}
		if resolvedRef == "" {
			return "", "", fmt.Errorf("spec.config.email.smtp.enableAuthentication is true but spec.config.email.secretRef is not set and no Environment labeled %s=%s has a smtp section",
				appsv1alpha1.InstanceRefLabel, instance.Name)
		}
		// Error messages name whichever source actually supplied resolvedRef, so a misconfigured
		// explicit field and a misconfigured Environment point the user at the right place to fix it.
		source := fmt.Sprintf("spec.config.email.secretRef %q", resolvedRef)
		if email.SecretRef == "" {
			source = fmt.Sprintf("Environment's smtp Secret %q", resolvedRef)
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: resolvedRef, Namespace: instance.Namespace,
		}, secret); err != nil {
			return "", "", fmt.Errorf("%s: %w", source, err)
		}
		if string(secret.Data[emailSecretKeyPassword]) == "" {
			return "", "", fmt.Errorf("%s is missing required key: password", source)
		}
		password = "file://" + emailPasswordMountPath
		secretName = resolvedRef
	}

	block = fmt.Sprintf(`email:
  smtp:
    host: %q
    port: %d
    username: %q
    password: %q
    from_address: %q
    enable_start_tls: %t
    enable_authentication: %t`,
		email.SMTP.Host, email.SMTP.Port, email.SMTP.Username, password, email.SMTP.FromAddress,
		email.SMTP.EnableStartTLS, email.SMTP.EnableAuthentication)

	return block, secretName, nil
}

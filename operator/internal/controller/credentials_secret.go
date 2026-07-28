package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// ensureCredentialsSecret resolves the Secret backing a User's credentials.
//
// If spec.credentialsSecretRef is set, that Secret is used as-is and must already
// exist. Otherwise a Secret named "<user>-credentials" is generated on first use with
// a random username/password, owned by the User. Like reconcileSecuritySecret, an
// auto-generated Secret is deliberately never regenerated on later reconciles, so a
// user's password doesn't rotate out from under them.
func (r *UserReconciler) ensureCredentialsSecret(ctx context.Context, u appsv1alpha1.User) (name string, created bool, err error) {
	if u.Spec.CredentialsSecretRef != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: u.Namespace, Name: u.Spec.CredentialsSecretRef}, secret); err != nil {
			if errors.IsNotFound(err) {
				return "", false, fmt.Errorf("spec.credentialsSecretRef %q not found", u.Spec.CredentialsSecretRef)
			}
			return "", false, err
		}
		return u.Spec.CredentialsSecretRef, false, nil
	}

	name = u.Name + "-credentials"
	existing := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: u.Namespace, Name: name}, existing); err == nil {
		return name, false, nil
	} else if !errors.IsNotFound(err) {
		return "", false, err
	}

	username, err := randomUsername()
	if err != nil {
		return "", false, fmt.Errorf("generating username: %w", err)
	}
	password, err := randomPassword()
	if err != nil {
		return "", false, fmt.Errorf("generating password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: u.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": username,
			"password": password,
		},
	}
	_ = ctrl.SetControllerReference(&u, secret, r.Scheme)
	if err := r.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			return name, false, nil
		}
		return "", false, err
	}
	return name, true, nil
}

// randomUsername generates a short, readable, random username for an
// auto-provisioned User credentials Secret.
func randomUsername() ([]byte, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	return append([]byte("user-"), suffix...), nil
}

// randomPassword generates a short, random password for an auto-provisioned User
// credentials Secret — meant to be typed during initial login, not cryptographic
// key material, so it's deliberately much shorter than randomHexSecret's 32 bytes.
func randomPassword() ([]byte, error) {
	return randomHex(4)
}

// randomHex returns n random bytes, hex-encoded (2n characters).
func randomHex(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return []byte(hex.EncodeToString(buf)), nil
}

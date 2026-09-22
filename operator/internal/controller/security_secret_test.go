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
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
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

type SecuritySecretTestSuite struct {
	suite.Suite
}

func TestSecuritySecretTestSuite(t *testing.T) {
	suite.Run(t, new(SecuritySecretTestSuite))
}

func (suite *SecuritySecretTestSuite) TestGenerateSelfSignedCertRSA() {
	certPEM, keyPEM, err := generateSelfSignedCert("rsa")
	suite.Require().NoError(err)

	certBlock, _ := pem.Decode(certPEM)
	suite.Require().NotNil(certBlock)
	suite.Equal("CERTIFICATE", certBlock.Type)
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	suite.Require().NoError(err)
	suite.Equal("localhost", cert.Subject.CommonName)

	keyBlock, _ := pem.Decode(keyPEM)
	suite.Require().NotNil(keyBlock)
	suite.Equal("PRIVATE KEY", keyBlock.Type)
	_, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	suite.NoError(err, "generated RSA key must parse as PKCS8")
}

func (suite *SecuritySecretTestSuite) TestGenerateSelfSignedCertECDSA() {
	certPEM, keyPEM, err := generateSelfSignedCert("ecdsa")
	suite.Require().NoError(err)

	certBlock, _ := pem.Decode(certPEM)
	suite.Require().NotNil(certBlock)
	_, err = x509.ParseCertificate(certBlock.Bytes)
	suite.NoError(err)

	keyBlock, _ := pem.Decode(keyPEM)
	suite.Require().NotNil(keyBlock)
	suite.Equal("EC PRIVATE KEY", keyBlock.Type)
	_, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	suite.NoError(err, "generated ECDSA key must parse as SEC1")
}

func (suite *SecuritySecretTestSuite) TestRandomHexSecret() {
	a, err := randomHexSecret()
	suite.Require().NoError(err)
	_, err = hex.DecodeString(string(a))
	suite.NoError(err, "randomHexSecret must be valid hex")
	suite.Len(a, 64, "32 bytes hex-encoded")
	suite.False(hasSuffixNewline(a), "matches setup.sh's printf '%%s' - no trailing newline")

	b, err := randomHexSecret()
	suite.Require().NoError(err)
	suite.NotEqual(string(a), string(b))
}

func hasSuffixNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretGeneratesOnce() {
	r := newFakeReconciler()
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}

	name, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal("inst-security", name)

	secret := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, secret))
	for _, key := range securityAllKeys {
		suite.NotEmpty(secret.Data[key], "key %q", key)
	}
	firstResourceVersion := secret.ResourceVersion

	// Second reconcile must not touch the Secret: the JWT signing key (and already-issued
	// tokens) must stay stable across reconciles, not just across pod restarts.
	name2, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal(name, name2)

	secret2 := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, secret2))
	suite.Equal(firstResourceVersion, secret2.ResourceVersion, "Secret must not be modified on second reconcile")
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretUserSuppliedValid() {
	data := map[string][]byte{}
	for _, key := range securityAllKeys {
		data[key] = []byte("provided-" + key)
	}
	provided := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-security", Namespace: "default"},
		Data:       data,
	}
	r := newFakeReconciler(provided)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{SecuritySecret: "my-security"},
	}

	name, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal("my-security", name, "used as-is, not regenerated")
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretUserSuppliedMissingKey() {
	provided := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "incomplete-security", Namespace: "default"},
		Data:       map[string][]byte{securityKeyServerCert: []byte("x")},
	}
	r := newFakeReconciler(provided)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{SecuritySecret: "incomplete-security"},
	}

	_, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().Error(err)
	suite.Contains(err.Error(), "missing required key")
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretEnvironmentFallback() {
	data := map[string][]byte{}
	for _, key := range securityAllKeys {
		data[key] = []byte("provided-" + key)
	}
	provided := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "env-security", Namespace: "default"},
		Data:       data,
	}
	env := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env", Namespace: "default",
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: appsv1alpha1.EnvironmentSpec{Security: appsv1alpha1.EnvSection{SecretRef: "env-security"}},
	}
	r := newFakeReconciler(provided, env)
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}

	name, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal("env-security", name, "spec.securitySecret unset - falls back to the security section of the Environment instead of auto-generating")
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretExplicitFieldWinsOverEnvironment() {
	explicitData := map[string][]byte{}
	for _, key := range securityAllKeys {
		explicitData[key] = []byte("explicit-" + key)
	}
	explicit := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "explicit-security", Namespace: "default"},
		Data:       explicitData,
	}
	env := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env", Namespace: "default",
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: appsv1alpha1.EnvironmentSpec{Security: appsv1alpha1.EnvSection{SecretRef: "env-security"}},
	}
	r := newFakeReconciler(explicit, env)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{SecuritySecret: "explicit-security"},
	}

	name, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal("explicit-security", name)
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretUserSuppliedNotFound() {
	r := newFakeReconciler()
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{SecuritySecret: "does-not-exist"},
	}

	_, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().Error(err)
	suite.Contains(err.Error(), "not found")
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretUserSuppliedGetErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{SecuritySecret: "my-security"},
	}
	wantErr := errors.New("simulated get failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.Secret); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)

	_, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *SecuritySecretTestSuite) TestReconcileSecuritySecretAutoGeneratedGetErrorPropagates() {
	instance := &appsv1alpha1.ThunderIDInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"}}
	wantErr := errors.New("simulated get failure")
	r := newFakeReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.Secret); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, instance)

	_, err := r.reconcileSecuritySecret(context.Background(), instance)
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

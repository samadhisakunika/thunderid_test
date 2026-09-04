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
	"regexp"
	"testing"

	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

type EnvSecretTestSuite struct {
	suite.Suite
}

func TestEnvSecretTestSuite(t *testing.T) {
	suite.Run(t, new(EnvSecretTestSuite))
}

func (suite *EnvSecretTestSuite) TestResolveEnvSecretHashUnset() {
	r := newFakeReconciler()
	instance := &appsv1alpha1.ThunderIDInstance{}
	hash, err := r.resolveEnvSecretHash(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Empty(hash)
}

func (suite *EnvSecretTestSuite) TestResolveEnvSecretHashMissingSecret() {
	r := newFakeReconciler()
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "missing-secret"},
	}
	_, err := r.resolveEnvSecretHash(context.Background(), instance)
	suite.Error(err)
}

func (suite *EnvSecretTestSuite) TestResolveEnvSecretHashStableFormat() {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "env-secret", Namespace: "default"},
		Data:       map[string][]byte{"A": []byte("1"), "B": []byte("2")},
	}
	r := newFakeReconciler(secret)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "env-secret"},
	}
	hash, err := r.resolveEnvSecretHash(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Regexp(regexp.MustCompile(`^[0-9a-f]{16}$`), hash)
}

func (suite *EnvSecretTestSuite) TestResolveEnvSecretHashChangesWithContent() {
	build := func(data map[string][]byte) *ThunderIDInstanceReconciler {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "env-secret", Namespace: "default"},
			Data:       data,
		}
		return newFakeReconciler(secret)
	}
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       appsv1alpha1.ThunderIDInstanceSpec{EnvSecretRef: "env-secret"},
	}

	r1 := build(map[string][]byte{"KEY": []byte("value1")})
	hash1, err := r1.resolveEnvSecretHash(context.Background(), instance)
	suite.Require().NoError(err)

	r2 := build(map[string][]byte{"KEY": []byte("value2")})
	hash2, err := r2.resolveEnvSecretHash(context.Background(), instance)
	suite.Require().NoError(err)

	suite.NotEqual(hash1, hash2)

	r3 := build(map[string][]byte{"KEY": []byte("value1")})
	hash3, err := r3.resolveEnvSecretHash(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal(hash1, hash3)
}

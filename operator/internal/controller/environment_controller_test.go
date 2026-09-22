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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

type EnvironmentControllerTestSuite struct {
	suite.Suite
}

func TestEnvironmentControllerTestSuite(t *testing.T) {
	suite.Run(t, new(EnvironmentControllerTestSuite))
}

func (suite *EnvironmentControllerTestSuite) TestIsMasked() {
	suite.True(isMasked(maskValue("hunter2")))
	suite.False(isMasked("hunter2"))
	suite.False(isMasked("thunderid-masked:looks-close-but-no-len-suffix"))
}

func (suite *EnvironmentControllerTestSuite) TestMaskValueRevealsAtMostFourChars() {
	cases := []struct {
		name       string
		in         string
		wantPrefix string
	}{
		{"long value", "supersecret", "thunderid-masked:supe*******:len=11"},
		{"exactly 4 chars", "abcd", "thunderid-masked:abc*:len=4"},
		{"3 chars", "abc", "thunderid-masked:ab*:len=3"},
		{"empty", "", "thunderid-masked::len=0"},
	}
	for _, c := range cases {
		suite.Run(c.name, func() {
			got := maskValue(c.in)
			suite.Equal(c.wantPrefix, got)
			suite.True(isMasked(got), "maskValue's own output must satisfy isMasked")
			// The full plaintext must never appear verbatim once masked (for anything longer
			// than the revealed prefix).
			if len(c.in) > 4 {
				suite.NotContains(got, c.in)
			}
		})
	}
}

func (suite *EnvironmentControllerTestSuite) TestManagedKeySet() {
	withAnnotation := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{managedKeysAnnotation: "A,B,C"},
	}}
	suite.Equal(map[string]bool{"A": true, "B": true, "C": true}, managedKeySet(withAnnotation))

	noAnnotation := &corev1.Secret{}
	suite.Empty(managedKeySet(noAnnotation))
}

func (suite *EnvironmentControllerTestSuite) TestSyncSectionSecretNewPlaintextCreatesAndMasks() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec:       appsv1alpha1.EnvironmentSpec{Credentials: appsv1alpha1.EnvSection{Env: map[string]string{"PASSWORD": "hunter2"}}},
	}
	r := newFakeEnvironmentReconciler(obj)

	keys, maskedEnv, maskedAny, err := r.syncSectionSecret(context.Background(), obj, "env-credentials", obj.Spec.Credentials.Env)
	suite.Require().NoError(err)
	suite.True(maskedAny)
	suite.Equal([]string{"PASSWORD"}, keys)
	suite.True(isMasked(maskedEnv["PASSWORD"]), "returned maskedEnv must hold the masked marker, not the plaintext")

	secret := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env-credentials", Namespace: "default"}, secret))
	suite.Equal("hunter2", string(secret.Data["PASSWORD"]))

	// syncSectionSecret itself never mutates obj.Spec - only Reconcile's maskSpec call does that,
	// across every section in one Update.
	suite.Equal("hunter2", obj.Spec.Credentials.Env["PASSWORD"])
}

func (suite *EnvironmentControllerTestSuite) TestSyncSectionSecretAlreadyMaskedIsNoop() {
	masked := maskValue("hunter2")
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
	}
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env-credentials", Namespace: "default",
			Annotations: map[string]string{managedKeysAnnotation: "PASSWORD"},
		},
		Data: map[string][]byte{"PASSWORD": []byte("hunter2")},
	}
	r := newFakeEnvironmentReconciler(obj, existingSecret)

	keys, maskedEnv, maskedAny, err := r.syncSectionSecret(context.Background(), obj, "env-credentials", map[string]string{"PASSWORD": masked})
	suite.Require().NoError(err)
	suite.False(maskedAny, "an already-masked key must not be treated as new plaintext")
	suite.Equal([]string{"PASSWORD"}, keys)
	suite.Equal(masked, maskedEnv["PASSWORD"], "the masked marker is carried through untouched")
}

func (suite *EnvironmentControllerTestSuite) TestSyncSectionSecretDropsPreviouslyManagedKey() {
	obj := &appsv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"}}
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env-database", Namespace: "default",
			Annotations: map[string]string{managedKeysAnnotation: "KEEP,DROPPED"},
		},
		Data: map[string][]byte{"KEEP": []byte("old-value"), "DROPPED": []byte("gone-now")},
	}
	r := newFakeEnvironmentReconciler(obj, existingSecret)

	_, _, _, err := r.syncSectionSecret(context.Background(), obj, "env-database", map[string]string{"KEEP": "new-value"})
	suite.Require().NoError(err)

	secret := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env-database", Namespace: "default"}, secret))
	suite.Equal("new-value", string(secret.Data["KEEP"]))
	suite.NotContains(secret.Data, "DROPPED")
}

func (suite *EnvironmentControllerTestSuite) TestSyncSectionSecretLeavesForeignKeysAlone() {
	obj := &appsv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"}}
	// A hand-made Secret sharing this section's target name, never written by this controller
	// (no managedKeysAnnotation) - its keys must survive the sync untouched.
	handMade := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-name", Namespace: "default"},
		Data:       map[string][]byte{"FOREIGN": []byte("do-not-touch")},
	}
	r := newFakeEnvironmentReconciler(obj, handMade)
	r.Recorder = fakeRecorder() // adopting an unmanaged Secret emits an AdoptedExistingSecret event

	_, _, _, err := r.syncSectionSecret(context.Background(), obj, "shared-name", map[string]string{"MINE": "value"})
	suite.Require().NoError(err)

	secret := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "shared-name", Namespace: "default"}, secret))
	suite.Equal("do-not-touch", string(secret.Data["FOREIGN"]))
	suite.Equal("value", string(secret.Data["MINE"]))
}

func (suite *EnvironmentControllerTestSuite) TestUpsertSecretStampsInstanceLabel() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env", Namespace: "default",
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "myinstance"},
		},
	}
	r := newFakeEnvironmentReconciler(obj)

	suite.Require().NoError(r.upsertSecret(context.Background(), obj, "env-security", false, nil, map[string][]byte{"crypto.key": []byte("x")}))

	secret := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env-security", Namespace: "default"}, secret))
	suite.Equal("myinstance", secret.Labels[appsv1alpha1.InstanceRefLabel],
		"generated Secret must carry the Environment's own instance label, so a plain Secret watch can map back to the instance")
}

func (suite *EnvironmentControllerTestSuite) TestUpsertSecretNoLabelWhenEnvironmentUnlabeled() {
	obj := &appsv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"}}
	r := newFakeEnvironmentReconciler(obj)

	suite.Require().NoError(r.upsertSecret(context.Background(), obj, "env-credentials", false, nil, map[string][]byte{"A": []byte("b")}))

	secret := &corev1.Secret{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env-credentials", Namespace: "default"}, secret))
	suite.Empty(secret.Labels[appsv1alpha1.InstanceRefLabel])
}

func (suite *EnvironmentControllerTestSuite) TestReconcileSetsSyncedStatus() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec:       appsv1alpha1.EnvironmentSpec{Credentials: appsv1alpha1.EnvSection{Env: map[string]string{"A": "b"}}},
	}
	r := newFakeEnvironmentReconciler(obj)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().NoError(err)

	updated := &appsv1alpha1.Environment{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env", Namespace: "default"}, updated))
	suite.Equal("Synced", updated.Status.Phase)
	suite.True(updated.Status.Synced)
	suite.Equal(map[string]string{"credentials": "env-credentials"}, updated.Status.SecretNames)
	suite.Equal(map[string][]string{"credentials": {"A"}}, updated.Status.Keys)
	suite.True(isMasked(updated.Spec.Credentials.Env["A"]), "plaintext must be masked in spec after sync")
}

func (suite *EnvironmentControllerTestSuite) TestReconcileMultipleSectionsProduceSeparateSecrets() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec: appsv1alpha1.EnvironmentSpec{
			Credentials: appsv1alpha1.EnvSection{Env: map[string]string{"APP_SECRET": "s1"}},
			Security:    appsv1alpha1.EnvSection{Env: map[string]string{"crypto.key": "s2"}},
			Database:    appsv1alpha1.EnvSection{Env: map[string]string{"password": "s3"}},
		},
	}
	r := newFakeEnvironmentReconciler(obj)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().NoError(err)

	for name, key := range map[string]string{"env-credentials": "APP_SECRET", "env-security": "crypto.key", "env-database": "password"} {
		secret := &corev1.Secret{}
		suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, secret), "secret %q must exist", name)
		suite.NotEmpty(secret.Data[key], "secret %q must hold key %q", name, key)
	}

	updated := &appsv1alpha1.Environment{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env", Namespace: "default"}, updated))
	suite.Equal(map[string]string{
		"credentials": "env-credentials",
		"security":    "env-security",
		"database":    "env-database",
	}, updated.Status.SecretNames)
	suite.NotContains(updated.Status.SecretNames, "smtp", "a section with neither Env nor SecretRef set must not appear in status")
}

func (suite *EnvironmentControllerTestSuite) TestReconcileSectionWithSecretRefIsNotGenerated() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec:       appsv1alpha1.EnvironmentSpec{Security: appsv1alpha1.EnvSection{SecretRef: "externally-managed-secret"}},
	}
	r := newFakeEnvironmentReconciler(obj)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().NoError(err)

	suite.Error(r.Get(context.Background(), types.NamespacedName{Name: "env-security", Namespace: "default"}, &corev1.Secret{}),
		"a SecretRef section must never generate its own Secret")

	updated := &appsv1alpha1.Environment{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env", Namespace: "default"}, updated))
	suite.Equal("externally-managed-secret", updated.Status.SecretNames["security"])
	suite.NotContains(updated.Status.Keys, "security", "an unmanaged SecretRef section's keys are never listed - its content isn't ours to know")
}

// TestReconcileSyncFailureSetsErrorStatus exercises Reconcile's error branch, which only fires
// when the underlying Secret write genuinely fails — not something the plain fake client will
// ever do on its own for a well-formed request. WithInterceptorFuncs (controller-runtime's
// pkg/client/interceptor) wraps the fake client so a specific call can be made to fail on
// command, the same pattern used across the ecosystem for exactly this: forcing an
// infra-failure branch without needing a real API server to actually break.
func (suite *EnvironmentControllerTestSuite) TestReconcileSyncFailureSetsErrorStatus() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec:       appsv1alpha1.EnvironmentSpec{Credentials: appsv1alpha1.EnvSection{Env: map[string]string{"A": "b"}}},
	}

	scheme := runtime.NewScheme()
	suite.Require().NoError(corev1.AddToScheme(scheme))
	suite.Require().NoError(appsv1alpha1.AddToScheme(scheme))

	wantErr := errors.New("simulated Secret create failure")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1alpha1.Environment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if _, ok := o.(*corev1.Secret); ok {
					return wantErr
				}
				return c.Create(ctx, o, opts...)
			},
		}).
		WithRuntimeObjects(obj).
		Build()

	r := &EnvironmentReconciler{Client: fakeClient, Scheme: scheme, Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)

	updated := &appsv1alpha1.Environment{}
	suite.Require().NoError(r.Get(context.Background(), types.NamespacedName{Name: "env", Namespace: "default"}, updated))
	suite.Equal("Error", updated.Status.Phase)
	suite.False(updated.Status.Synced)
}

func (suite *EnvironmentControllerTestSuite) TestReconcileGetErrorPropagates() {
	wantErr := errors.New("simulated get failure")
	r := newFakeEnvironmentReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*appsv1alpha1.Environment); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	})

	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *EnvironmentControllerTestSuite) TestReconcileSuccessStatusUpdateErrorPropagates() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec:       appsv1alpha1.EnvironmentSpec{Credentials: appsv1alpha1.EnvSection{Env: map[string]string{"A": "b"}}},
	}
	wantErr := errors.New("simulated status update failure")
	r := newFakeEnvironmentReconcilerWithInterceptor(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				return wantErr
			}
			return c.Status().Update(ctx, o, opts...)
		},
	}, obj)
	r.Recorder = fakeRecorder()

	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *EnvironmentControllerTestSuite) TestSyncSectionSecretGetErrorPropagates() {
	obj := &appsv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"}}
	wantErr := errors.New("simulated secret get failure")
	r := newFakeEnvironmentReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.Secret); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, obj)

	_, _, _, err := r.syncSectionSecret(context.Background(), obj, "env-credentials", map[string]string{"A": "b"})
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *EnvironmentControllerTestSuite) TestReconcileMaskSpecErrorPropagates() {
	obj := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"},
		Spec:       appsv1alpha1.EnvironmentSpec{Credentials: appsv1alpha1.EnvSection{Env: map[string]string{"A": "new-plaintext"}}},
	}
	wantErr := errors.New("simulated environment update failure")
	r := newFakeEnvironmentReconcilerWithInterceptor(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			if _, ok := o.(*appsv1alpha1.Environment); ok {
				return wantErr
			}
			return c.Update(ctx, o, opts...)
		},
	}, obj)
	r.Recorder = fakeRecorder()

	// New plaintext drives Reconcile into calling maskSpec once, after every section's Secret is
	// synced - whose own Update is what's intercepted here.
	_, err := r.Reconcile(context.Background(), reconcileRequest("env"))
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *EnvironmentControllerTestSuite) TestUpsertSecretRetryGetErrorPropagates() {
	obj := &appsv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"}}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env-credentials", Namespace: "default",
			Annotations: map[string]string{managedKeysAnnotation: "A"},
		},
		Data: map[string][]byte{"A": []byte("old")},
	}
	wantErr := errors.New("simulated retry-get failure")
	r := newFakeEnvironmentReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.Secret); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, obj, existing)

	// Called directly (not via syncSectionSecret) so this interceptor's Get override only ever
	// sees upsertSecret's own retry-loop Get, not some earlier unrelated one.
	err := r.upsertSecret(context.Background(), obj, "env-credentials", true, existing, map[string][]byte{"A": []byte("new")})
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

func (suite *EnvironmentControllerTestSuite) TestMaskSpecRetryGetErrorPropagates() {
	obj := &appsv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "default"}}
	wantErr := errors.New("simulated retry-get failure")
	r := newFakeEnvironmentReconcilerWithInterceptor(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*appsv1alpha1.Environment); ok {
				return wantErr
			}
			return c.Get(ctx, key, o, opts...)
		},
	}, obj)

	err := r.maskSpec(context.Background(), obj, map[string]map[string]string{"credentials": {"A": "masked"}})
	suite.Require().Error(err)
	suite.ErrorIs(err, wantErr)
}

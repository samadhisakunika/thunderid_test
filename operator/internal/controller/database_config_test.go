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
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

type DatabaseConfigTestSuite struct {
	suite.Suite
}

func TestDatabaseConfigTestSuite(t *testing.T) {
	suite.Run(t, new(DatabaseConfigTestSuite))
}

func (suite *DatabaseConfigTestSuite) TestUsingPostgres() {
	pg := appsv1alpha1.DatabaseBackendSpec{Type: "postgres"}
	sqlite := appsv1alpha1.DatabaseBackendSpec{Type: "sqlite"}

	cases := []struct {
		name string
		db   *appsv1alpha1.DatabaseSpec
		want bool
	}{
		{"nil database", nil, false},
		{"unset (all default sqlite)", &appsv1alpha1.DatabaseSpec{}, false},
		{"all four postgres", &appsv1alpha1.DatabaseSpec{Config: pg, RuntimeTransient: pg, Entity: pg, RuntimePersistent: pg}, true},
		{"mixed sqlite and postgres", &appsv1alpha1.DatabaseSpec{Config: pg, RuntimeTransient: sqlite, Entity: pg, RuntimePersistent: pg}, false},
		{"three of four postgres", &appsv1alpha1.DatabaseSpec{Config: pg, RuntimeTransient: pg, Entity: pg}, false},
	}
	for _, c := range cases {
		suite.Run(c.name, func() {
			instance := &appsv1alpha1.ThunderIDInstance{Spec: appsv1alpha1.ThunderIDInstanceSpec{
				Config: appsv1alpha1.AppConfigSpec{Database: c.db},
			}}
			suite.Equal(c.want, usingPostgres(instance))
		})
	}
}

func (suite *DatabaseConfigTestSuite) TestScopeJSONName() {
	cases := map[string]string{
		"runtime_transient":  "runtimeTransient",
		"runtime_persistent": "runtimePersistent",
		"config":             "config",
		"entity":             "entity",
	}
	for in, want := range cases {
		suite.Equal(want, scopeJSONName(in))
	}
}

func (suite *DatabaseConfigTestSuite) TestRenderDatabaseScopeSQLiteDefaults() {
	got, err := renderDatabaseScope("config", appsv1alpha1.DatabaseBackendSpec{})
	suite.Require().NoError(err)
	suite.Contains(got, `type: "sqlite"`)
	suite.Contains(got, `path: "repository/database/configdb.db"`)
	suite.Contains(got, "max_open_conns: 500")
}

func (suite *DatabaseConfigTestSuite) TestRenderDatabaseScopeSQLiteOverrides() {
	got, err := renderDatabaseScope("entity", appsv1alpha1.DatabaseBackendSpec{
		SQLite: &appsv1alpha1.SQLiteBackendSpec{
			Path: "/custom/path.db", Options: "_journal_mode=OFF", MaxOpenConns: 10, MaxIdleConns: 5, ConnMaxLifetime: 60,
		},
	})
	suite.Require().NoError(err)
	suite.Contains(got, `path: "/custom/path.db"`)
	suite.Contains(got, `options: "_journal_mode=OFF"`)
	suite.Contains(got, "max_open_conns: 10")
	suite.Contains(got, "max_idle_conns: 5")
	suite.Contains(got, "conn_max_lifetime: 60")
}

func (suite *DatabaseConfigTestSuite) TestRenderDatabaseScopeSQLiteDefaultOptionsWhenUnset() {
	got, err := renderDatabaseScope("entity", appsv1alpha1.DatabaseBackendSpec{
		SQLite: &appsv1alpha1.SQLiteBackendSpec{Path: "/custom/path.db"},
	})
	suite.Require().NoError(err)
	// Options was not overridden, so it must still fall back to the default.
	suite.Contains(got, "_journal_mode=WAL")
}

func (suite *DatabaseConfigTestSuite) TestRenderDatabaseScopePostgresRequiresFields() {
	_, err := renderDatabaseScope("config", appsv1alpha1.DatabaseBackendSpec{
		Type:     "postgres",
		Postgres: &appsv1alpha1.PostgresBackendSpec{},
	})
	suite.Require().Error(err)
	for _, field := range []string{"postgres.hostname", "postgres.username", "postgres.name"} {
		suite.Contains(err.Error(), field)
	}
}

func (suite *DatabaseConfigTestSuite) TestRenderDatabaseScopePostgresMissingBlock() {
	_, err := renderDatabaseScope("config", appsv1alpha1.DatabaseBackendSpec{Type: "postgres"})
	suite.Error(err)
}

func (suite *DatabaseConfigTestSuite) TestRenderDatabaseScopePostgresDefaults() {
	got, err := renderDatabaseScope("config", appsv1alpha1.DatabaseBackendSpec{
		Type: "postgres",
		Postgres: &appsv1alpha1.PostgresBackendSpec{
			Hostname: "db.example.com",
			Username: "thunderid",
			Name:     "thunderid_config",
		},
	})
	suite.Require().NoError(err)
	suite.Contains(got, `hostname: "db.example.com"`)
	suite.Contains(got, `port: 5432`)
	suite.Contains(got, `username: "thunderid"`)
	suite.Contains(got, `password: "file://`+databasePasswordMountPath+`"`)
	suite.Contains(got, `sslmode: "require"`)
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigUnset() {
	r := newFakeReconciler()
	instance := &appsv1alpha1.ThunderIDInstance{}
	block, secretName, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Empty(block)
	suite.Empty(secretName)
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigPostgresMissingSecretRef() {
	r := newFakeReconciler()
	instance := &appsv1alpha1.ThunderIDInstance{Spec: appsv1alpha1.ThunderIDInstanceSpec{
		Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
			Config: appsv1alpha1.DatabaseBackendSpec{Type: "postgres", Postgres: &appsv1alpha1.PostgresBackendSpec{
				Hostname: "h", Username: "u", Name: "n",
			}},
		}},
	}}
	_, _, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().Error(err)
	suite.Contains(err.Error(), "secretRef")
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigEnvironmentFallback() {
	// The database section's Env, not SecretRef - exercises the generated-Secret naming
	// convention ("<environment-name>-database") end to end, rather than a hand-named passthrough.
	env := &appsv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env", Namespace: "default",
			Labels: map[string]string{appsv1alpha1.InstanceRefLabel: "inst"},
		},
		Spec: appsv1alpha1.EnvironmentSpec{Database: appsv1alpha1.EnvSection{Env: map[string]string{"password": "hunter2"}}},
	}
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "env-database", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	r := newFakeReconciler(env, dbSecret)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
				// SecretRef deliberately unset - falls back to the Environment's database section.
				Config: appsv1alpha1.DatabaseBackendSpec{Type: "postgres", Postgres: &appsv1alpha1.PostgresBackendSpec{
					Hostname: "h", Username: "u", Name: "n",
				}},
			}},
		},
	}
	block, secretName, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal("env-database", secretName)
	suite.Contains(block, `password: "file://`+databasePasswordMountPath+`"`)
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigPostgresSecretNotFound() {
	r := newFakeReconciler() // no Secret seeded at all
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
				SecretRef: "does-not-exist",
				Config: appsv1alpha1.DatabaseBackendSpec{Type: "postgres", Postgres: &appsv1alpha1.PostgresBackendSpec{
					Hostname: "h", Username: "u", Name: "n",
				}},
			}},
		},
	}
	_, _, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().Error(err)
	suite.Contains(err.Error(), "does-not-exist")
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigScopeRenderErrorPropagates() {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("x")},
	}
	r := newFakeReconciler(secret)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
				SecretRef: "db-secret",
				// type: postgres with no Postgres block set - renderDatabaseScope itself errors.
				Config: appsv1alpha1.DatabaseBackendSpec{Type: "postgres"},
			}},
		},
	}
	_, _, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().Error(err)
	suite.Contains(err.Error(), "spec.config.database.config")
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigPostgresMissingPasswordKey() {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: "default"},
		Data:       map[string][]byte{"not-password": []byte("x")},
	}
	r := newFakeReconciler(secret)
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
				SecretRef: "db-secret",
				Config: appsv1alpha1.DatabaseBackendSpec{Type: "postgres", Postgres: &appsv1alpha1.PostgresBackendSpec{
					Hostname: "h", Username: "u", Name: "n",
				}},
			}},
		},
	}
	_, _, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().Error(err)
	suite.Contains(err.Error(), "missing required key: password")
}

func (suite *DatabaseConfigTestSuite) TestResolveDatabaseConfigAllFourScopes() {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}
	r := newFakeReconciler(secret)
	pgSpec := appsv1alpha1.DatabaseBackendSpec{Type: "postgres", Postgres: &appsv1alpha1.PostgresBackendSpec{
		Hostname: "h", Username: "u", Name: "n",
	}}
	instance := &appsv1alpha1.ThunderIDInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: appsv1alpha1.ThunderIDInstanceSpec{
			Config: appsv1alpha1.AppConfigSpec{Database: &appsv1alpha1.DatabaseSpec{
				SecretRef: "db-secret", Config: pgSpec, RuntimeTransient: pgSpec, Entity: pgSpec, RuntimePersistent: pgSpec,
			}},
		},
	}
	block, secretName, err := r.resolveDatabaseConfig(context.Background(), instance)
	suite.Require().NoError(err)
	suite.Equal("db-secret", secretName)
	for _, scope := range []string{"config:", "runtime_transient:", "entity:", "runtime_persistent:"} {
		suite.Contains(block, scope)
	}
	// No leading/trailing blank lines - reconcileConfigMap swaps this in verbatim.
	suite.False(strings.HasPrefix(block, "\n"))
	suite.False(strings.HasSuffix(block, "\n"))
}

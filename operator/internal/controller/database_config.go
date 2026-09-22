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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const (
	databaseTypePostgres = "postgres"

	databaseSecretKeyPassword = "password"

	// databasePasswordMountPath is where the password key of spec.config.database.secretRef is mounted
	// into the container. Every postgres scope's rendered block references it via a file:// URI, the
	// same pattern deployment-config.yaml already uses for direct_auth_secret and crypto.key.
	databasePasswordMountPath = "config/secrets/db_password"

	sqliteConnOptionsDefault = "_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)"

	// The four database scopes ThunderID's own deployment.yaml "database:" section has - see
	// DatabaseSpec's doc comment for why these four and not one shared connection.
	databaseScopeConfig            = "config"
	databaseScopeRuntimeTransient  = "runtime_transient"
	databaseScopeEntity            = "entity"
	databaseScopeRuntimePersistent = "runtime_persistent"
)

// databaseScopeDefaultPath is the sqlite file each scope falls back to when its own
// spec.config.database.<scope>.sqlite.path is unset — the image's own convention, and what
// deployment-config.yaml's static default section (staticSQLiteDatabaseSection) already uses.
var databaseScopeDefaultPath = map[string]string{
	databaseScopeConfig:            "repository/database/configdb.db",
	databaseScopeRuntimeTransient:  "repository/database/runtime_transient.db",
	databaseScopeEntity:            "repository/database/entitydb.db",
	databaseScopeRuntimePersistent: "repository/database/runtime_persistent.db",
}

// staticSQLiteDatabaseSection is the literal "database:" body deployment-config.yaml carries by
// default (sqlite, the image's own built-in behavior) — the template owns this content directly,
// not the operator. resolveDatabaseConfig only comes into play once spec.config.database is set,
// where it needs this exact text as the search target to swap out wholesale for the rendered
// scopes. Must stay byte-identical to deployment-config.yaml's database: body (minus the
// "database:" header line, which stays put either way).
const staticSQLiteDatabaseSection = `  config:
    type: "sqlite"
    sqlite:
      path: "repository/database/configdb.db"
      options: "_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)"
      max_open_conns: 500
      max_idle_conns: 100
      conn_max_lifetime: 3600
  runtime_transient:
    type: "sqlite"
    sqlite:
      path: "repository/database/runtime_transient.db"
      options: "_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)"
      max_open_conns: 500
      max_idle_conns: 100
      conn_max_lifetime: 3600
  entity:
    type: "sqlite"
    sqlite:
      path: "repository/database/entitydb.db"
      options: "_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)"
      max_open_conns: 500
      max_idle_conns: 100
      conn_max_lifetime: 3600
  runtime_persistent:
    type: "sqlite"
    sqlite:
      path: "repository/database/runtime_persistent.db"
      options: "_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)"
      max_open_conns: 500
      max_idle_conns: 100
      conn_max_lifetime: 3600`

// usingPostgres reports whether instance is configured with every database scope pointed at
// PostgreSQL — meaning no local sqlite files exist anywhere, so spec.dataVolumeSize becomes
// unnecessary. A partial/mixed configuration (or spec.config.database unset entirely, which means
// all-sqlite) still needs it, since at least one scope's sqlite file needs somewhere to persist.
func usingPostgres(instance *appsv1alpha1.ThunderIDInstance) bool {
	db := instance.Spec.Config.Database
	if db == nil {
		return false
	}
	return db.Config.Type == databaseTypePostgres &&
		db.RuntimeTransient.Type == databaseTypePostgres &&
		db.Entity.Type == databaseTypePostgres &&
		db.RuntimePersistent.Type == databaseTypePostgres
}

// resolveDatabaseConfig returns "" when spec.config.database is unset (deployment-config.yaml's
// static sqlite section already correct, nothing to substitute), or the full "database:" body —
// all four scopes, mirroring ThunderID's own deployment.yaml shape 1:1 — to swap in for
// staticSQLiteDatabaseSection. secretName is the postgres password Secret to mount into the pod,
// set whenever any scope needs one.
func (r *ThunderIDInstanceReconciler) resolveDatabaseConfig(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) (block string, secretName string, err error) {
	db := instance.Spec.Config.Database
	if db == nil {
		return "", "", nil
	}

	needsPassword := db.Config.Type == databaseTypePostgres || db.RuntimeTransient.Type == databaseTypePostgres ||
		db.Entity.Type == databaseTypePostgres || db.RuntimePersistent.Type == databaseTypePostgres
	if needsPassword {
		// spec.config.database.secretRef, if set, always wins; otherwise fall back to the database
		// section of the Environment labeled for this instance.
		resolvedRef, err := resolveSectionSecretRef(ctx, r.Client, instance, db.SecretRef,
			func(e *appsv1alpha1.Environment) appsv1alpha1.EnvSection { return e.Spec.Database }, "database")
		if err != nil {
			return "", "", err
		}
		if resolvedRef == "" {
			return "", "", fmt.Errorf("spec.config.database has a postgres scope but spec.config.database.secretRef is not set and no Environment labeled %s=%s has a database section",
				appsv1alpha1.InstanceRefLabel, instance.Name)
		}
		// Error messages name whichever source actually supplied resolvedRef, so a misconfigured
		// explicit field and a misconfigured Environment point the user at the right place to fix it.
		source := fmt.Sprintf("spec.config.database.secretRef %q", resolvedRef)
		if db.SecretRef == "" {
			source = fmt.Sprintf("Environment's database Secret %q", resolvedRef)
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: resolvedRef, Namespace: instance.Namespace,
		}, secret); err != nil {
			return "", "", fmt.Errorf("%s: %w", source, err)
		}
		if string(secret.Data[databaseSecretKeyPassword]) == "" {
			return "", "", fmt.Errorf("%s is missing required key: password", source)
		}
		secretName = resolvedRef
	}

	scopes := []struct {
		name string
		spec appsv1alpha1.DatabaseBackendSpec
	}{
		{databaseScopeConfig, db.Config},
		{databaseScopeRuntimeTransient, db.RuntimeTransient},
		{databaseScopeEntity, db.Entity},
		{databaseScopeRuntimePersistent, db.RuntimePersistent},
	}

	// No "database:\n" header here — staticSQLiteDatabaseSection (the search target in
	// reconcileConfigMap) excludes the header line too, since the template keeps it fixed and
	// only the body underneath gets swapped.
	var sb strings.Builder
	for _, s := range scopes {
		scopeBlock, err := renderDatabaseScope(s.name, s.spec)
		if err != nil {
			return "", "", fmt.Errorf("spec.config.database.%s: %w", scopeJSONName(s.name), err)
		}
		fmt.Fprintf(&sb, "  %s:\n%s\n", s.name, scopeBlock)
	}

	return strings.TrimRight(sb.String(), "\n"), secretName, nil
}

// scopeJSONName maps a rendered scope's snake_case name (ThunderID's own convention) to the CRD's
// camelCase field name, for error messages.
func scopeJSONName(scope string) string {
	switch scope {
	case databaseScopeRuntimeTransient:
		return "runtimeTransient"
	case databaseScopeRuntimePersistent:
		return "runtimePersistent"
	default:
		return scope
	}
}

// renderDatabaseScope returns the 4-space-indented "type:"/"sqlite:"/"postgres:" body for one
// database scope.
func renderDatabaseScope(scopeName string, spec appsv1alpha1.DatabaseBackendSpec) (string, error) {
	if spec.Type == databaseTypePostgres {
		pg := spec.Postgres
		if pg == nil {
			return "", fmt.Errorf("type is %q but postgres is not set", databaseTypePostgres)
		}
		var missing []string
		if pg.Hostname == "" {
			missing = append(missing, "postgres.hostname")
		}
		if pg.Username == "" {
			missing = append(missing, "postgres.username")
		}
		if pg.Name == "" {
			missing = append(missing, "postgres.name")
		}
		if len(missing) > 0 {
			return "", fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
		}

		port := pg.Port
		if port == "" {
			port = "5432"
		}
		sslmode := pg.SSLMode
		if sslmode == "" {
			sslmode = "require"
		}
		maxOpen := pg.MaxOpenConns
		if maxOpen == 0 {
			maxOpen = 500
		}
		maxIdle := pg.MaxIdleConns
		if maxIdle == 0 {
			maxIdle = 100
		}
		connLifetime := pg.ConnMaxLifetime
		if connLifetime == 0 {
			connLifetime = 3600
		}

		return fmt.Sprintf(`    type: "postgres"
    postgres:
      hostname: "%s"
      port: %s
      username: "%s"
      password: "file://%s"
      name: "%s"
      sslmode: "%s"
      max_open_conns: %d
      max_idle_conns: %d
      conn_max_lifetime: %d`, pg.Hostname, port, pg.Username, databasePasswordMountPath, pg.Name, sslmode,
			maxOpen, maxIdle, connLifetime), nil
	}

	// sqlite — default when Type is empty or "sqlite".
	path := databaseScopeDefaultPath[scopeName]
	options := sqliteConnOptionsDefault
	maxOpen, maxIdle, connLifetime := int32(500), int32(100), int32(3600)
	if spec.SQLite != nil {
		if spec.SQLite.Path != "" {
			path = spec.SQLite.Path
		}
		if spec.SQLite.Options != "" {
			options = spec.SQLite.Options
		}
		if spec.SQLite.MaxOpenConns != 0 {
			maxOpen = spec.SQLite.MaxOpenConns
		}
		if spec.SQLite.MaxIdleConns != 0 {
			maxIdle = spec.SQLite.MaxIdleConns
		}
		if spec.SQLite.ConnMaxLifetime != 0 {
			connLifetime = spec.SQLite.ConnMaxLifetime
		}
	}

	return fmt.Sprintf(`    type: "sqlite"
    sqlite:
      path: "%s"
      options: "%s"
      max_open_conns: %d
      max_idle_conns: %d
      conn_max_lifetime: %d`, path, options, maxOpen, maxIdle, connLifetime), nil
}

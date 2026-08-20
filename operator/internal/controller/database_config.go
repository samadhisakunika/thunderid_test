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
	databaseTypePostgreSQL = "postgresql"

	databaseSecretKeyPassword = "password"

	// databasePasswordMountPath is where the password key of spec.config.database.secretRef is mounted
	// into the container. The postgres block built below references it via a file:// URI, the
	// same pattern deployment-config.yaml already uses for direct_auth_secret and crypto.key.
	databasePasswordMountPath = "config/secrets/db_password"

	// databaseBlockMarker is the placeholder line in deployment-config.yaml that
	// reconcileConfigMap replaces with the resolved sqlite/postgres block. It's a full,
	// syntactically valid YAML entry so the template stays parseable at rest.
	databaseBlockMarker = `  __DATABASE_BLOCK__: placeholder # replaced whole-line by reconcileConfigMap with the sqlite or postgres block`
)

// sqliteDatabaseBlock is the "database:" body used when spec.config.database.type is unset or "sqlite" —
// unchanged from what deployment-config.yaml hardcoded before postgres support existed.
const sqliteDatabaseBlock = `  config:
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

// usingPostgres reports whether instance is configured to connect to an external PostgreSQL
// server instead of the image's built-in SQLite files.
func usingPostgres(instance *appsv1alpha1.ThunderIDInstance) bool {
	return instance.Spec.Config.Database.Type == databaseTypePostgreSQL
}

// resolveDatabaseConfig returns the YAML to substitute into deployment-config.yaml's "database:"
// section, and — for postgres — the credentials Secret name so reconcileDeployment can mount its
// password key into the pod. Connection details (host/port/username/database names/sslmode) come
// straight from spec.config.database and are inlined directly since they aren't sensitive; only the
// password is kept out of the ConfigMap, sourced from spec.config.database.secretRef and resolved at
// runtime via a file:// reference like ThunderID's other secret values.
func (r *ThunderIDInstanceReconciler) resolveDatabaseConfig(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) (block string, secretName string, err error) {
	if !usingPostgres(instance) {
		return sqliteDatabaseBlock, "", nil
	}

	db := instance.Spec.Config.Database

	var missing []string
	if db.Host == "" {
		missing = append(missing, "spec.config.database.host")
	}
	if db.Username == "" {
		missing = append(missing, "spec.config.database.username")
	}
	if db.SecretRef == "" {
		missing = append(missing, "spec.config.database.secretRef")
	}
	if len(missing) > 0 {
		return "", "", fmt.Errorf("spec.config.database.type is %q but missing required field(s): %s",
			databaseTypePostgreSQL, strings.Join(missing, ", "))
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: db.SecretRef, Namespace: instance.Namespace,
	}, secret); err != nil {
		return "", "", fmt.Errorf("spec.config.database.secretRef %q: %w", db.SecretRef, err)
	}

	password := string(secret.Data[databaseSecretKeyPassword])
	if password == "" {
		return "", "", fmt.Errorf("spec.config.database.secretRef %q is missing required key: password", db.SecretRef)
	}

	getOr := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}

	host := db.Host
	port := getOr(db.Port, "5432")
	username := db.Username
	sslmode := getOr(db.SSLMode, "require")
	configDB := getOr(db.ConfigDatabase, "thunderid_config")
	runtimeDB := getOr(db.RuntimeDatabase, "thunderid_runtime")

	// Field names (config.PostgresDataSource) confirmed by iterating against the running image's
	// own strict YAML unmarshal errors, not just binary string inspection: it named the struct
	// and rejected "host"/"database"/"dbname" one at a time while accepting
	// port/username/password/sslmode/pool settings unmodified — hostname/name is the corrected
	// pair.
	section := func(dbName string) string {
		return fmt.Sprintf(`    type: "postgres"
    postgres:
      hostname: "%s"
      port: %s
      username: "%s"
      password: "file://%s"
      name: "%s"
      sslmode: "%s"
      max_open_conns: 500
      max_idle_conns: 100
      conn_max_lifetime: 3600`, host, port, username, databasePasswordMountPath, dbName, sslmode)
	}

	// runtime_transient and runtime_persistent share one database (per the runtimeDatabase key),
	// and so do config and entity (per configDatabase): ThunderID's own dbscripts/ cross-reference
	// tables across those pairs (e.g. entity-category lookups query ENTITY_TYPES, which lives in
	// dbscripts/configdb/), so each pair must resolve to the same physical database, distinguished
	// by table set rather than by database.
	block = fmt.Sprintf("  config:\n%s\n  runtime_transient:\n%s\n  entity:\n%s\n  runtime_persistent:\n%s",
		section(configDB), section(runtimeDB), section(configDB), section(runtimeDB))

	return block, instance.Spec.Config.Database.SecretRef, nil
}

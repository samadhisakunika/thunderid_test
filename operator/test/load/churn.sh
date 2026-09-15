#!/usr/bin/env bash
# Load-generates Resource CR churn (create -> update -> delete) against one ThunderIDInstance,
# across all 11 resource_types, from N concurrent workers. Used to find operator-side races/bugs
# under concurrent reconciles (ConfigMap rebuild, finalizer cleanup, RetryOnConflict paths).
#
# Usage: churn.sh <instance-name> <namespace> <workers> <cycles-per-worker> <logdir>
set -u
INSTANCE="$1"
NS="$2"
WORKERS="$3"
CYCLES="$4"
LOGDIR="$5"
mkdir -p "$LOGDIR"

TYPES=(application flow theme role user group organization_unit user_type resource_server server_config translation)

# One anchor OU + UserType per instance, created once, never touched by churn — gives the
# per-iteration application/role/user/group/resource_server/user_type resources something real
# to reference (ouHandle/type), instead of dangling refs.
apply_anchors() {
  kubectl -n "$NS" apply -f - <<EOF
apiVersion: apps.thunderid.io/v1alpha1
kind: Resource
metadata:
  name: ${INSTANCE}-anchor-ou
  labels: {thunderid.io/instance: ${INSTANCE}}
spec:
  resource_type: organization_unit
  handle: ${INSTANCE}-anchor-ou
  name: Loadtest Anchor OU
---
apiVersion: apps.thunderid.io/v1alpha1
kind: Resource
metadata:
  name: ${INSTANCE}-anchor-usertype
  labels: {thunderid.io/instance: ${INSTANCE}}
spec:
  resource_type: user_type
  category: user
  name: Loadtest Anchor UserType
  ouHandle: ${INSTANCE}-anchor-ou
  schema:
    username: {"type": "string", "displayName": "Username", "required": true, "unique": true}
    email: {"type": "string", "displayName": "Email", "required": true, "unique": true}
EOF
}

# spec body for one resource_type. $1=name id (unique per iteration), $2=mutation suffix (used on
# update pass to change a field so the ConfigMap content actually changes).
spec_for() {
  local type="$1" id="$2" tag="$3"
  case "$type" in
  application)
    cat <<EOF
  resource_type: application
  name: Loadtest App ${id} ${tag}
  type: browser
  ouHandle: ${INSTANCE}-anchor-ou
  authFlowHandle: console-app-flow
  logoUrl: "emoji:🚀"
  isRegistrationFlowEnabled: true
  isRecoveryFlowEnabled: false
  isSignOutFlowEnabled: true
  inboundAuthConfig:
    - type: oauth2
      config:
        clientId: LOADTEST_${id}
        redirectUris: ["https://localhost:8090/console"]
        grantTypes: ["authorization_code", "refresh_token"]
        responseTypes: ["code"]
        tokenEndpointAuthMethod: client_secret_basic
EOF
    ;;
  flow)
    cat <<EOF
  resource_type: flow
  handle: loadtest-flow-${id}
  name: Loadtest Flow ${id} ${tag}
  flowType: AUTHENTICATION
  isReadOnly: false
  nodes:
    - id: start
      type: START
      onSuccess: end
    - id: end
      type: END
EOF
    ;;
  theme)
    cat <<EOF
  resource_type: theme
  handle: loadtest-theme-${id}
  displayName: Loadtest Theme ${id} ${tag}
  description: churn theme
  isReadOnly: false
  theme: {"defaultColorScheme":"dark"}
EOF
    ;;
  role)
    cat <<EOF
  resource_type: role
  name: Loadtest Role ${id} ${tag}
  description: churn role
  ouHandle: ${INSTANCE}-anchor-ou
EOF
    ;;
  user)
    cat <<EOF
  resource_type: user
  type: Loadtest Anchor UserType
  ouHandle: ${INSTANCE}-anchor-ou
  attributes:
    username: loadtestuser${id}
    email: loadtestuser${id}@example.com
    name: Loadtest User ${id} ${tag}
EOF
    ;;
  group)
    cat <<EOF
  resource_type: group
  name: Loadtest Group ${id} ${tag}
  description: churn group
  ouHandle: ${INSTANCE}-anchor-ou
EOF
    ;;
  organization_unit)
    cat <<EOF
  resource_type: organization_unit
  handle: loadtest-ou-${id}
  name: Loadtest OU ${id} ${tag}
EOF
    ;;
  user_type)
    cat <<EOF
  resource_type: user_type
  category: user
  name: Loadtest UserType ${id} ${tag}
  ouHandle: ${INSTANCE}-anchor-ou
  schema:
    username: {"type": "string", "displayName": "Username", "required": true, "unique": true}
EOF
    ;;
  resource_server)
    cat <<EOF
  resource_type: resource_server
  name: Loadtest RS ${id} ${tag}
  description: churn resource server
  identifier: https://loadtest.local/${id}
  ouHandle: ${INSTANCE}-anchor-ou
  resources:
    - name: System
      handle: system
      description: system resource
EOF
    ;;
  server_config)
    cat <<EOF
  resource_type: server_config
  name: loadtestConfig${id}
  value:
    idleTimeoutSeconds: 1800
    tag: "${tag}"
EOF
    ;;
  translation)
    cat <<EOF
  resource_type: translation
  language: xx-${id}
  translations:
    auth:
      login: "Log in ${tag}"
EOF
    ;;
  esac
}

apply_resource() {
  local type="$1" name="$2" id="$3" tag="$4"
  kubectl -n "$NS" apply -f - <<EOF
apiVersion: apps.thunderid.io/v1alpha1
kind: Resource
metadata:
  name: ${name}
  labels: {thunderid.io/instance: ${INSTANCE}}
spec:
$(spec_for "$type" "$id" "$tag")
EOF
}

worker() {
  local wid="$1"
  local log="$LOGDIR/worker-${INSTANCE}-${wid}.log"
  : >"$log"
  for ((i = 0; i < CYCLES; i++)); do
    local type="${TYPES[$(( (wid + i) % ${#TYPES[@]} ))]}"
    local id="${wid}-${i}-$$"
    local name="loadtest-${type//_/-}-${id}"

    local t0 t1
    t0=$(date +%s.%N)
    if ! apply_resource "$type" "$name" "$id" "v1" >>"$log" 2>&1; then
      echo "CREATE_FAIL $type $name" >>"$log"
    fi
    if ! apply_resource "$type" "$name" "$id" "v2-updated" >>"$log" 2>&1; then
      echo "UPDATE_FAIL $type $name" >>"$log"
    fi
    if ! kubectl -n "$NS" delete resource "$name" --timeout=30s >>"$log" 2>&1; then
      echo "DELETE_FAIL $type $name" >>"$log"
    fi
    t1=$(date +%s.%N)
    echo "CYCLE_DONE $type $name $(echo "$t1 - $t0" | bc 2>/dev/null || echo "?")" >>"$log"
  done
}

echo "=== churn phase: instance=$INSTANCE workers=$WORKERS cycles=$CYCLES ==="
apply_anchors
for ((w = 0; w < WORKERS; w++)); do
  worker "$w" &
done
wait
echo "=== churn phase done: instance=$INSTANCE ==="

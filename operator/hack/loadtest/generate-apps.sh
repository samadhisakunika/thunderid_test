#!/usr/bin/env bash
# Generates N Application CRs (all pointing at the same ThunderIDInstance) into one YAML file.
# Usage: ./generate-apps.sh [count] [instanceRef] [outfile] [namePrefix]
set -euo pipefail

COUNT="${1:-100}"
INSTANCE_REF="${2:-thunderid-prod}"
OUT="${3:-$(dirname "$0")/loadtest-100-apps.yaml}"
PREFIX="${4:-loadtest-app}"

> "$OUT"
for i in $(seq -w 1 "$COUNT"); do
cat >> "$OUT" <<EOF
apiVersion: apps.thunderid.io/v1alpha1
kind: Application
metadata:
  name: $PREFIX-$i
  namespace: default
  labels:
    loadtest: "true"
    thunderid.io/instance: $INSTANCE_REF
spec:
  config:
    name: "Load Test App $i"
    ouId: "019ef85a-0aa4-7736-9357-d25edba4db95"
    authFlowHandle: console-app-flow
    themeId: "019ef85a-0eab-7d82-b9a9-62b8be7efe8c"
    isRegistrationFlowEnabled: true
    isRecoveryFlowEnabled: false
    allowedUserTypes:
      - Person
    assertion:
      validityPeriod: 3600
    loginConsent:
      validityPeriod: 0
---
EOF
done
echo "wrote $COUNT Application CRs to $OUT"

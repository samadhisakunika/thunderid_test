#!/usr/bin/env bash
# Creates N Application CRs one at a time, each via its own `kubectl apply` call —
# simulates individual client requests rather than one bulk multi-doc apply.
# Usage: ./create-apps-individual.sh [count] [instanceRef] [namePrefix]
set -euo pipefail

COUNT="${1:-100}"
INSTANCE_REF="${2:-thunderid-prod}"
PREFIX="${3:-loadtest-app}"

for i in $(seq -w 1 "$COUNT"); do
  kubectl apply -f - <<EOF
apiVersion: apps.thunderid.io/v1alpha1
kind: Resource
metadata:
  name: $PREFIX-$i
  namespace: default
  labels:
    loadtest: "true"
    thunderid.io/instance: $INSTANCE_REF
spec:
  resource_type: application
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
EOF
done

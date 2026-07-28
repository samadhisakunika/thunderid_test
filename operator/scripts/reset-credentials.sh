#!/usr/bin/env bash
# Resets an auto-generated User credentials Secret to a fresh random username/password.
#
# Only works for users that left spec.credentialsSecretRef empty — the operator only
# owns (and can safely regenerate) Secrets it created itself. If you supplied your own
# Secret, rotate it yourself (e.g. `kubectl edit secret <name>`); the operator picks up
# whatever values are in it on its next reconcile.
#
# A ThunderID pod restart is still required afterwards for the new password to
# actually work for login — ThunderID only reads its resources at pod startup.
#
# Usage: reset-credentials.sh <user-name> [namespace]
set -euo pipefail

name="${1:?usage: reset-credentials.sh <user-name> [namespace]}"
namespace="${2:-default}"

ref="$(kubectl get user "$name" -n "$namespace" -o jsonpath='{.spec.credentialsSecretRef}' 2>/dev/null)"
if [ -n "$ref" ]; then
  echo "error: user '$name' has spec.credentialsSecretRef=$ref set explicitly — the operator doesn't own that Secret." >&2
  echo "Rotate it yourself, e.g.: kubectl edit secret $ref -n $namespace" >&2
  exit 1
fi

secret="$(kubectl get user "$name" -n "$namespace" -o jsonpath='{.status.credentialsSecretRef}' 2>/dev/null)"
if [ -z "$secret" ]; then
  echo "error: user '$name' in namespace '$namespace' has no generated credentials Secret yet." >&2
  exit 1
fi

echo "Deleting $secret and forcing a reconcile..." >&2
kubectl delete secret "$secret" -n "$namespace"
kubectl annotate user "$name" -n "$namespace" "thunderid.io/reset-credentials=$(date +%s)" --overwrite >/dev/null

for _ in $(seq 1 30); do
  if kubectl get secret "$secret" -n "$namespace" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
echo 
echo "New credentials Secret $secret created." >&2
kubectl get secret "$secret" -n "$namespace" \
  -o go-template='Username: {{.data.username|base64decode}}{{"\n"}}New Password: {{.data.password|base64decode}}{{"\n"}}'
echo
echo "Note: the ThunderID pod must restart before this new password actually works for login." >&2

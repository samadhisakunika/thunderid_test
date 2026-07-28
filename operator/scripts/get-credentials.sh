#!/usr/bin/env bash
# Prints the username/password for a User's credentials Secret.
#
# Usage: get-credentials.sh <user-name> [namespace]
set -euo pipefail

name="${1:?usage: get-credentials.sh <user-name> [namespace]}"
namespace="${2:-default}"

secret="$(kubectl get user "$name" -n "$namespace" -o jsonpath='{.status.credentialsSecretRef}' 2>/dev/null)"
if [ -z "$secret" ]; then
  echo "error: user '$name' in namespace '$namespace' has no status.credentialsSecretRef yet (not synced?)" >&2
  exit 1
fi

kubectl get secret "$secret" -n "$namespace" \
  -o go-template='Username: {{.data.username|base64decode}}{{"\n"}}Password: {{.data.password|base64decode}}{{"\n"}}'

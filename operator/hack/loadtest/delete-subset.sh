#!/usr/bin/env bash
# Deletes N loadtest Applications (real delete, exercises the finalizer/cleanup path).
# Usage: ./delete-subset.sh [count]
set -euo pipefail
COUNT="${1:-100}"

targets=$(kubectl get applications -l loadtest=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -n "$COUNT")
if [ -z "$targets" ]; then
  echo "no loadtest applications found"
  exit 0
fi

for name in $targets; do
  kubectl delete application "$name" --wait=false
done

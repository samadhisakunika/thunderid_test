#!/usr/bin/env bash
# Patches a handful of loadtest Applications' specs while they may still be
# mid-reconcile (Phase != Ready), to exercise the concurrent-edit path.
# Usage: ./mid-flight-edit.sh [appNamePattern] [count]
set -euo pipefail
COUNT="${1:-10}"

# Pick N apps that are not yet Ready (falls back to first N if all already converged).
targets=$(kubectl get applications -l loadtest=true -o jsonpath='{range .items[?(@.status.phase!="Ready")]}{.metadata.name}{"\n"}{end}' | head -n "$COUNT")
if [ -z "$targets" ]; then
  targets=$(kubectl get applications -l loadtest=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | head -n "$COUNT")
fi

for name in $targets; do
  phase=$(kubectl get application "$name" -o jsonpath='{.status.phase}')
  echo "patching $name (phase was: $phase)"
  kubectl patch application "$name" --type=merge -p \
    '{"spec":{"description":"mid-flight-edit-'"$(date +%s%N)"'"}}'
done

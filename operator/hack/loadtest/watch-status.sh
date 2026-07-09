#!/usr/bin/env bash
# Polls phase/synced of all loadtest Applications every 2s until all Ready or timeout.
set -euo pipefail
TIMEOUT="${1:-180}"
elapsed=0
while [ "$elapsed" -lt "$TIMEOUT" ]; do
  total=$(kubectl get applications -l loadtest=true --no-headers | wc -l)
  ready=$(kubectl get applications -l loadtest=true -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^Ready$' || true)
  pending=$(kubectl get applications -l loadtest=true -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^Pending$' || true)
  error=$(kubectl get applications -l loadtest=true -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^Error$' || true)
  echo "[t+${elapsed}s] total=$total ready=$ready pending=$pending error=$error"
  if [ "$ready" -eq "$total" ] && [ "$total" -gt 0 ]; then
    echo "all $total applications Ready"
    exit 0
  fi
  sleep 2
  elapsed=$((elapsed + 2))
done
echo "timed out after ${TIMEOUT}s — not all applications reached Ready"
kubectl get applications -l loadtest=true -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,SYNCED:.status.synced

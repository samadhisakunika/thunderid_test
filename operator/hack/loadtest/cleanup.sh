#!/usr/bin/env bash
# Removes all loadtest Application CRs.
set -euo pipefail
kubectl delete applications -l loadtest=true

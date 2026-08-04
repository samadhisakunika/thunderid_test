# GitOps-managed resources

Everything under this directory is watched by Flux (`flux-system` Kustomization
`gitops`, see `.flux`-generated manifests under `clusters/` at repo root after
bootstrap). Any CR file committed and pushed here is automatically applied to
the cluster — no `kubectl apply` needed.

## Rules

- Only put `apps.thunderid.io` CRs here (`ThunderIDInstance`, `Application`,
  `Flow`, `Theme`, etc.) that you want Flux to own and continuously reconcile.
- Don't put scratch/one-off test files here — Flux will keep re-applying
  (and, on deletion from Git, deleting) whatever's in this directory. Use the
  repo root or `operator/` for local experiments instead.
- A change to a file here (including edits to an existing CR, not just new
  files) is picked up on Flux's next reconciliation (default interval: 1m).

## Layout

Empty for now. Once Flux is bootstrapped, CR files placed here (or moved here
from the root-level demo files) become the live, Git-driven source of truth
for the cluster.

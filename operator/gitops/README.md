# GitOps-managed resources

Everything under this directory (except `flux-system/`, which is Flux's own
bootstrap-generated config) is watched by the `flux-system` Kustomization on
the cluster. Any CR file committed and pushed here is automatically applied
— no `kubectl apply` needed. Reconcile interval is 30s (see
`flux-system/gotk-sync.yaml`); force an immediate sync with `flux reconcile
kustomization flux-system --with-source`.

A second Kustomization, `crds`, applies `operator/config/crd/bases/`
(outside this directory) before `flux-system` reconciles — `flux-system`
declares `dependsOn: crds`, so the `apps.thunderid.io` CRDs always land
before any CR that needs them. This means CRD installation is Git-managed
too, not just the CRs themselves.

## Rules

- Only put `apps.thunderid.io` CRs here (`ThunderIDInstance`, `Application`,
  `Flow`, `Theme`, etc.) that you want Flux to own and continuously reconcile.
- Don't put scratch/one-off test files here — Flux will keep re-applying
  (and, on deletion from Git, deleting) whatever's in this directory. Use the
  repo root or `operator/` for local experiments instead.
- A change to a file here (including edits to an existing CR, not just new
  files) is picked up on Flux's next reconciliation (default interval: 1m).

## Layout

- `thunderidConfig.yaml` — the `ThunderIDInstance` (`thunderid-prod`) itself.
  Promoted here from the repo root, so even standing up the instance is now
  a `git push`, not a `kubectl apply`.
- `organizationunit-flux-test.yaml` — throwaway OU proving commit-to-apply and
  edit-to-apply work end to end. Safe to delete (delete the file + push) once
  no longer needed; Flux prunes it from the cluster on the next sync.
- `demo-partner-app.yaml` / `demo-partner-app-cors.yaml` — the live-demo
  `Application` and CORS `ServerConfig`, promoted here from the repo root so
  edits (e.g. changing the login flow, redirect URIs) go live via `git push`.
- `test-flow.yaml` — `test-basic-flow`, the demo login flow with the
  "Welcome to Demo Partner Portal" heading used in the modification demo.

Not here, and not Git-managed: the operator itself (still run via `go run
./cmd/main.go`, see [demo-partner-app/README.md](../../demo-partner-app/README.md))
and the CRDs' source files (`operator/config/crd/bases/`, applied by the
`crds` Kustomization above — that directory is shared with `make install`
for non-Flux setups, so it stays in its original location rather than being
duplicated here).

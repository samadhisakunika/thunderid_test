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
- **Never put an `Environment` CR here.** Its controller mutates the object
  in place after processing it (masking each synced key's value in each
  section's `env`). A continuously-reconciling GitOps tool like Flux enforces
  "live must match Git" and will keep reverting that mutation every
  reconcile interval, fighting the controller forever. `Environment` is an
  ad-hoc, one-time `kubectl apply` resource by design — see
  `environment_type.go` and `environment_controller.go`. It also carries
  plaintext credentials in its spec, which shouldn't be committed to Git
  regardless.
- **Always apply `Environment` CRs — and any hand-written `Secret` manifest
  — with `kubectl apply --server-side`, never plain `kubectl apply`.** Plain
  (client-side) `kubectl apply` copies your entire manifest, credentials
  included, into the object's
  `kubectl.kubernetes.io/last-applied-configuration` annotation — visible
  forever via `kubectl get ... -o yaml`, completely bypassing `Environment`'s
  masking. This isn't a `Secret`-specific protection either — `kubectl
  describe secret` hiding values is a print-time convenience that doesn't
  touch this annotation; the same leak
  happens on a directly-applied Secret. `--server-side` doesn't create this
  annotation at all (it tracks field ownership via `managedFields` instead),
  and as a bonus it rejects an accidental reapply of an unedited file outright
  (a conflict error) instead of silently letting it through. Caveat: when
  changing one key in a multi-key `Environment`, apply a manifest containing
  *only* that key — resubmitting the full original file makes every
  already-masked sibling key conflict at once, and `--force-conflicts` would
  overwrite all of them simultaneously. An object already carrying the old
  annotation from a prior plain apply needs a one-time manual strip:
  `kubectl annotate <kind> <name> kubectl.kubernetes.io/last-applied-configuration- -n <namespace>`.
- Don't put scratch/one-off test files here — Flux will keep re-applying
  (and, on deletion from Git, deleting) whatever's in this directory. Use the
  repo root or `operator/` for local experiments instead.
- A change to a file here (including edits to an existing CR, not just new
  files) is picked up on Flux's next reconciliation (default interval: 1m).

## Layout

- `thunderidConfig.yaml` — the `ThunderIDInstance` (`thunderid-prod`) itself.
  Standing up the instance is a `git push`, not a `kubectl apply`.
- `organizationunit-flux-test.yaml` — throwaway OU used to verify
  commit-to-apply and edit-to-apply work end to end. Safe to delete (delete
  the file + push) once you no longer need it; Flux prunes it from the
  cluster on the next sync.
- `demo-partner-app.yaml` / `demo-partner-app-cors.yaml` — the live-demo
  `Application` and CORS `ServerConfig`; edits (e.g. changing the login
  flow, redirect URIs) go live via `git push`.
- `test-flow.yaml` — `test-basic-flow`, the demo login flow with the
  "Welcome to Demo Partner Portal" heading used in the modification demo.

Not here, and not Git-managed: the operator itself (still run via `go run
./cmd/main.go`, see [demo-partner-app/README.md](../../demo-partner-app/README.md))
and the CRDs' source files (`operator/config/crd/bases/`, applied by the
`crds` Kustomization above — that directory is shared with `make install`
for non-Flux setups, so it stays in its original location rather than being
duplicated here).

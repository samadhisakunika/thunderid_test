# Demo Partner App — Local Setup Guide

A minimal React SPA that logs in through a ThunderID-managed OAuth2 flow.
It's two halves: the ThunderID backend resources (a `ThunderIDInstance` plus
an `Application`/`ServerConfig`/`Flow` CR, all managed by the
[thunderid-operator](../operator)), and this frontend, which authenticates
against them via `@thunderid/react`.

The backend half is deliberately GitOps-managed (via [Flux](https://fluxcd.io))
— the point of this demo isn't just "an app that logs in," it's demonstrating
the full workflow this project is built around: edit a CR file, `git push`,
and the change is live in the cluster with no `kubectl` involved. That's not
limited to the demo's own CRs — the CRDs and the `ThunderIDInstance` itself
are Flux-managed too (via `operator/gitops/`), so *everything* below has a
commit-and-push equivalent, not just the app-specific bits.

For deeper detail on any CRD, see the [operator README](../README.md).

## Prerequisites

| Tool | Notes |
|------|-------|
| kubectl | Configured to talk to your cluster |
| A local Kubernetes cluster | [minikube](https://minikube.sigs.k8s.io/) or [kind](https://kind.sigs.k8s.io/) |
| Go 1.21+ | To run the operator locally |
| Node.js 18+ / npm | To run this app |
| A GitHub account + fork of this repo | To bootstrap your own Flux instance against it (step 1) |

## 1. Set up GitOps (Flux)

1. **Fork this repo** on GitHub if you don't already have push access to it.

2. **Install the Flux CLI:**
   ```bash
   # Linux / WSL
   curl -s https://fluxcd.io/install.sh | sudo bash

   # Windows (PowerShell)
   winget install --id FluxCD.Flux -e
   ```

3. **Check your cluster is compatible:**
   ```bash
   flux check --pre
   ```

4. **Create a GitHub token** (classic PAT with `repo` scope, or fine-grained
   with Contents + Administration read/write on your fork) at
   https://github.com/settings/tokens

5. **Bootstrap Flux against your fork:**
   ```bash
   export GITHUB_TOKEN="your-token-here"
   flux bootstrap github \
     --owner=<your-github-username> \
     --repository=<your-fork-name> \
     --branch=main \
     --path=operator/gitops \
     --personal
   ```
   This installs the Flux controllers on your cluster **and** pushes a
   commit back to your fork with the generated `flux-system` manifests
   under `operator/gitops/flux-system/` — that's expected, it's how Flux
   wires itself up to watch that path.

6. **Verify it's reconciling — you should see two Kustomizations:**
   ```bash
   kubectl get gitrepository,kustomization -n flux-system
   ```
   `crds` (applies `operator/config/crd/bases/`) and `flux-system` (applies
   `operator/gitops/`, and declares `dependsOn: crds` so ordering is always
   correct) should both show `READY=True` within a minute or two.

By default Flux polls your fork every 1 minute (source) / 10 minutes
(apply) — plenty for real use, but slow for live-demoing a change. To speed
that up, edit the generated `operator/gitops/flux-system/gotk-sync.yaml`
(set the `interval` fields to e.g. `15s`/`30s`), commit, push, then run
`flux reconcile kustomization flux-system --with-source` once to apply that
change itself — everything after that is fast automatically.

## 2. Confirm the CRDs and ThunderID instance came up

Once `crds` and `flux-system` both report `READY=True`, the 12
`apps.thunderid.io` CRDs and the `thunderid-prod` `ThunderIDInstance`
(defined in `operator/gitops/thunderidConfig.yaml`) should already be on
your cluster — nothing to apply by hand:

```bash
kubectl get crd | grep thunderid.io   # should list 12
kubectl get pod -w                    # wait for thunderid-prod to reach 1/1 Running
```

The image bakes in its own default organization unit, user types, and flows
(`default` OU, `Person` user type, `console-app-flow`, `cloud-login-flow`,
etc.), so applications can register against them with no separate seed step.

`thunderidConfig.yaml` uses SQLite (`dataVolumeSize`) as committed. PostgreSQL
is also supported — see the [Database section](../README.md#database) of the
operator README if you'd rather use that; edit the file in your fork, commit,
and push to switch.

## 3. Run the operator

```bash
cd operator
go run ./cmd/main.go
```

Keep this running in its own terminal — CR changes only take effect while
it's up. Flux only gets CR files onto the Kubernetes API server; it's the
operator that actually turns them into ThunderID config, so it needs to
already be running (or start it now) once the CRDs from step 2 exist.

## 4. Confirm the demo's ThunderID resources applied

`demo-partner-app.yaml`, `demo-partner-app-cors.yaml`, and `test-flow.yaml`
also live in `operator/gitops/`, applied by the same `flux-system`
Kustomization as step 2 — nothing further to run. Confirm they landed:

```bash
kubectl get application demo-partner-app
kubectl get serverconfig thunderid-prod-cors
kubectl get flow test-basic-flow
```

All three should reach `Ready` once the operator (step 3) has reconciled
them.


**Skipping Flux entirely for a quick one-off test?** Apply everything by hand
instead of step 1 — just know you'll need to re-run these anytime you change
one of these files, since nothing else will pick up the change:
```bash
kubectl apply -f operator/config/crd/bases/
kubectl apply -f operator/gitops/thunderidConfig.yaml
kubectl apply -f operator/gitops/demo-partner-app-cors.yaml
kubectl apply -f operator/gitops/demo-partner-app.yaml
kubectl apply -f operator/gitops/test-flow.yaml
```

## 5. Point the frontend at your instance

The React app needs to know this `Application` CR's actual ID, which
Kubernetes assigns randomly per cluster — the value already in
`src/main.jsx` belongs to whoever's cluster last set this up, not yours.
Get your own:

```bash
kubectl get application demo-partner-app -o jsonpath='{.metadata.uid}'
```

Paste that into `src/main.jsx`:

```jsx
<ThunderIDProvider
  clientId="DEMO_PARTNER_APP"
  applicationId="<paste the uid from above>"
  baseUrl="https://localhost:8090"
>
```

(`clientId` doesn't need changing — it's the literal string set in
`demo-partner-app.yaml`'s `inboundAuthConfig.clientId`, not cluster-specific.)

## 6. Expose ThunderID to your browser

```bash
kubectl port-forward svc/thunderid-prod 8090:8090
```

Keep this running too. ThunderID serves a self-signed cert by default —
open `https://localhost:8090` directly in your browser once first and accept
the certificate warning, otherwise the app's background requests to it will
silently fail.

## 7. Run the frontend

```bash
cd demo-partner-app
npm install
npm run dev
```

Open `http://localhost:5173`.

## 8. Try it out

- Click **Sign In** — you'll be redirected to ThunderID's hosted login page
  (`console-app-flow`, or `test-basic-flow`'s "Welcome to Demo Partner
  Portal" screen if you switched `authFlowHandle`).
- Log in as a user that exists in your instance (the baked-in defaults don't
  include one — create a `User` CR first, see the [User](../README.md#user)
  section of the operator README for how to get its generated credentials).
- You'll land back on `http://localhost:5173` signed in, showing your name/
  email and a **Sign Out** button.
- Now make the point of this whole setup: edit `test-flow.yaml`'s heading
  text (or `demo-partner-app.yaml`'s `logoUrl`), commit, push, and refresh
  the login page once the pod restarts — no `kubectl` command run at any
  point.

## Troubleshooting

- **Redirect fails / CORS error in the browser console** — the port you're
  running the dev server on isn't in `demo-partner-app-cors.yaml`'s
  `allowedOrigins` or `demo-partner-app.yaml`'s `redirectUris`. Vite defaults
  to `5173`, but picks the next free port (`5174`, `5175`, ...) if that's
  taken — check what URL it actually printed, add it to both files, commit,
  push, and wait for the pod restart.
- **Blank page / network errors only** — you probably skipped visiting
  `https://localhost:8090` directly first to accept the self-signed cert.
- **`invalid_client` or similar on redirect back** — `applicationId` in
  `main.jsx` doesn't match this cluster's `demo-partner-app` UID (step 5) —
  common after re-creating the `Application` CR, since a new object gets a
  new UID.
- **A CR change isn't showing up on an already-open sign-in tab** — ThunderID
  appears to snapshot the flow definition into your session when the gate
  first loads, not re-resolve it on every request. If you edit a `Flow` CR
  while a sign-in tab is already open, that tab keeps showing the old
  version even after the pod's picked up the change (confirm with
  `kubectl get configmap <instance>-flows -o yaml` if in doubt — the new
  content will already be there). Navigate away and back (or open a fresh
  tab) to force a new session against the current definition.
- **A CR change isn't showing up in the cluster** — check Flux's own status
  before assuming the operator is broken:
  ```bash
  kubectl get gitrepository,kustomization -n flux-system
  ```
  If `crds` shows `READY=False`, `flux-system` never even starts (it depends
  on `crds`). If `flux-system` shows `READY=False`, the error message
  usually names the exact bad field (e.g. an unknown field not in the CRD
  schema) — Flux applies nothing under `operator/gitops/` until that's
  fixed, not just the one broken file.
- **Everything else** (APP-1018, APP-1042, pod stuck, etc.) — see the
  [operator README's Troubleshooting section](../README.md#troubleshooting).

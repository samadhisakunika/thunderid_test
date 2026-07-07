# ThunderID Operator

A Kubernetes operator that manages [ThunderID](https://github.com/thunder-id/thunderid) identity provider deployments declaratively. It lets you spin up ThunderID instances, define auth flows and themes, and register OAuth2 applications — all as Kubernetes custom resources.

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Architecture](#architecture)
- [Quick Start](#quick-start)
  - [1. Install CRDs](#1-install-crds)
  - [2. Apply base resources](#2-apply-base-resources)
  - [3. Start ThunderID](#3-start-thunderid)
  - [4. Run the operator](#4-run-the-operator)
  - [5. Register an application](#5-register-an-application)
- [Custom Resources](#custom-resources)
  - [ThunderIDConfig](#thunderidconfig)
  - [ThunderIDInstance](#thunderidinstance)
  - [Application](#application)
  - [Flow](#flow)
  - [Theme](#theme)
- [Checking Status](#checking-status)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

---

## Prerequisites

| Tool | Notes |
|------|-------|
| Go 1.26+ | Only needed for local development |
| kubectl | Configured to talk to your cluster |
| Kubernetes 1.28+ | Local: [kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/) |
| make | Needed for operator build/deploy targets |

---

## Architecture

```
ThunderIDConfig  ──creates──►  ThunderIDInstance  ──creates──►  Deployment / Pod
      │                               │
      └──creates──► HPA               └── reads ── thunderid-resources ConfigMap
                                               (base OU, flows, themes)

Application CRD  ──writes──►  <instance>-applications ConfigMap
                               └── mounted into ThunderID pod at startup

Flow CRD         ──calls──►  ThunderID API  (live, no restart needed)
Theme CRD        ──calls──►  ThunderID API  (live, no restart needed)
```

**Key facts:**
- Each ThunderID pod uses an **ephemeral SQLite database**. On every pod restart the database is wiped and rebuilt from the resources ConfigMap.
- `spec.resourcesConfigMap` must point to a ConfigMap that seeds the base OU, flows, and themes **before** any Application resources are loaded. Without this, Application loading will fail with `APP-1018`.
- Applications are loaded at **pod startup** via a mounted ConfigMap. A config change triggers a rolling restart; it is not applied live.

---

## Quick Start

### 1. Install CRDs

```bash
cd operator
make install
```

Verify:

```bash
kubectl get crd | grep thunderid
```

Expected output:
```
applications.apps.thunderid.io
flows.apps.thunderid.io
themes.apps.thunderid.io
thunderidconfigs.apps.thunderid.io
thunderidinstances.apps.thunderid.io
```

---

### 2. Apply base resources

ThunderID's SQLite database starts empty on every pod restart. The base resources ConfigMap seeds it with the default OU, auth flows, and themes that your applications reference.

```bash
kubectl apply -f operator/config/samples/thunderid-resources-configmap.yaml
```

> **Important:** The IDs inside the live `thunderid-resources` ConfigMap in your cluster may differ from what the sample file shows. Before creating Application resources, look up the actual IDs from the running cluster:
>
> ```bash
> kubectl get cm thunderid-resources -o yaml | grep -E '^\s+id:|handle:|displayName:'
> ```
>
> Always use the IDs from the live ConfigMap — never hardcode IDs from the sample file.

---

### 3. Start ThunderID

Apply a `ThunderIDConfig` to create and manage a ThunderID deployment:

```bash
kubectl apply -f operator/thunderidConfig.yaml
```

The file at `operator/thunderidConfig.yaml` is a working example. Key field to keep:

```yaml
spec:
  resourcesConfigMap: thunderid-resources   # must match the ConfigMap name from step 2
```

Wait for the pod to be ready:

```bash
kubectl get pod -w
```

You should see a pod reach `1/1 Running`.

---

### 4. Run the operator

**Local (development):**

```bash
cd operator
make run
```

This runs the operator binary against whatever cluster `kubectl` is currently pointing to. Keep this terminal open — it must stay running to reconcile resources.

**In-cluster (production):**

```bash
cd operator
make deploy IMG=<your-registry>/thunderid-operator:latest
```

---

### 5. Register an application

Create an `Application` resource. The `instanceRef` must match the name of your `ThunderIDConfig`.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: Application
metadata:
  name: my-console-app
  namespace: default
spec:
  instanceRef: thunderid-prod
  name: My Console App
  ouId: "<ou-id-from-thunderid-resources>"        # look this up — see step 2
  authFlowHandle: console-app-flow
  themeId: "<theme-id-from-thunderid-resources>"   # look this up — see step 2
  isRegistrationFlowEnabled: true
  isRecoveryFlowEnabled: false
  allowedUserTypes:
    - Person
  assertion:
    validityPeriod: 3600
  loginConsent:
    validityPeriod: 0
  inboundAuthConfig: |
    - type: oauth2
      config:
        clientId: MY_APP_CLIENT
        redirectUris:
          - https://localhost:8090/callback
        grantTypes:
          - authorization_code
          - refresh_token
        responseTypes:
          - code
        tokenEndpointAuthMethod: none
        pkceRequired: true
        publicClient: true
```

```bash
kubectl apply -f my-app.yaml
```

Check status:

```bash
kubectl get applications
kubectl describe applications my-console-app
```

---

## Custom Resources

### ThunderIDConfig

The top-level resource. Creates and manages a `ThunderIDInstance`, `Deployment`, and (optionally) an `HPA`.

| Field | Required | Description |
|-------|----------|-------------|
| `spec.image` | Yes | ThunderID container image |
| `spec.domain` | Yes | Public domain for the ThunderID instance |
| `spec.resourcesConfigMap` | Yes* | ConfigMap containing base resources YAML. **Must be set** — omitting it causes APP-1018 on Application load |
| `spec.tlsSecret` | No | Kubernetes Secret name for TLS |
| `spec.resources` | No | CPU/memory requests and limits |
| `spec.autoScaling` | No | HPA settings (`minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`, `targetMemoryUtilizationPercentage`) |

---

### ThunderIDInstance

Managed automatically by `ThunderIDConfig`. You can also create one directly if you do not need the config-level abstraction.

| Field | Description |
|-------|-------------|
| `spec.image` | Container image |
| `spec.domain` | Public domain |
| `spec.resourcesConfigMap` | Base resources ConfigMap — **required for applications to load** |
| `spec.replicas` | Replica count (set to 0 when using HPA, which manages replicas) |
| `spec.resources` | Resource limits/requests |

---

### Application

Registers an OAuth2/OIDC application in ThunderID. The operator writes all applications for an instance into a single ConfigMap (`<instance-name>-applications`) which is mounted into the ThunderID pod. Changes trigger a rolling restart.

| Field | Description |
|-------|-------------|
| `spec.instanceRef` | Name of the `ThunderIDConfig` or `ThunderIDInstance` to register with |
| `spec.name` | Display name of the application |
| `spec.ouId` / `spec.ouHandle` | Organizational Unit — use IDs from the live `thunderid-resources` ConfigMap |
| `spec.authFlowHandle` / `spec.authFlowId` / `spec.authFlowRef` | Auth flow reference |
| `spec.themeId` / `spec.themeRef` | UI theme reference |
| `spec.logoUrl` | Logo (`emoji:👨‍💻` or a URL) |
| `spec.inboundAuthConfig` | Raw YAML block defining OAuth2 client configuration |
| `spec.isRegistrationFlowEnabled` | Enable user self-registration |
| `spec.isRecoveryFlowEnabled` | Enable password recovery |

**OAuth2 configuration notes:**
- Public client (browser app, SPA): use `publicClient: true`, `tokenEndpointAuthMethod: none`, `pkceRequired: true`
- Confidential client (server-side): use `publicClient: false`, `tokenEndpointAuthMethod: client_secret_basic`
- `client_credentials` grant type requires a confidential client (`publicClient: false`) with a real auth method — it cannot use `tokenEndpointAuthMethod: none`

---

### Flow

Registers a custom authentication or registration flow via the ThunderID live API (no pod restart required).

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: Flow
metadata:
  name: my-login-flow
  namespace: default
spec:
  instanceRef: thunderid-prod
  handle: my-login-flow
  name: My Login Flow
  flowType: AUTHENTICATION   # or REGISTRATION
  nodes: '[...]'             # ThunderID flow node JSON
```

---

### Theme

Registers a custom UI theme via the ThunderID live API (no pod restart required).

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: Theme
metadata:
  name: my-theme
  namespace: default
spec:
  instanceRef: thunderid-prod
  displayName: My Theme
  description: Dark purple theme
  theme: '{"defaultColorScheme":"dark","colorSchemes":{"dark":{"palette":{"primary":{"main":"#5567d5"}}}}}'
```

---

## Checking Status

### Pod health

```bash
kubectl get pod
```

During a rolling update you will see the old pod (`Running`) and a new pod simultaneously. The new pod cycles through:

```
0/1 Error  →  0/1 Running  →  0/1 Error  →  0/1 CrashLoopBackOff
```

This is **normal Kubernetes CrashLoopBackOff behaviour**. Kubernetes introduces an exponential backoff between restarts. If the application config is valid the pod will eventually reach `1/1 Running`.

### Application status

```bash
kubectl get applications
```

| Phase | Meaning |
|-------|---------|
| `Pending` | Config change detected; waiting for ThunderID pod to apply it |
| `Ready` | Application loaded successfully |
| `Error` | ThunderID rejected the config or the pod is crashing |

```bash
kubectl describe applications <name>
```

Look at the **Events** section. Event types:

| Reason | Meaning |
|--------|---------|
| `Synced` | Config change detected; ThunderID pod is restarting |
| `Ready` | Application loaded successfully |
| `ThunderIDError` | ThunderID rejected the config — APP error code and message are shown |
| `ThunderIDCrashed` | Pod is in CrashLoopBackOff; no structured error found in logs |

> **Tip:** `ThunderIDError` only appears after the pod reaches stable `CrashLoopBackOff`. Wait until `kubectl get pod` shows `CrashLoopBackOff` before running `kubectl describe applications`.

---

## Troubleshooting

### APP-1018 — Invalid request format

**Cause:** The Application references an OU or auth flow that does not exist in the ThunderID database. ThunderID uses ephemeral SQLite — on every pod restart it starts empty and the database must be seeded first.

**Fix:** Ensure `spec.resourcesConfigMap` is set on your `ThunderIDConfig` and points to a ConfigMap that contains the base OU and flows your applications reference.

```bash
# Check the ConfigMap is present
kubectl get cm thunderid-resources

# Verify the ThunderIDInstance has resourcesConfigMap set
kubectl get thunderidinstance <name> -o jsonpath='{.spec.resourcesConfigMap}'
```

### APP-1024 — Invalid OAuth configuration

**Cause:** Incompatible OAuth2 settings in `spec.inboundAuthConfig`. Common examples:

- `client_credentials` grant type with `tokenEndpointAuthMethod: none`
- `client_credentials` with `publicClient: true`

**Fix:** Read the `ThunderIDError` event message — it states the exact constraint violated.

### Phase: Error but no ThunderIDError event

**Cause:** The pod has not yet reached stable `CrashLoopBackOff`. The operator fetches logs only after the pod enters `CrashLoopBackOff` (typically within 30–40 seconds of the first crash).

**Fix:** Wait for `kubectl get pod` to show `CrashLoopBackOff`, then re-run `kubectl describe applications`.

### Pod never reaches Ready after applying ThunderIDConfig

**Cause:** The operator is not running, or it cannot reach the cluster.

**Fix:** Check that `make run` is still active in your terminal, or check the operator pod logs if deployed in-cluster:

```bash
kubectl logs -n system deployment/controller-manager
```

### HPA stuck, pod never scales up from 0

**Cause:** If a Deployment was created with `replicas: 0`, the HPA treats it as intentionally stopped and will not scale it up.

**Fix:**

```bash
kubectl delete deployment <instance-name>
# The operator recreates it correctly
```

---

## Development

```bash
cd operator

# Run tests
make test

# Build the operator binary
make build

# Generate CRD manifests after changing API types in api/v1alpha1/
make manifests

# Regenerate DeepCopy methods after changing API types
make generate

# Install CRDs into the cluster
make install

# Run locally against the current kubeconfig
make run

# Build and push the container image
make docker-build docker-push IMG=<registry>/thunderid-operator:<tag>

# Deploy to cluster
make deploy IMG=<registry>/thunderid-operator:<tag>

# Remove CRDs from the cluster
make uninstall

# Remove operator deployment
make undeploy
```

After changing any type in `api/v1alpha1/`, always run `make manifests generate` before running or deploying.

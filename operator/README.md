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
  - [OrganizationUnit](#organizationunit)
  - [ResourceServer](#resourceserver)
  - [Role](#role)
  - [UserType](#usertype)
  - [User](#user)
  - [Group](#group)
- [Checking Status](#checking-status)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

---

## Prerequisites

| Tool | Notes |
|------|-------|
| Go 1.21+ | Only needed for local development |
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

OrganizationUnit CRD  ──writes──►  <instance>-organizationunits ConfigMap
ResourceServer CRD    ──writes──►  <instance>-resourceservers ConfigMap
Theme CRD             ──writes──►  <instance>-themes ConfigMap
Flow CRD              ──writes──►  <instance>-flows ConfigMap
UserType CRD          ──writes──►  <instance>-usertypes ConfigMap
User CRD              ──writes──►  <instance>-users ConfigMap
Group CRD             ──writes──►  <instance>-groups ConfigMap
Role CRD              ──writes──►  <instance>-roles ConfigMap
Application CRD       ──writes──►  <instance>-applications ConfigMap
                                    └── all mounted into ThunderID pod at startup
```

**Key facts:**
- Every `ThunderIDInstance` must pick a persistent backend: either `spec.dataVolumeSize` (PVC-backed SQLite — `repository/database` persists, `setup.sh` runs once on the pod's first-ever start and is skipped on every restart after that) or `spec.database.type: postgresql` with `spec.database.secretRef` (external PostgreSQL — see [Database](#database) below; every restart re-syncs `resources.yaml` into the database via `start.sh --bootstrap-and-serve`, and `spec.dataVolumeSize` is ignored since there's no local state to persist). Setting neither is rejected — ephemeral, wipe-on-every-restart SQLite is not a supported configuration.
- `spec.resourcesConfigMap` must point to a ConfigMap that seeds the base OU, flows, and themes **before** any Application resources are loaded. Without this, Application loading will fail with `APP-1018`.
- All ten resource kinds are loaded at **pod startup** via mounted ConfigMaps, in dependency order — organization units first (everything else can reference `ouId`), resource servers and user types early (roles/users reference them), users before groups (groups reference users by id), groups and resource servers before roles (roles reference both by id), applications last (they can reference themes, flows, and organization units). A config change triggers a rolling restart; it is not applied live. Each CRD's controller reports `Pending` → `Ready`/`Error` on its `status.phase` while the rollout is verified, the same way Application does.

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
groups.apps.thunderid.io
organizationunits.apps.thunderid.io
resourceservers.apps.thunderid.io
roles.apps.thunderid.io
themes.apps.thunderid.io
thunderidconfigs.apps.thunderid.io
thunderidinstances.apps.thunderid.io
users.apps.thunderid.io
usertypes.apps.thunderid.io
```

---

### 2. Apply base resources

ThunderID's database starts empty on every fresh pod start. The base resources ConfigMap seeds it with the default OU, auth flows, and themes that your applications reference.

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

### ThunderIDInstance

The top-level resource. Creates and manages a `Deployment`, `Service`, `Ingress`, and (optionally) an `HPA`.

| Field | Required | Description |
|-------|----------|-------------|
| `spec.image` | Yes | ThunderID container image |
| `spec.domain` | Yes | Public domain for the ThunderID instance |
| `spec.resourcesConfigMap` | Yes* | ConfigMap containing base resources YAML. **Must be set** — omitting it causes APP-1018 on Application load |
| `spec.database.type` | See note | `sqlite` (default) or `postgresql`. See [Database](#database) below. One of `spec.database.type: postgresql` or `spec.dataVolumeSize` **must** be set — a `ThunderIDInstance` with neither is rejected |
| `spec.database.secretRef` | No | Required when `spec.database.type` is `postgresql`: name of the Secret with connection credentials |
| `spec.tlsSecret` | No | Kubernetes Secret name for TLS |
| `spec.dataVolumeSize` | See note | e.g. `"1Gi"`. Provisions a ReadWriteOnce PVC for `repository/database` (the SQLite files) and makes `setup.sh` run only once — on every restart after the first, the entrypoint finds a marker file on that volume and skips straight to `start.sh`. Only meaningful with `replicas: 1` (the Deployment switches to a `Recreate` strategy since the PVC can't be mounted by two pods at once). Ignored when `spec.database.type` is `postgresql` |
| `spec.replicas` | No | Replica count (set to 0 when using HPA, which manages replicas) |
| `spec.resources` | No | CPU/memory requests and limits |
| `spec.autoScaling` | No | HPA settings (`minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`, `targetMemoryUtilizationPercentage`) |

#### Database

By default, `spec.database.type` is `sqlite` and no further configuration is needed. To use PostgreSQL instead, set `spec.database.type: postgresql` and point `spec.database.secretRef` at a Secret shaped like:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: thunderid-db
  namespace: default
stringData:
  host: "postgres.example.com"
  port: "5432"                          # optional, defaults to 5432
  username: "thunderid"
  password: "secret"
  sslmode: "require"                    # optional; require (default), verify-full, verify-ca, or disable
  configDatabase: "thunderid_config"    # optional, default shown
  runtimeDatabase: "thunderid_runtime"  # optional, default shown — shared by both the transient and
                                         # persistent runtime stores (they differ by table set, not database)
  userDatabase: "thunderid_users"       # optional, default shown
```

Everything except `password` is inlined directly into the generated `deployment.yaml`; `password` is mounted into the pod as a file and referenced with a `file://` URI, the same way ThunderID resolves `direct_auth_secret` and `crypto.key`. The operator does not create the databases or run migrations — point it at an empty PostgreSQL server and ThunderID applies its own schema on startup, the same way it seeds SQLite from `dbscripts/*/sqlite.sql` at image-build time.

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
| `spec.template` | ThunderID UI template for the app (e.g. `react`, `browser`) |
| `spec.allowedUserTypes` / `spec.allowedUserTypeIds` | User types allowed to log in — by name (must match an existing `UserType`'s `spec.name`) or by the `UserType` CR's UID |
| `spec.inboundAuthConfig` | Raw YAML block defining OAuth2 client configuration |
| `spec.isRegistrationFlowEnabled` | Enable user self-registration |
| `spec.isRecoveryFlowEnabled` | Enable password recovery |

**OAuth2 configuration notes:**
- Public client (browser app, SPA): use `publicClient: true`, `tokenEndpointAuthMethod: none`, `pkceRequired: true`
- Confidential client (server-side): use `publicClient: false`, `tokenEndpointAuthMethod: client_secret_basic`
- `client_credentials` grant type requires a confidential client (`publicClient: false`) with a real auth method — it cannot use `tokenEndpointAuthMethod: none`

---

### Flow

Registers a custom authentication or registration flow. Loaded via the `<instance>-flows` ConfigMap at pod startup, like Application.

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
  nodes: |
    - id: start
      type: START
      onSuccess: end
    - id: end
      type: END
```

`spec.nodes` is the flow's node graph as raw YAML (the same shape ThunderID exports/imports natively), pasted verbatim — not a JSON string.

---

### Theme

Registers a custom UI theme. Loaded via the `<instance>-themes` ConfigMap at pod startup, like Application.

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

### OrganizationUnit

Registers an organization unit (a container for users, roles, and applications). Loaded via the `<instance>-organizationunits` ConfigMap — first, before any other resource kind, since everything else can reference `ouId`.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: OrganizationUnit
metadata:
  name: default-ou
  namespace: default
spec:
  instanceRef: thunderid-prod
  handle: default
  name: Default
  description: Default organization unit
  logoUrl: "https://example.com/logo.png"
```

| Field | Description |
|-------|--------------|
| `spec.handle` / `spec.name` | Required. Handle is the machine-readable slug, name is the display name |
| `spec.parentId` / `spec.parentHandle` / `spec.parentRef` | Optional parent organization unit — literal ID, literal handle, or a reference to another `OrganizationUnit` CR by Kubernetes object name (priority: ID > handle > ref) |
| `spec.logoUrl` | Optional. Free text — quoted safely for values that may contain emoji |

---

### ResourceServer

Registers a resource server and the resources/actions it exposes, for use in Role permission grants. Loaded via the `<instance>-resourceservers` ConfigMap.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: ResourceServer
metadata:
  name: system
  namespace: default
spec:
  instanceRef: thunderid-prod
  name: System
  description: System resource server
  identifier: https://localhost:8090/mcp
  ouRef: default-ou
  delimiter: ":"
  resources:
    - name: System
      handle: system
      description: System resource
    - name: Organization Unit
      handle: ou
      description: Organization unit resource
      parent: system
      actions:
        - name: View
          handle: view
          description: Read-only access to organization units
```

| Field | Description |
|-------|--------------|
| `spec.name` / `spec.identifier` | Required. ThunderID rejects a resource server with no identifier, crash-looping the whole pod on next restart — the CRD now enforces this too |
| `spec.handle` / `spec.delimiter` | Optional. `delimiter` joins a resource and action handle into a permission string (e.g. `ou:view`) |
| `spec.ouId` / `spec.ouHandle` / `spec.ouRef` | Owning organization unit, same triad as Role/UserType. One of `ouId`/`ouHandle` must resolve to a real org unit, or the operator skips syncing this resource server rather than writing an incomplete entry |
| `spec.resources[].name` / `.handle` / `.description` | Required name/handle per resource, optional description |
| `spec.resources[].parent` | Optional — the `handle` of another entry in the same `resources` list (not a Kubernetes reference) |
| `spec.resources[].actions[]` | Optional list of `{name, handle, description}` actions available on that resource |

---

### Role

Registers a role and its permission/assignment grants. Loaded via the `<instance>-roles` ConfigMap.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: Role
metadata:
  name: administrator
  namespace: default
spec:
  instanceRef: thunderid-prod
  name: Administrator
  description: System administrator role with full permissions
  ouRef: default-ou   # or ouId / ouHandle for a literal reference
  permissions:
    - resourceServerId: "019f215e-1a55-72db-9743-ac25d839c72a"
      permissions:
        - system
  assignments:
    - id: "019f215e-1bc0-773b-a202-04fbcee09eeb"
      type: group
```

| Field | Description |
|-------|--------------|
| `spec.name` | Required |
| `spec.ouId` / `spec.ouHandle` / `spec.ouRef` | Owning organization unit — literal ID, literal handle, or a reference to an `OrganizationUnit` CR by name |
| `spec.permissions[].resourceServerId` / `.permissions` | The resource server this grant applies to, and which permission names are granted on it |
| `spec.assignments[].id` / `.type` | Who the role is assigned to (e.g. a group ID) and the assignment type |

---

### UserType

Registers a user type and its attribute schema. Loaded via the `<instance>-usertypes` ConfigMap.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: UserType
metadata:
  name: person
  namespace: default
spec:
  instanceRef: thunderid-prod
  category: user
  name: Person
  ouRef: default-ou
  systemAttributes:
    display: username
  schema: |
    {
      "username": {"type": "string", "displayName": "Username", "required": true, "unique": true},
      "email": {"type": "string", "displayName": "Email", "required": true, "unique": true},
      "password": {"type": "string", "displayName": "Password", "required": false, "credential": true}
    }
```

| Field | Description |
|-------|--------------|
| `spec.category` / `spec.name` | Required. Category is a free-form string (e.g. `user`) |
| `spec.ouId` / `spec.ouHandle` / `spec.ouRef` | Owning organization unit, same triad as Role |
| `spec.systemAttributes` | Small key/value map, e.g. which attribute is used as the display name |
| `spec.schema` | Required. The attribute schema as raw JSON — pasted verbatim, single or multi-line |

---

### User

Registers a user. Loaded via the `<instance>-users` ConfigMap.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: User
metadata:
  name: admin
  namespace: default
spec:
  instanceRef: thunderid-prod
  type: Person
  ouRef: default-ou
  attributes:
    email: admin@example.com
    given_name: Admin
    family_name: User
    name: Administrator
    phone_number: "+12345678920"
    picture: "https://example.com/avatar.jpg"
  credentialsSecretRef: admin-credentials
```

| Field | Description |
|-------|--------------|
| `spec.type` | Required. The **name** of a `UserType` (e.g. `Person`) — a literal string, not a Kubernetes reference, same convention as `Application.allowedUserTypes` |
| `spec.ouId` / `spec.ouHandle` / `spec.ouRef` | Owning organization unit, same triad as Role/UserType/ResourceServer |
| `spec.attributes` | Free-form key/value map — the keys should match the attributes defined in the referenced `UserType`'s `schema` |
| `spec.credentialsSecretRef` | Name of a Secret (same namespace) holding this user's credentials — see below |

#### Credentials (`spec.credentialsSecretRef`)

A user's credentials (and, if present, `username`) come from a Kubernetes Secret rather than plaintext CR fields, so they never sit in the CR spec or etcd unencrypted.

- **You supply the Secret** (`spec.credentialsSecretRef: admin-credentials`) — every key in the Secret's `data`/`stringData` is passed through as a credential type (e.g. `password`, or any other type name a UserType's `schema` marks `"credential": true`), **except** `username`, which is special-cased: it overrides `spec.attributes.username` instead, since ThunderID treats username as an identifying attribute, not a credential. If both are set, the Secret wins — you don't need `username` in `spec.attributes` at all once it's in the Secret.

  ```yaml
  apiVersion: v1
  kind: Secret
  metadata:
    name: admin-credentials
    namespace: default
  type: Opaque
  stringData:
    username: admin
    password: change-me
  ```

- **You omit it** — the operator auto-generates a Secret named `<user-name>-credentials` on first reconcile, with a random `username` (`user-<8 hex chars>`) and `password` (8 hex chars), owned by the User (deleted automatically when the User is). Like the auto-generated security Secret, it's created once and never regenerated on later reconciles, so the password doesn't rotate out from under you — see `scripts/reset-credentials.sh` below if you need a new one.

  To find and read the generated credentials:
  ```
  ./scripts/get-credentials.sh <name>
  ```
  which is shorthand for:
  ```
  kubectl get user <name> -o jsonpath='{.status.credentialsSecretRef}'
  kubectl get secret <name>-credentials -o go-template='{{.data.username|base64decode}} / {{.data.password|base64decode}}'
  ```
  The operator also emits a one-time `CredentialsGenerated` Event on the User with this command pre-filled — `kubectl describe user <name>` shortly after creation. Events expire (~1h, cluster-dependent), so the `status.credentialsSecretRef` + Secret are the durable way to find them later.

  To reset an auto-generated user's password (and username) to a fresh random pair:
  ```
  ./scripts/reset-credentials.sh <name>
  ```
  This only works when `spec.credentialsSecretRef` was left empty — it refuses to touch a Secret you supplied yourself, since the operator doesn't own it. For a user-supplied Secret, rotate it directly (`kubectl edit secret <your-secret-name>`); the operator picks up whatever's in it on its next reconcile. Either way, the ThunderID pod still needs to restart before a changed password actually works for login (see below).

Either way, ThunderID's declarative loader only reads resources at pod **startup** — changing a User's credentials Secret after the fact still requires the ThunderID pod to restart (triggered automatically by the ConfigMap-version annotation bump) before the new values take effect.

---

### Group

Registers a group and its member list. Loaded via the `<instance>-groups` ConfigMap.

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: Group
metadata:
  name: administrators
  namespace: default
spec:
  instanceRef: thunderid-prod
  name: Administrators
  description: System administrators group
  ouRef: default-ou
  members:
    - id: "<user-or-group-id>"
      type: user
```

| Field | Description |
|-------|--------------|
| `spec.name` | Required |
| `spec.ouId` / `spec.ouHandle` / `spec.ouRef` | Owning organization unit, same triad as Role/UserType/User |
| `spec.members[].id` / `.type` | Literal member ID (a `User` or `Group`'s ID) and type (e.g. `user`, `group`) — literal, not a Kubernetes reference, same convention as `Role.assignments` |

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

`kubectl get flows|themes|organizationunits|resourceservers|roles|usertypes|users|groups` and `kubectl describe <kind> <name>` report the same `Pending`/`Ready`/`Error` phases and `Synced`/`Ready`/`ThunderIDError`/`ThunderIDCrashed` events as Application, since every resource kind is reconciled the same way. `kubectl get thunderidinstance` additionally shows a `DEGRADED` count summarizing how many dependent resources (of any kind) currently have a sync failure, without needing to check each one individually.

---

## Troubleshooting

### APP-1018 — Invalid request format

**Cause:** The Application references an OU or auth flow that does not exist in the ThunderID database. With SQLite, the database starts empty on every pod restart and must be seeded first.

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

### Pod restarts are slow (SQLite mode)

**Cause:** `setup.sh` runs on every pod restart to reinitialise the SQLite database. This can only happen if `spec.dataVolumeSize` is unset — the operator rejects `ThunderIDInstance` specs with neither `spec.dataVolumeSize` nor `spec.database.type: postgresql` set, precisely to avoid this.

**Fix:** Set `spec.dataVolumeSize` (e.g. `"1Gi"`) on your `ThunderIDInstance`. This provisions a PVC for `repository/database` and gates `setup.sh` behind a marker file on it, so it only runs on the pod's first-ever start — every restart after that skips straight to `start.sh`. Only use this with `replicas: 1`. If you don't need SQLite at all, `spec.database.type: postgresql` avoids `setup.sh` entirely — see [Database](#database).

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

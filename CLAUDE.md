# ThunderID Operator — Claude Context

## What this is
A Kubernetes operator that deploys and configures **ThunderID** (a WSO2 IAM platform) via CRDs.
All source is in the `operator/` subdirectory.
Go module: `github.com/wso2/thunderid-operator`

## Architecture

### How it works
1. `ThunderIDInstance` CR deploys a ThunderID pod (Deployment + Service + Ingress + HPA).
2. Sub-controllers watch their own CRDs, build YAML files, write them into ConfigMaps, and annotate the ThunderIDInstance. The annotation change triggers a rolling restart so ThunderID picks up the new config.
3. ThunderID reads its resource definitions from mounted ConfigMaps at startup.

### The 12 controllers (all in `operator/internal/controller/`)
| Controller | CRD | ConfigMap written |
|---|---|---|
| `thunderidinstance_controller.go` | `ThunderIDInstance` | `<name>-config` |
| `theme_controller.go` | `Theme` | `<instance>-themes` |
| `flow_controller.go` | `Flow` | `<instance>-flows` |
| `application_controller.go` | `Application` | `<instance>-applications` |
| `organizationunit_controller.go` | `OrganizationUnit` | `<instance>-organizationunits` |
| `role_controller.go` | `Role` | `<instance>-roles` |
| `usertype_controller.go` | `UserType` | `<instance>-usertypes` |
| `user_controller.go` | `User` | `<instance>-users` |
| `resourceserver_controller.go` | `ResourceServer` | `<instance>-resourceservers` |
| `group_controller.go` | `Group` | `<instance>-groups` |
| `serverconfig_controller.go` | `ServerConfig` | `<instance>-serverconfigs` |
| `translation_controller.go` | `Translation` | `<instance>-translations` |

### Key patterns
- **Idempotency**: every sub-controller SHA-256 hashes its ConfigMap content and stores it as `thunderid.io/<resource>-version` annotation on ThunderIDInstance. No annotation change → no ConfigMap write → no restart.
- **Status subresource**: all CRDs have `// +kubebuilder:subresource:status`. Status writes go through `/status` endpoint and do NOT increment `generation`, preventing reconcile loops.
- **Health check**: after syncing, every sub-controller re-queues in 30s, checks ThunderID pod container status, and sets `status.phase` = Ready/Pending/Error.
- **Finalizers**: each sub-controller adds a finalizer on create and removes it on delete (after rebuilding ConfigMap without that resource).
- **`deterministicUUIDv7`** in `refindex.go`: converts Kubernetes UUIDs (v4) to UUIDv7 format that ThunderID requires.
- **`buildXxxYAML`**: each controller has a function that renders a ThunderID bootstrap YAML document for its resource.

### Cross-references (refindex.go)
`SetupReferenceIndexes` registers field indexes for handle-based lookups:
- `spec.authFlowHandle`, `spec.registrationFlowHandle`, `spec.recoveryFlowHandle` → Application
- `spec.themeHandle` → Application
- `spec.ouHandle` → Application, Role, UserType, User, ResourceServer, Group

## File layout
```
operator/
  api/v1alpha1/          # CRD Go types + zz_generated.deepcopy.go
  cmd/main.go            # Manager startup, wires all controllers
  cmd/validate/          # Standalone CRD validation tool
  config/crd/bases/      # CRD YAML (manually maintained — see Windows note)
  config/rbac/role.yaml  # RBAC ClusterRole
  internal/controller/   # All controllers + refindex.go
```

## CRD types (api/v1alpha1/)
Each type file has `Spec`, `Status`, the root object, and a `List` type.
All specs include `instanceRef string` (required) pointing to a `ThunderIDInstance` name.

| File | Key spec fields |
|---|---|
| `thunderidinstance_types.go` | `domain`, `image`, `replicas`, `resources`, `autoScaling`, `tlsSecret`, `resourcesConfigMap` |
| `application_type.go` | `name`, `ouId`, `url`, `logoUrl`, `authFlowId`, `registrationFlowId`, `isRegistrationFlowEnabled`, `isRecoveryFlowEnabled`, `isSignOutFlowEnabled`, `assertion`, `loginConsent`, `allowedUserTypes`, `inboundAuthConfig` |
| `flow_type.go` | `handle`, `name`, `flowType` (AUTHENTICATION\|REGISTRATION), `nodes` (raw JSON/YAML), `isReadOnly` |
| `theme_type.go` | `displayName`, `handle`, `description`, `theme` (raw JSON), `isReadOnly` |
| `organizationunit_type.go` | `handle`, `name`, `description`, `parentId`, `logoUrl`, `createdAt`, `updatedAt` |
| `role_type.go` | `name`, `description`, `ouId`, `permissions` (resourceServerId + []permissions), `assignments` (id + type) |
| `usertype_type.go` | `category`, `name`, `ouId`, `systemAttributes` (map), `schema` (raw JSON) |
| `user_type.go` | `type`, `ouId`, `attributes` (map[string]string) |
| `resourceserver_type.go` | `name`, `identifier`, `type`, `ouId`, `delimiter`, `resources` (nested: name, handle, description, parent, actions) |
| `group_type.go` | `name`, `description`, `ouId`, `members` (id + type) |
| `serverconfig_type.go` | `name` (cors\|session\|defaultResourceServer), `cors` (allowedOrigins), `session` (idle/absoluteTimeoutSeconds), `defaultResourceServer` (resourceServerId) |
| `translation_type.go` | `language` (BCP47), `translations` (map[namespace]map[key]string) |

## DeepCopy (zz_generated.deepcopy.go)
Manually maintained (controller-gen cannot run on Windows). Rules:
- Flat structs (only scalars): use `*out = *in` — no extra code.
- Struct with slice/map/pointer fields: add explicit copy loops.
- Adding simple `string`/`bool` fields to an existing spec: `*out = *in` already covers them, no change needed.
- Adding a new slice field: add `if in.Field != nil { *out = make([]T, len(*in)); copy(*out, *in) }`.

## Windows constraints
- **No `make`**: use the raw Go/kubectl commands instead.
- **controller-gen** in `operator/bin/` is a Linux ELF — cannot run on Windows. Generate CRD YAML manually.
- **envtest teardown** prints "not supported by windows" — expected, not a real error.
- Build: `cd operator && go build ./...`
- Run locally: `cd operator && go run ./cmd/main.go`
- Apply CRDs: `kubectl apply -f config/crd/bases/`

## ThunderIDInstance requirements
For the operator to work correctly:
- `spec.resourcesConfigMap` must be `thunderid-resources`
- Resource IDs must be valid UUIDv7 (use `deterministicUUIDv7` helper)
- The `inboundAuthConfig` field in Application is raw YAML/JSON passed through `writeYAMLBlock`

## Current branch: Application-CRD
Recent commits added: ThunderIDConfig CRD merge, Application CRD, Flow CRD, then all remaining sub-controllers and types (OU, Role, UserType, User, ResourceServer, Group, ServerConfig, Translation).

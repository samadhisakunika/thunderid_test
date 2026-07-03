# ThunderID Operator

A Kubernetes operator that manages ThunderID identity server instances and declarative resources (Themes, Flows, Applications) via CRDs.

## Prerequisites

- Go 1.21+
- `kubectl` connected to a running cluster
- `make`

## Setup

### 1. Install CRDs

```sh
make manifests
kubectl apply -f config/crd/bases/
```

### 2. Run the operator

```sh
make run
```

---

## Deploy a ThunderID Instance

Apply the instance with a base resources ConfigMap:

```sh
kubectl apply -f test-instance-resources.yaml
```

Or with inline resources YAML:

```sh
kubectl apply -f test-inline.yaml
```

Check the instance status:

```sh
kubectl get thunderidinstances
kubectl describe thunderidinstance test-resources
```

---

## Port-forward to ThunderID

Port-forward to the Service so the connection survives pod restarts:

```sh
kubectl port-forward service/test-resources 8090:8090
```

To auto-restart on disconnect (recommended during development):

```sh
while true; do kubectl port-forward service/test-resources 8090:8090; sleep 2; done
```

ThunderID console: https://localhost:8090

---

## Declarative Resources via CRDs

Resources are synced to ThunderID by applying CRs. The operator watches for changes, rebuilds the resources ConfigMap, and triggers a rolling restart of the ThunderID pod.

> **Order matters:** Apply Themes and Flows before Applications that reference them via `themeRef` / `authFlowRef`.

### Themes

```sh
kubectl apply -f test-theme.yaml
kubectl get themes
```

To remove a theme from ThunderID:

```sh
kubectl delete theme my-custom-theme
```

### Flows

```sh
kubectl apply -f test-flow.yaml
kubectl get flows
```

### Applications

```sh
kubectl apply -f test-application.yaml
kubectl get applications
```

---

## Application CR Reference

```yaml
apiVersion: apps.thunderid.io/v1alpha1
kind: Application
metadata:
  name: my-web-app
  namespace: default
spec:
  instanceRef: test-resources
  name: My Web App
  description: Test application
  url: https://myapp.example.com
  ouId: "019ef85a-0aa4-7736-9357-d25edba4db95"  # Organisation unit ID (required)

  # Auth flow — pick one:
  authFlowRef: my-custom-login       # Reference a Flow CRD by name
  # authFlowHandle: some-handle      # Use an existing ThunderID flow handle directly
  # authFlowId: <uuid>               # Use an existing ThunderID flow ID directly

  # Theme — pick one:
  themeRef: my-custom-theme          # Reference a Theme CRD by name
  # themeId: "019ef85a-0e5d-..."     # Use an existing ThunderID theme ID directly
```

---

## After Adding New Fields to a CRD Type

Regenerate and re-apply the CRD manifest:

```sh
make manifests
kubectl apply -f config/crd/bases/apps.thunderid.io_<resource>.yaml
```

---

## Useful Commands

```sh
# Watch all ThunderID-related resources
kubectl get thunderidinstances,themes,flows,applications

# Check operator logs
kubectl logs -n <operator-namespace> deployment/<operator-name> --follow

# Check ThunderID pod logs
kubectl logs -l app=test-resources --follow

# Check the merged resources.yaml sent to ThunderID
kubectl get configmap test-resources-config -o jsonpath='{.data.resources\.yaml}'

# Check individual resource ConfigMaps
kubectl get configmap test-resources-themes -o jsonpath='{.data.themes\.yaml}'
kubectl get configmap test-resources-flows -o jsonpath='{.data.flows\.yaml}'
kubectl get configmap test-resources-applications -o jsonpath='{.data.applications\.yaml}'
```

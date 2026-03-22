# crd-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning database bindings, registering NATS subscriptions, and managing the lifecycle of deployed WASM modules.

## Application CRD

`Application` is the primary resource. Each instance declares a single deployable WASM module and its runtime requirements.

```yaml
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: default
spec:
  module:
    image: registry.example.com/my-app@sha256:<digest>
  nats:
    topic: my-app.messages
  env:
    - name: LOG_LEVEL
      value: info
  sql:
    - name: orders
  keyValue:
    - name: sessions
```

### Fields

#### `spec.module`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `image` | string | yes | OCI image reference for the `.wasm` module. Use a digest-pinned reference (`@sha256:…`) for deterministic deployments. |

#### `spec.nats`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `topic` | string | yes | NATS subject the execution host subscribes to. Messages arriving on this subject invoke the module's `on-message` export. |

#### `spec.env`

An ordered list of environment variables injected into the module's runtime configuration. Each entry has:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Variable name. Must be unique within the list. |
| `value` | string | yes | Variable value. |

#### `spec.sql` (optional)

An opt-in list of SQL database bindings. Omit the field entirely to disable SQL access. Each entry has:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Logical database name. Exposed to the module via the `sql` host import using this name as the `db` argument. Must correspond to a provisioned database managed by the [db-operator](https://github.com/benjamin-wright/db-operator). |

#### `spec.keyValue` (optional)

An opt-in list of key-value store bindings. Omit the field entirely to disable KV access. Each entry has:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Logical store name. Exposed to the module via the `kv` host import using this name as the `store` argument. Must correspond to a provisioned KV store managed by the [db-operator](https://github.com/benjamin-wright/db-operator). |

## Operator Behaviour

On `Application` create or update, the operator:

1. Resolves the OCI digest for `spec.module.image` if a mutable tag is given, and writes the resolved digest to the status.
2. Ensures each declared `spec.sql` database exists (via [db-operator](https://github.com/benjamin-wright/db-operator) resources) and that credentials are provisioned.
3. Ensures each declared `spec.keyValue` store exists (via [db-operator](https://github.com/benjamin-wright/db-operator) resources) and that credentials are provisioned.
4. Creates or updates the NATS consumer configuration for `spec.nats.topic`.
5. Writes a config projection (env vars + binding references) that the execution host reads at invocation time.

On `Application` delete, the operator removes the NATS consumer and releases (but does not destroy) the database and KV bindings so data is not lost on accidental deletion.

## Status

| Field | Description |
|-------|-------------|
| `status.resolvedImage` | Fully qualified OCI reference with resolved digest. |
| `status.conditions` | Standard Kubernetes condition list (`Ready`, `DatabasesBound`, `StoresBound`). |

## TODO

1. Define the Group/Version/Kind registration and kubebuilder markers.
2. Specify the exact [db-operator](https://github.com/benjamin-wright/db-operator) resource kinds used to request SQL and KV instances.
3. Add HTTP route bindings (`spec.routes`) in a future pass.
4. Add scheduling bindings (`spec.schedules`) in a future pass.
5. Define RBAC rules required by the operator service account.

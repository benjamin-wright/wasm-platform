# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning database bindings, registering message subscriptions, and managing the lifecycle of deployed WASM modules.

## Application CRD

`Application` is the primary resource. Each instance declares a single deployable WASM module and its runtime requirements. Exactly one of `spec.topic` or `spec.http` must be set — they are mutually exclusive.

### Topic-only application (message-passing)

```yaml
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: default
spec:
  module: oci://registry.example.com/my-app@sha256:<digest>
  topic: my-app.messages
  env:
    LOG_LEVEL: info
  sql: orders
  keyValue: sessions
```

### HTTP application

```yaml
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: order-api
  namespace: default
spec:
  module: oci://registry.example.com/order-api@sha256:<digest>
  http:
    path: /api/orders
    methods:
      - GET
      - POST
  sql: orders
```

### Fields

#### `spec.module`

| Type | Required | Description |
|------|----------|-------------|
| string | yes | OCI URI for the `.wasm` module. Use a digest-pinned reference (`@sha256:…`) for deterministic deployments. Format: `oci://<registry>/<repository>@sha256:<digest>`. |

#### `spec.topic`

| Type | Required | Description |
|------|----------|-------------|
| string | one of `topic`/`http` | Message subject the execution host subscribes to. Messages arriving on this subject invoke the module's `on-message` export. Must be **unique cluster-wide** across all namespaces. Wildcard characters (`*` and `>`) are forbidden and rejected at admission time. Mutually exclusive with `spec.http`. |

**Internal prefix:** the operator adds a `fn.` prefix to the user-supplied topic before pushing it to execution hosts. A user who writes `spec.topic: my-app.messages` results in the NATS subject `fn.my-app.messages`. This is invisible to the module author and enforced transparently by the platform to prevent collisions with HTTP-triggered topics.

**Uniqueness rule:** NATS subscriptions are global, so two Applications in different namespaces claiming the same subject would silently compete for messages. The operator enforces cluster-wide uniqueness: the Application with the oldest `metadata.creationTimestamp` is the rightful owner of a given topic. If two Applications share the same timestamp, the one with the lexicographically lower `namespace/name` wins. A blocked Application receives `Ready: False` with reason `TopicConflict` and no side-effectful work is performed for it (no NATS consumer, no SQL provisioning, no config pushed to execution hosts). When the owning Application is deleted or changes its topic, blocked Applications are automatically re-evaluated without manual intervention.

#### `spec.env` (optional)

A string map of environment variables injected into the module's runtime configuration. Keys must be unique.

```yaml
env:
  LOG_LEVEL: info
  FEATURE_FLAG: enabled
```

#### `spec.sql` (optional)

| Type | Required | Description |
|------|----------|-------------|
| string | no | Logical database name. The wp-operator creates a dedicated database and user with this name inside the shared PostgreSQL instance, and provisions the necessary permissions. Connection credentials are passed to execution hosts via the gRPC `ConfigSync` service. Exposed to the module via the `sql` host import as the `db` argument. Omit to disable SQL access entirely. |

#### `spec.keyValue` (optional)

| Type | Required | Description |
|------|----------|-------------|
| string | no | Key prefix for the module's key-value namespace inside the shared Redis instance. The execution host prepends `<namespace>/<spec.keyValue>/` to every key it reads or writes on behalf of the module, preventing conflicts between applications. Applications in the same namespace that declare the same `spec.keyValue` intentionally share keys, supporting the FaaS pattern of composing independent functions. Omit to disable KV access entirely. |

#### `spec.http` (optional, mutually exclusive with `spec.topic`)

Marks this Application as an HTTP-triggered app served by the platform gateway. Exactly one of `spec.topic` or `spec.http` must be set — providing both or neither is rejected at admission time.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.http.path` | string | yes | URL path the gateway exposes (e.g. `/api/orders`). Must start with `/`. Must be unique cluster-wide; two Applications may not expose the same path. |
| `spec.http.methods` | []string | no | Allowed HTTP methods. If omitted, the gateway accepts all methods. Valid values: `GET`, `HEAD`, `POST`, `PUT`, `DELETE`, `PATCH`, `OPTIONS`. The gateway returns `405 Method Not Allowed` (with an `Allow` header) when a request uses an unlisted method. |

**Internal topic:** when `spec.http` is set, the operator auto-generates the NATS subject as `http.<namespace>.<name>`. This is invisible to the module author; the execution host dispatches the request to the module's `on-request` export using typed WIT records rather than raw NATS payloads. No user-visible `spec.topic` is needed or permitted.

## Operator Behaviour

On `Application` create or update, the operator:

1. Resolves the OCI digest for `spec.module` if a mutable tag is given, and writes the resolved digest to the status.
2. If `spec.sql` is set, creates a dedicated database and user with the given name inside the shared PostgreSQL instance (if they do not already exist), grants the appropriate permissions, and retrieves the connection credentials.
3. If `spec.keyValue` is set, validates the key prefix and records it for inclusion in the app config (no external provisioning required — isolation is enforced by the execution host at runtime).
4. Creates or updates the message consumer configuration for `spec.topic`.
5. Pushes an incremental config update (env vars + binding references + database credentials + resolved module reference) to all connected execution hosts via the gRPC `PushIncrementalUpdate` RPC so they can load the new module.

On `Application` delete, the operator:

1. Removes the NATS message consumer for `spec.topic`.
2. If `spec.sql` is set, decrements a Redis reference count for the named database. If the count reaches zero, sets a TTL on the counter key. When the TTL elapses, the operator drops the database and its user. This prevents accidental data loss on spurious deletes while still recovering storage over time.

## Config API

The operator exposes a gRPC `ConfigSync` service that execution hosts use to stay in sync. The canonical schema is at [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto).

### RPCs

- **`RequestFullConfig` (host → operator)** — on startup or when a desync is suspected, the execution host calls this RPC to receive the latest full configuration snapshot for all applications. The response includes the complete `FullConfig` (version, all `ApplicationConfig` entries, and timestamp).

- **`PushIncrementalUpdate` (bidirectional streaming)** — after an execution host connects, it calls this RPC and keeps the stream open. The operator streams `IncrementalUpdateRequest` messages (config deltas) to the host whenever applications are created, updated, or deleted. Each message carries a version identifier, a list of `AppUpdate` entries (add/modify or delete), and a timestamp. The host streams back an `IncrementalUpdateAck` after processing each delta; if the ack reports a failure, the host should close the stream and re-request the full configuration via `RequestFullConfig`.

## Generated Code

Go gRPC stubs are generated from the proto schema into `internal/grpc/configsync/` (files marked `DO NOT EDIT`). CRD deepcopy functions are generated into `api/v1alpha1/` (also `DO NOT EDIT`). To regenerate all generated code, run from `components/wp-operator/`:

```sh
make generate
```

This requires `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`, and `controller-gen` to be installed.

## Status Conditions

| Condition | Values | Description |
|-----------|--------|-------------|
| `Ready` | `True` / `False` | Set to `True` once the application config has been successfully pushed to all connected execution hosts. Set to `False` while provisioning is in progress, or when blocked by a `TopicConflict`. |
| `TopicConflict` | `True` (when present) | Set when another Application is the rightful owner of `spec.topic`. The operator performs no side-effectful work for this Application until the owning Application is deleted or changes its topic. Automatically cleared on successful reconciliation — no manual intervention required. |

| Status field | Description |
|-------------|-------------|
| `status.resolvedImage` | Fully qualified OCI reference with resolved digest. |

## TODO

1. Add HTTP route bindings (`spec.routes`) in a future pass.
2. Add scheduling bindings (`spec.schedules`) in a future pass.

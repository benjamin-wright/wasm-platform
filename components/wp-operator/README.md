# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning databases, registering message subscriptions, and pushing configuration to execution hosts and the gateway.

## Application CRD

Each `Application` declares a single deployable WASM module and its runtime requirements. Exactly one of `spec.topic` or `spec.http` must be set.

### Examples

```yaml
# Topic-only (message-passing)
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
spec:
  module: oci://registry.example.com/my-app@sha256:<digest>
  topic: my-app.messages
  env:
    LOG_LEVEL: info
  sql: orders
  keyValue: sessions
---
# HTTP-triggered
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: order-api
spec:
  module: oci://registry.example.com/order-api@sha256:<digest>
  http:
    path: /api/orders
    methods: [GET, POST]
  sql: orders
```

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.module` | string | yes | OCI reference for the `.wasm` module. Prefer digest-pinned (`@sha256:…`). |
| `spec.topic` | string | one of `topic`/`http` | NATS subject for `on-message` invocation. Must be unique cluster-wide. Wildcards (`*`, `>`) forbidden. Internally prefixed with `fn.` before pushing to execution hosts. |
| `spec.http.path` | string | with `http` | URL path the gateway exposes. Must start with `/`, unique cluster-wide. |
| `spec.http.methods` | []string | no | Allowed HTTP methods. Omit to accept all. Gateway returns `405` for unlisted methods. |
| `spec.env` | map[string]string | no | Environment variables injected into the module's runtime config. |
| `spec.sql` | string | no | Logical database name. The operator creates a dedicated PG database + user and passes credentials (database name, username, password) to execution hosts via ConfigSync. Exposed to the module as the `db` argument in the `sql` host import. |
| `spec.keyValue` | string | no | Key-prefix namespace in the shared Redis instance. Execution hosts prepend `<namespace>/<spec.keyValue>/` to all keys. Apps sharing a `spec.keyValue` within the same namespace intentionally share key-space. |

**Topic uniqueness:** the operator enforces cluster-wide uniqueness — the Application with the oldest `creationTimestamp` owns the topic (tiebreak: lexicographically lower `namespace/name`). A blocked Application receives `Ready: False` with reason `TopicConflict`. When the owner is deleted or changes topic, blocked Applications are automatically re-evaluated.

**Internal topics:** `spec.topic` apps get a `fn.` prefix; `spec.http` apps get an auto-generated `http.<namespace>.<name>` subject. Both are invisible to the module author.

## Operator Behaviour

**On create/update:**

1. Resolves the OCI digest for `spec.module` (writes to `status.resolvedImage`).
2. If `spec.sql` is set, creates the PG database and user if they don't exist.
3. If `spec.keyValue` is set, records the prefix for inclusion in the app config.
4. Pushes an incremental config update to all connected execution hosts via `PushIncrementalUpdate`.

**On delete:**

1. Pushes a delete config update to execution hosts.
2. If `spec.sql` is set, decrements a Redis reference count for the database. At zero, sets a TTL; when the TTL elapses, drops the database and user.

## Config API

gRPC `ConfigSync` service for execution hosts. Schema: [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto).

- **`RequestFullConfig`** — full snapshot on startup or desync.
- **`PushIncrementalUpdate`** — bidirectional stream for ongoing deltas. Host acks each delta; on failure, falls back to `RequestFullConfig`.

The operator also exposes a `GatewayRoutes` service on the same gRPC port for the gateway's route table sync.

## Generated Code

```sh
make generate   # from components/wp-operator/
```

Requires `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`, `controller-gen`.

Generates: gRPC stubs → `internal/grpc/configsync/`, CRD deepcopy → `api/v1alpha1/`. All marked `DO NOT EDIT`.

## Status

| Condition | Description |
|-----------|-------------|
| `Ready` | `True` when config is pushed to all hosts. `False` while provisioning or on `TopicConflict`. |
| `TopicConflict` | Set when another Application owns `spec.topic`. Cleared automatically on resolution. |

| Field | Description |
|-------|-------------|
| `status.resolvedImage` | Fully qualified OCI reference with resolved digest. |

## TODO

1. Add scheduling bindings (`spec.schedules`).

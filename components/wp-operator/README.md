# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning databases, registering message subscriptions, and pushing configuration to execution hosts and the gateway.

## Application CRD

Each `Application` declares one or more deployable WASM functions and their shared runtime requirements.  Functions are listed under `spec.functions`; each function has its own module reference and trigger (exactly one of `trigger.http` or `trigger.topic`).  Application-level fields (`spec.env`, `spec.sql`, `spec.keyValue`) are shared across all functions.

### Examples

```yaml
# Single message-triggered function
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
spec:
  env:
    LOG_LEVEL: info
  sql: orders
  keyValue: sessions
  functions:
    - name: handler
      module: oci://registry.example.com/my-app@sha256:<digest>
      trigger:
        topic: my-app.messages
---
# Single HTTP-triggered function
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: order-api
spec:
  sql: orders
  functions:
    - name: handler
      module: oci://registry.example.com/order-api@sha256:<digest>
      trigger:
        http:
          path: /api/orders
          methods: [GET, POST]
---
# Multi-function application
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: order-service
spec:
  sql: orders
  functions:
    - name: api
      module: oci://registry.example.com/order-api@sha256:<digest>
      trigger:
        http:
          path: /api/orders
          methods: [GET, POST]
    - name: processor
      module: oci://registry.example.com/order-processor@sha256:<digest>
      trigger:
        topic: orders.created
```

### Fields

**Application-level (shared across all functions):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.functions` | []FunctionSpec | yes (min 1) | List of functions in this application. |
| `spec.env` | map[string]string | no | Environment variables injected into all functions. |
| `spec.sql` | string | no | Logical database name. The operator creates a dedicated PG database + user and passes credentials to execution hosts via ConfigSync. |
| `spec.keyValue` | string | no | Key-prefix namespace in the shared Redis instance. Execution hosts prepend `<namespace>/<spec.keyValue>/` to all keys. |
| `spec.metrics` | []MetricDefinition | no | User-defined Prometheus metrics (max 50). Each metric has a `name`, `type` (`counter`/`gauge`), and optional `labels` (max 10). Names must follow `[a-zA-Z_:][a-zA-Z0-9_:]{0,63}$` and must not start with `__`. Labels must not include `app_name` or `app_namespace` (host-injected). |

**Per-function (`spec.functions[]`):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Function identifier, unique within the Application. |
| `module` | string | yes | OCI reference for the `.wasm` module. Prefer digest-pinned (`@sha256:…`). |
| `trigger.topic` | string | one of `topic`/`http` | NATS subject for `on-message` invocation. Must be unique cluster-wide. Wildcards (`*`, `>`) forbidden. Internally prefixed with `fn.` before pushing to execution hosts. |
| `trigger.http.path` | string | with `http` | URL path the gateway exposes. Must start with `/`, unique cluster-wide. |
| `trigger.http.methods` | []string | no | Allowed HTTP methods. Omit to accept all. Gateway returns `405` for unlisted methods. |

**Topic uniqueness:** the operator enforces cluster-wide uniqueness per topic — the Application with the oldest `creationTimestamp` owns the topic (tiebreak: lexicographically lower `namespace/name`). A blocked Application receives `Ready: False` with reason `TopicConflict`. When the owner is deleted or changes topic, blocked Applications are automatically re-evaluated.

**Metric name uniqueness:** the operator enforces cluster-wide uniqueness per metric name across all Applications — same ownership rule as topics (oldest `creationTimestamp` wins, tiebreak: lexicographically lower `namespace/name`). A blocked Application receives `Ready: False` with reason `MetricConflict`. When the owner is deleted or removes the conflicting metric name, blocked Applications are automatically re-evaluated.

**Internal NATS subjects:** `trigger.topic` functions get a `fn.` prefix; `trigger.http` functions get an auto-generated `http.<namespace>.<app-name>.<function-name>` subject. Both are invisible to the module author.

## Operator Behaviour

**On create/update:**

1. For each message-triggered function, checks cluster-wide topic uniqueness.
2. If `spec.sql` is set, creates the PG database and user if they don't exist.
3. If `spec.keyValue` is set, records the prefix for inclusion in the app config.
4. Pushes an incremental config update (with all functions) to all connected execution hosts via `PushIncrementalUpdate`.
5. For each HTTP-triggered function, pushes a route update to all connected gateways via `PushRouteUpdate`.

**On delete:**

1. Pushes a delete config update to execution hosts.
2. Pushes route delete updates for all HTTP-triggered functions to gateways.
3. If `spec.sql` is set, decrements a Redis reference count for the database. At zero, sets a TTL; when the TTL elapses, drops the database and user.

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
| `Ready` | `True` when config is pushed to all hosts. `False` while provisioning or on `TopicConflict` or `MetricConflict`. |
| `TopicConflict` | Set when another Application owns a topic claimed by one of this app's functions. Cleared automatically on resolution. |
| `MetricConflict` | Set when another Application owns a metric name claimed by this app. Cleared automatically on resolution. |

## TODO

1. Add scheduling bindings (`spec.schedules`).


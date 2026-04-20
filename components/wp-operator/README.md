# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning databases, registering message subscriptions, and pushing configuration to execution hosts and the gateway.

## Application CRD

Each `Application` declares one or more deployable WASM functions and their shared runtime requirements.  Functions are listed under `spec.functions`; each function has its own module reference and trigger (exactly one of `trigger.http` or `trigger.topic`).  Application-level fields (`spec.env`, `spec.sql`) are shared across all functions.  KV access is available to every application automatically, with no opt-in field required.

### Examples

```yaml
# Single message-triggered function with SQL access
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: my-app
spec:
  env:
    LOG_LEVEL: info
  sql: {}   # implicit 'app' user, ALL on all tables
  functions:
    - name: handler
      module: oci://registry.example.com/my-app@sha256:<digest>
      trigger:
        topic: my-app.messages
---
# Multi-user SQL: different functions bound to different users
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: order-service
spec:
  sql:
    users:
      - name: reader
        permissions:
          - tables: [orders]
            grant: [SELECT]
      - name: writer
        permissions:
          - tables: [orders]
            grant: [SELECT, INSERT, UPDATE, DELETE]
  functions:
    - name: api
      module: oci://registry.example.com/order-api@sha256:<digest>
      sqlUser: writer
      trigger:
        http:
          path: /api/orders
          methods: [GET, POST]
    - name: reporter
      module: oci://registry.example.com/order-reporter@sha256:<digest>
      sqlUser: reader
      trigger:
        topic: orders.report
    - name: notifier
      module: oci://registry.example.com/order-notifier@sha256:<digest>
      # no sqlUser — SQL calls fail at runtime; module has no DB access
      trigger:
        topic: orders.notify
```

### Fields

**Application-level (shared across all functions):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.functions` | []FunctionSpec | yes (min 1) | List of functions in this application. |
| `spec.env` | map[string]string | no | Environment variables injected into all functions. |
| `spec.sql` | SQLSpec | no | SQL database access configuration. Absent means no SQL access. Present as `{}` provisions a single implicit `app` user with ALL privileges on all tables, and every function is implicitly bound to it. Present with `users` provisions exactly those users; functions must opt in via `sqlUser`. |
| `spec.metrics` | []MetricDefinition | no | User-defined Prometheus metrics (max 50). Each metric has a `name`, `type` (`counter`/`gauge`), and optional `labels` (max 10). Names must follow `[a-zA-Z_:][a-zA-Z0-9_:]{0,63}$` and must not start with `__`. Labels must not include `app_name` or `app_namespace` (host-injected). |

**`spec.sql.users[]` (when explicit users are listed):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Logical user identifier. Referenced by function `sqlUser` fields. The name `migrations` is reserved. |
| `permissions[].tables` | []string | no | Tables to grant on. Absent means all tables. |
| `permissions[].grant` | []string | yes | PostgreSQL privileges: `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, or `ALL`. |

**Per-function (`spec.functions[]`):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Function identifier, unique within the Application. |
| `module` | string | yes | OCI reference for the `.wasm` module. Prefer digest-pinned (`@sha256:…`). |
| `trigger.topic` | string | one of `topic`/`http` | NATS subject for `on-message` invocation. Must be unique cluster-wide. Wildcards (`*`, `>`) forbidden. Internally prefixed with `fn.` before pushing to execution hosts. |
| `trigger.http.path` | string | with `http` | URL path the gateway exposes. Must start with `/`, unique cluster-wide. |
| `trigger.http.methods` | []string | no | Allowed HTTP methods. Omit to accept all. Gateway returns `405` for unlisted methods. |
| `sqlUser` | string | no | Name of the SQL user (from `spec.sql.users`) this function uses. Ignored when `spec.sql.users` is absent/empty (implicit `app` user). When `spec.sql.users` is non-empty, functions without this field have no SQL access. |

**Topic uniqueness:** the operator enforces cluster-wide uniqueness per topic — the Application with the oldest `creationTimestamp` owns the topic (tiebreak: lexicographically lower `namespace/name`). A blocked Application receives `Ready: False` with reason `TopicConflict`. When the owner is deleted or changes topic, blocked Applications are automatically re-evaluated.

**Metric name uniqueness:** the operator enforces cluster-wide uniqueness per metric name across all Applications — same ownership rule as topics (oldest `creationTimestamp` wins, tiebreak: lexicographically lower `namespace/name`). A blocked Application receives `Ready: False` with reason `MetricConflict`. When the owner is deleted or removes the conflicting metric name, blocked Applications are automatically re-evaluated.

**Internal NATS subjects:** `trigger.topic` functions get a `fn.` prefix; `trigger.http` functions get an auto-generated `http.<namespace>.<app-name>.<function-name>` subject. Both are invisible to the module author.

**CEL validation rules on `spec.sql`:**
- `migrations` is reserved in `spec.sql.users[*].name`.
- When `spec.sql.users` is non-empty, each function's `sqlUser` must name a defined user or be absent.

## PostgreSQL Identifier Derivation

The operator derives deterministic PostgreSQL identifiers from the Application's namespace and name. Both the operator and execution host use the same algorithm so the correct pool is looked up at invocation time.

**Algorithm** (hyphens replaced with underscores throughout):

| Identifier | Formula |
|---|---|
| Database name | `wasm_<namespace>__<app_name>` |
| PG username | `wasm_<namespace>__<app_name>__<user_name>` |

**Truncation:** if the result exceeds 63 characters, take the first 47 characters, append `_`, then the first 15 hex characters of the lowercase SHA-256 of the full pre-truncation string.

**Inputs with consecutive hyphens** (`--`) are rejected at reconcile time with `Ready: False, reason: InvalidIdentifier` — double hyphens would produce `____` after sanitisation, colliding with the `__` component separator.

The derived database name and per-user PG usernames are surfaced in `status.sqlDatabaseName` and `status.sqlUsernames` for observability.

## Operator Behaviour

**On create/update:**

1. For each message-triggered function, checks cluster-wide topic uniqueness.
2. If `spec.sql` is set:
   a. Validates that namespace and app name contain no consecutive hyphens (`--`).
   b. Checks that `Config.PostgresDatabaseName` is configured and the named `PostgresDatabase` CR exists.
   c. Creates one `PostgresCredential` CR per SQL user (including the implicit `app` user when `spec.sql.users` is absent). Each credential targets the derived PG username, derived database name, and declared privileges.
   d. Waits until all credentials reach `Ready` phase and their Secrets are available. Returns `RequeueAfter: 5s` while any credential or Secret is pending.
   e. Assembles per-user connection URLs from the db-operator Secrets (`PGUSER`, `PGPASSWORD`, `PGHOST`, `PGPORT`) and the derived database name.
3. Pushes an incremental config update (with all functions) to all connected execution hosts via `PushIncrementalUpdate`.
4. For each HTTP-triggered function, pushes a route update to all connected gateways via `PushRouteUpdate`.

**On delete:**

1. Pushes a delete config update to execution hosts.
2. Pushes route delete updates for all HTTP-triggered functions to gateways.
3. If `spec.sql` is set, deletes all associated `PostgresCredential` CRs. The db-operator cleans up the PG users; the database itself persists until the `PostgresDatabase` CR is removed.

## SQL Credential Lifecycle

For each SQL user (or the synthetic `app` user when `spec.sql: {}`):

- **`PostgresCredential` name:** `wasm-<namespace>-<app_name>-<user_name>-pg` (Kubernetes-name-safe; hash-truncated at 238 chars to leave room for suffixes).
- **Secret name:** `wasm-<namespace>-<app_name>-<user_name>-pg-creds` (created by db-operator).
- **Namespace:** the operator's own namespace (`POD_NAMESPACE`).
- **Privileges:** declared in `spec.sql.users[*].permissions`; defaults to `ALL` on all tables when permissions are absent.

The operator does not push an `ApplicationConfig` to execution hosts until all `PostgresCredential` CRs for the Application have reached `Ready` phase.

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
| `Ready` | `True` when config is pushed to all hosts. `False` while provisioning or on error. |
| `TopicConflict` | Set when another Application owns a topic claimed by one of this app's functions. Cleared automatically on resolution. |
| `MetricConflict` | Set when another Application owns a metric name claimed by this app. Cleared automatically on resolution. |
| `DatabaseConfigMissing` | Set when `spec.sql` is present but `PostgresDatabaseName` is not configured. |
| `DatabaseNotFound` | Set when the named `PostgresDatabase` CR does not exist. |
| `InvalidIdentifier` | Set when namespace or app name contains consecutive hyphens, preventing PG identifier derivation. |

**Status fields:**

| Field | Description |
|-------|-------------|
| `status.sqlDatabaseName` | Derived PostgreSQL database name. Populated when `spec.sql` is set. |
| `status.sqlUsernames` | Derived PostgreSQL usernames, one per provisioned user. Populated when `spec.sql` is set. |


# wp-operator

A Kubernetes operator that watches `Application` CRDs and reconciles platform resources — provisioning databases, registering message subscriptions, and pushing configuration to execution hosts and the gateway.

## Application CRD

Each `Application` declares one or more deployable WASM functions and their shared runtime requirements.  Functions are listed under `spec.functions`; each function has its own module reference and trigger (exactly one of `trigger.http` or `trigger.topic`).  Application-level fields (`spec.env`, `spec.sql`, `spec.kv`) are shared across all functions.

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
| `spec.sql.migrations` | MigrationsSpec | no | Database migrations configuration. When set, the operator creates a `PostgresMigrationSet` CR (managed by the db-operator) and withholds function activation until it reaches `Ready` phase. See [Application Authoring — Database Migrations](#database-migrations). |
| `spec.sql.migrations.artifact` | string | yes (when `migrations` set) | ORAS artifact reference to a tar+gzip of SQL migration files. Media type `application/vnd.db-operator.migrations.v1.tar+gzip`. Use an immutable tag or digest. |
| `spec.sql.migrations.targetRevision` | int64 | yes (when `migrations` set) | Numeric migration ID to converge the database to. Must match a revision present in the artifact. The db-operator runs all pending apply migrations up to and including this ID; if the database is currently at a higher revision, it runs rollbacks down to it. |
| `spec.kv` | bool | no | Key-value store access. When `true`, the operator provisions a cluster-wide `RedisDatabase` and a `RedisCredential` for the execution host if they do not already exist. When `false` or absent, no KV access is provisioned. |
| `spec.metrics` | []MetricDefinition | no | User-defined Prometheus metrics (max 50). Each metric has a `name`, `type` (`counter`/`gauge`), and optional `labels` (max 10). Names must follow `[a-zA-Z_:][a-zA-Z0-9_:]{0,63}$` and must not start with `__`. Labels must not include `app_name` or `app_namespace` (host-injected). |

**`spec.sql.users[]` (when explicit users are listed):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Logical user identifier. Referenced by function `sqlUser` fields. |
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

**Topic uniqueness:** enforced cluster-wide at admission by the `ValidatingWebhookConfiguration`. A `kubectl apply` that would create a topic conflict is rejected immediately with an error message naming the conflicting topic. The Application with the oldest `creationTimestamp` owns the topic (tiebreak: lexicographically lower `namespace/name`).

**Metric name uniqueness:** enforced cluster-wide at admission by the `ValidatingWebhookConfiguration`. A `kubectl apply` that would create a metric name conflict is rejected immediately with an error message naming the conflicting metric. Ownership follows the same rule as topics.

**Identifier constraint:** when `spec.sql` is set, the application name and namespace must not contain consecutive hyphens (`--`). This constraint is enforced at admission and also re-validated by the reconciler as defense-in-depth.

**Internal NATS subjects:** `trigger.topic` functions get a `fn.` prefix; `trigger.http` functions get an auto-generated `http.<namespace>.<app-name>.<function-name>` subject. Both are invisible to the module author.

**CEL validation rules on `spec.sql`:**
- When `spec.sql.users` is non-empty, each function's `sqlUser` must name a defined user or be absent.

## Application Authoring

### Database Migrations

Applications can include versioned SQL schema migrations. When `spec.sql.migrations` is set, the operator creates a `PostgresMigrationSet` CR for the Application; the db-operator runs the migrations and reports status. The wp-operator gates function activation on the migration set reaching `Ready`, ensuring the schema is correct before traffic is served.

**File naming convention**

Migration files are paired by ID. Both files must exist for every migration:

```
migrations/
  001-create-greetings-apply.sql
  001-create-greetings-rollback.sql
  002-add-active-column-apply.sql
  002-add-active-column-rollback.sql
```

> **Footgun — never edit an applied file.** The runner tracks a content hash of every file it has applied. Editing or deleting an applied file is a hard error; the runner will refuse to run until the file is restored. Only ever add new migration files.

For the full file-format contract (ID format, hash tracking, advisory lock behaviour) see the [db-operator migrations spec](../../../db-operator/cmd/db-migrations/spec.md).

**Packaging migrations as an ORAS artifact**

Migrations are packaged as a tar+gzip of the `migrations/` directory and pushed as an OCI artifact with media type `application/vnd.db-operator.migrations.v1.tar+gzip`:

```sh
tar czf sql-hello-migrations.tar.gz -C examples/sql-hello/migrations .
oras push registry.example.com/sql-hello-migrations:v1 \
  sql-hello-migrations.tar.gz:application/vnd.db-operator.migrations.v1.tar+gzip \
  --artifact-type application/vnd.db-operator.migrations.v1.tar+gzip
```

A worked example lives in [`examples/sql-hello/Tiltfile`](../../examples/sql-hello/Tiltfile) (the `sql-hello-migrations` `custom_build` block) and [`examples/sql-hello/migrations/`](../../examples/sql-hello/migrations/).

**Reference the artifact from the Application:**

```yaml
spec:
  sql:
    migrations:
      artifact: registry.example.com/sql-hello-migrations:v1
      targetRevision: 2
```

`targetRevision` is the migration ID to converge to. Bump it when adding new migration files; the db-operator will apply the pending migrations and update the `PostgresMigrationSet` status. The wp-operator watches that status and re-pushes config when the set reaches `Ready` again.

**Immutable tags required**

> **Warning:** `spec.sql.migrations.artifact` accepts any OCI reference, but mutable tags (`:latest`, branch tags) cause silent schema skew across replicas and mask changes from the operator. Always use an immutable tag or digest (`@sha256:…`). This is not enforced by CEL — it is a deployment discipline requirement.

**Failure path**

If a migration fails, the `PostgresMigrationSet` enters `Failed` phase. The wp-operator surfaces this as `Ready: False, reason: MigrationFailed` on the Application, with the failure message copied from the migration set's status conditions. There is no automatic retry; recovery requires fixing the SQL files, pushing a new immutable artifact tag, and updating `spec.sql.migrations.artifact` (or `targetRevision`) — both trigger a fresh migrations run via the same `PostgresMigrationSet` CR.

Rollback is not yet wired through the platform; if a schema rollback is needed, add forward-correcting SQL rather than editing existing migration files.

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

1. The admission webhook has already enforced topic uniqueness, metric name uniqueness, and identifier constraints before the reconciler runs. The reconciler re-validates the identifier constraint as defense-in-depth.
2. **NATS infrastructure (always):** ensures a `NatsCluster` CR (`wasm-platform-nats`) and two `NatsAccount` CRs (`wasm-platform-nats-execution-host` and `wasm-platform-nats-gateway`) exist in the operator namespace and are `Ready`. While any is pending, sets `Ready: False, reason: NatsProvisioningPending` and requeues every 5 s. Once ready, reads the execution-host NATS Secret and publishes the connection info to all connected execution hosts via configsync.
3. If `spec.sql` is set:
   a. Validates that namespace and app name contain no consecutive hyphens (`--`).
   b. Derives the PostgresDatabase CR name as `wasm-<namespace>-postgres`; creates the CR (using `databases.postgres.version` and `databases.postgres.storageSize` from Helm values) if not present. Returns `RequeueAfter: 5s` while the DB is provisioning.
   c. Creates one `PostgresCredential` CR per SQL user (including the implicit `app` user when `spec.sql.users` is absent). Each credential targets the derived PG username, derived database name, and declared privileges.
   d. Waits until all credentials reach `Ready` phase and their Secrets are available. Returns `RequeueAfter: 5s` while any credential or Secret is pending.
   e. If `spec.sql.migrations` is set: creates (or patches) a `PostgresMigrationSet` CR named `wasm-<namespace>-<app_name>-migrations`. Returns `RequeueAfter: 5s` while pending/running; surfaces failures as `Ready: False, reason: MigrationFailed`.
   f. Assembles per-user connection URLs from the db-operator Secrets and the derived database name.
4. If `spec.kv` is set: ensures a `RedisDatabase` CR (`wasm-platform-redis`) and `RedisCredential` (`wasm-platform-redis-execution-host`) exist in the operator namespace. Returns `RequeueAfter: 5s` while pending. Once ready, reads the Redis Secret and publishes the connection URL to all connected execution hosts via configsync.
5. Pushes an incremental config update (with all functions and current infra connection info) to all connected execution hosts via `PushIncrementalUpdate`.
6. For each HTTP-triggered function, pushes a route update to all connected gateways via `PushRouteUpdate`.

**On delete:**

1. Pushes a delete config update to execution hosts.
2. Pushes route delete updates for all HTTP-triggered functions to gateways.
3. If `spec.sql` is set, deletes all associated `PostgresCredential` CRs. The db-operator cleans up the PG users; the `PostgresDatabase` CR is also deleted (it is per-namespace; if another Application in the same namespace still uses SQL, the CR is preserved).
4. If `spec.kv` is set and no other Application in the cluster has `spec.kv`, deletes the `RedisCredential` and `RedisDatabase` CRs.

## Infrastructure CR Naming

All infrastructure CRs are created in the operator's own namespace (`POD_NAMESPACE`).

| Resource | CR name |
|---|---|
| `NatsCluster` | `wasm-platform-nats` |
| `NatsAccount` (execution host) | `wasm-platform-nats-execution-host` |
| `NatsAccount` (gateway) | `wasm-platform-nats-gateway` |
| `PostgresDatabase` | `wasm-<namespace>-postgres` (per app namespace) |
| `RedisDatabase` | `wasm-platform-redis` |
| `RedisCredential` (execution host) | `wasm-platform-redis-execution-host` |

All operator-managed CRs carry the label `app.kubernetes.io/managed-by: wp-operator`.

## SQL Credential Lifecycle

For each SQL user (or the synthetic `app` user when `spec.sql: {}`):

- **`PostgresCredential` name:** `wasm-<namespace>-<app_name>-<user_name>-pg` (Kubernetes-name-safe; hash-truncated at 238 chars to leave room for suffixes).
- **Secret name:** `wasm-<namespace>-<app_name>-<user_name>-pg-creds` (created by db-operator).
- **Namespace:** the operator's own namespace (`POD_NAMESPACE`).
- **Privileges:** declared in `spec.sql.users[*].permissions`; defaults to `ALL` on all tables when permissions are absent.

When `spec.sql.migrations` is set, the operator additionally creates a `PostgresMigrationSet` CR (one per Application) that the db-operator reconciles — see [Database Migrations](#database-migrations). The set is named `wasm-<namespace>-<app_name>-migrations` and is deleted alongside the user credentials on Application deletion. The migrations runner uses an internal db-operator-owned role; the wp-operator does not provision a separate `migrations` `PostgresCredential`.

The operator does not push an `ApplicationConfig` to execution hosts until all `PostgresCredential` CRs for the Application have reached `Ready` phase **and**, if `spec.sql.migrations` is set, the `PostgresMigrationSet` has reached `Ready` phase.

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

## Validating Webhook

wp-operator exposes a validating admission webhook on port 9443, registered as `wasm-platform-application-validator` in Kubernetes. TLS is provided by a cert-manager self-signed `ClusterIssuer` (`wasm-platform-selfsigned`) and a `Certificate` (`wp-operator-webhook-tls`) whose Secret is mounted into the operator container.

**Scope:** fires on `Application` create and update.

**Checks performed:**

| Check | Description |
|-------|-------------|
| `InvalidIdentifier` | When `spec.sql` is set, rejects names containing consecutive hyphens (`--`). |
| `TopicConflict` | Rejects any function whose `trigger.topic` is already owned by another Application. |
| `MetricConflict` | Rejects any metric name already owned by another Application. |

**Failure policy:** `Fail` — if the webhook is unavailable, `Application` create and update requests are blocked. Existing execution hosts continue serving the last-known configuration while the operator is down; in-flight traffic is unaffected.

## Status

| Condition | Description |
|-----------|-------------|
| `Ready` | `True` when config is pushed to all hosts. `False` while provisioning or on error. |
| `NatsProvisioningPending` | Set while `NatsCluster` or `NatsAccount` CRs are not yet `Ready`. |
| `PostgresProvisioningPending` | Set while the per-namespace `PostgresDatabase` CR is not yet `Ready`. |
| `RedisProvisioningPending` | Set while the `RedisDatabase` or `RedisCredential` CR is not yet `Ready` (only when `spec.kv` is set). |
| `InvalidIdentifier` | Set when namespace or app name contains consecutive hyphens, preventing PG identifier derivation. |
| `MigrationsRunning` | Set while the `PostgresMigrationSet` is in `Pending` or `Running` phase. |
| `MigrationFailed` | Set when the `PostgresMigrationSet` reports `Failed`. Message is copied from the set's status conditions. No automatic retry — update `spec.sql.migrations.artifact` or `targetRevision` to trigger a new run. |

**Status fields:**

| Field | Description |
|-------|-------------|
| `status.sqlDatabaseName` | Derived PostgreSQL database name. Populated when `spec.sql` is set. |
| `status.sqlUsernames` | Derived PostgreSQL usernames, one per provisioned user. Populated when `spec.sql` is set. |
